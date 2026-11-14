package metadata

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// recordingObservation は変更検出に必要な stat 値と録画完了判定を保持する。
// 同じサイズでも更新日時が変わった場合と、書き込み終了後の安定化を別々に検出する。
type recordingObservation struct {
	size     int64
	modified time.Time
	stable   bool
}

type recordingCandidate struct {
	observation    recordingObservation
	priority       bool
	removed        bool
	witness        string      // 保存先が空や未接続でないことを、処理直前にも確認するための現存ファイル
	rootInfo       os.FileInfo // 空になった保存先では、削除イベントと同じディレクトリかも確認する
	removalVersion uint64      // 古い観測や worker が、それ以降の削除通知を消さないための世代
}

// RunRecordingMonitor は録画フォルダを継続監視し、新規・変更・録画完了・欠落を DB へ反映する。
// OS 通知と巡回を併用し、通知を受け取れない NAS 上でも変更の検出を続ける。
// フォルダごとに解析を一つへ制限し、過去録画の取り込み中も別 goroutine で変更を検出する。
// ctx の終了時には走行中の解析をキャンセルし、全監視 goroutine の終了を待つ。
func (s *Service) RunRecordingMonitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	plans := compactRecordingWatchPlans(s.recordingWatchPlans())
	if len(plans) == 0 {
		return
	}
	var workers sync.WaitGroup
	completionChecks := make([]chan struct{}, 0, len(plans))
	for _, plan := range plans {
		check := make(chan struct{}, 1)
		completionChecks = append(completionChecks, check)
		workers.Go(func() { s.monitorRecordedFolder(ctx, plan, check) })
	}
	// 別の手動スキャンが保留した移動も、通知の有無にかかわらず継続確認する。
	// 巡回間隔とは独立した一つの時計で、全保存先の録画完了を確認する。
	workers.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, check := range completionChecks {
					select {
					case check <- struct{}{}:
					default:
					}
				}
				s.retryPendingMoves(ctx)
			}
		}
	})
	workers.Wait()
}

// monitorRecordedFolder は一つの録画保存先に対する発見と解析のキューを管理する。
// 発見時点で直ちに worker へ通知するため、全件収集・ソートによる固定待ち時間を作らない。
func (s *Service) monitorRecordedFolder(ctx context.Context, plan recordingWatchPlan, completionChecks <-chan struct{}) {
	folder := plan.folder
	var mu sync.Mutex
	observed := map[string]recordingObservation{}
	pending := map[string]recordingCandidate{}
	aliases := map[string]string{}
	removed := map[string]uint64{}
	failed := map[string]bool{}
	changed := map[string]string{}
	rescanRequested := false
	var removalVersion uint64
	notify := make(chan struct{}, 1)
	changes := make(chan struct{}, 1)
	rootProbes := make(chan struct{}, 1)
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
	// 通知を使える保存先では、最後のファイルの削除も空のマウントと区別する。
	// 通知されない NAS などでは従来の巡回を続け、空・読み取り失敗の保存先は保持する。
	var watcher *fsnotify.Watcher
	var watchErr error
	if !plan.polling {
		watcher, watchErr = fsnotify.NewWatcher()
	}
	watched := map[string]bool{}
	watchInfo := map[string]os.FileInfo{}
	if watchErr != nil {
		s.logger().Warn("Failed to create recording folder watcher; using polling.", "folder", folder, "error", watchErr)
	} else if watcher != nil {
		var events sync.WaitGroup
		events.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event, open := <-watcher.Events:
					if !open {
						return
					}
					if !event.Has(fsnotify.Remove | fsnotify.Rename | fsnotify.Create | fsnotify.Write | fsnotify.Chmod) {
						continue
					}
					mu.Lock()
					changed[recordedPathKey(event.Name)] = event.Name
					if event.Has(fsnotify.Remove | fsnotify.Rename) {
						removalVersion++
						key := recordedPathKey(event.Name)
						prefix := key + string(filepath.Separator)
						for alias, path := range aliases {
							if alias == key || strings.HasPrefix(alias, prefix) {
								removed[path] = removalVersion
							}
						}
					}
					mu.Unlock()
					select {
					case changes <- struct{}{}:
					default:
					}
				case err, open := <-watcher.Errors:
					if !open {
						return
					}
					s.logger().Warn("Recording folder watcher reported an error; polling continues.", "folder", folder, "error", err)
					mu.Lock()
					rescanRequested = true
					mu.Unlock()
					select {
					case changes <- struct{}{}:
					default:
					}
				}
			}
		})
		defer events.Wait()
		defer watcher.Close()
	}
	var worker sync.WaitGroup
	worker.Go(func() {
		for {
			if ctx.Err() != nil {
				return
			}
			// 直近の録画と監視開始後に変わったファイルを優先し、過去分で新規登録を塞がない。
			mu.Lock()
			var name string
			var candidate recordingCandidate
			for path, item := range pending {
				if name == "" || item.priority {
					name, candidate = path, item
				}
				if item.priority {
					break
				}
			}
			delete(pending, name)
			mu.Unlock()
			if name == "" {
				select {
				case <-ctx.Done():
					return
				case <-notify:
					continue
				}
			}
			if candidate.removed {
				// 保存先全体が取り外された場合は、欠落した録画として扱わない。
				// 観測後に保存先が変化している可能性もあるため、worker 側でも確認する。
				root, err := os.Stat(folder)
				if err != nil || candidate.rootInfo == nil || !os.SameFile(root, candidate.rootInfo) {
					continue
				}
				if candidate.witness != "" {
					if info, err := os.Stat(candidate.witness); err != nil || !info.Mode().IsRegular() {
						continue
					}
				}
				if !root.IsDir() {
					continue
				}
				// 通知の後に同じ場所へ戻った実体を、一時的にも欠落として保存しない。
				if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
					mu.Lock()
					pending[name] = recordingCandidate{observation: candidate.observation, priority: true}
					mu.Unlock()
					continue
				} else if err != nil && !os.IsNotExist(err) {
					continue
				}
				rows, err := s.Store.FindVideoSummariesByPaths(ctx, []string{name})
				if err == nil {
					for _, row := range rows {
						if err = s.Store.handleMissingVideo(ctx, row, true); err != nil {
							break
						}
					}
				}
				if err != nil {
					if ctx.Err() == nil {
						s.logger().Error("Failed to synchronize removed recording.", "file_path", name, "error", err)
					}
					continue
				}
				// 同じ場所に戻ったファイルを観測済みのまま残すと、復帰を取りこぼす。
				if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
					mu.Lock()
					pending[name] = recordingCandidate{observation: candidate.observation, priority: true}
					mu.Unlock()
					continue
				}
				s.forgetRecordingFile(name)
				mu.Lock()
				// 待機中に再出現していれば、その後の観測・登録は消さない。
				if observed[name] == candidate.observation && removed[name] == candidate.removalVersion {
					delete(observed, name)
					delete(removed, name)
					for alias, path := range aliases {
						if path == name {
							delete(aliases, alias)
						}
					}
					if pending[name].removed {
						delete(pending, name)
					}
				}
				mu.Unlock()
				continue
			}
			// サイズが止まった録画を Recording のまま固定しないよう、完了時には追跡状態を外す。
			if candidate.observation.stable {
				s.forgetRecordingFile(name)
			}
			err := s.ProcessRecordedFile(ctx, name, ProcessOptions{MetadataOnly: true})
			if err != nil && ctx.Err() == nil {
				s.logger().Error("Failed to synchronize recorded file.", "file_path", name, "error", err)
				// 一時的な DB / IO エラーは次回観測で再試行する。後から観測した変更は消さない。
				mu.Lock()
				if observed[name] == candidate.observation {
					delete(observed, name)
					failed[name] = true
				}
				mu.Unlock()
			} else if err == nil {
				mu.Lock()
				delete(failed, name)
				mu.Unlock()
				// WalkDir の列挙済み stat は古いことがある。通知は巡回中には消さず、
				// 処理後の実ファイルを確認したときだけ、同じ世代の通知を解除する。
				mu.Lock()
				version := removed[name]
				mu.Unlock()
				if version > 0 {
					if info, statErr := os.Stat(name); statErr == nil && info.Mode().IsRegular() {
						mu.Lock()
						if removed[name] == version {
							delete(removed, name)
						}
						mu.Unlock()
					}
				}
			}
		}
	})
	defer worker.Wait()
	// 通知を使える保存先は対象パスだけを観測する。通知を使わない場合や追加に失敗した
	// ディレクトリがある場合だけ、設定された間隔で全体を巡回する。
	watchIncomplete := watcher == nil
	walkRoot := plan.canonical
	var lastObservedRoot os.FileInfo
	var watchedRootInfo os.FileInfo
	rootUnavailable := false
	refreshWatches := func() {
		if watcher == nil {
			return
		}
		watched = map[string]bool{}
		for _, name := range watcher.WatchList() {
			watched[name] = true
		}
		for name := range watchInfo {
			if !watched[name] {
				delete(watchInfo, name)
			}
		}
	}
	eligible := func(name string) bool {
		return !plan.excludes(name) && !isExcluded(normalizePathForPrefixMatch(name), excluded) &&
			!strings.HasPrefix(filepath.Base(name), "._")
	}
	observeFile := func(original, resolved string, info os.FileInfo, initial, force bool) {
		if !eligible(original) || !eligible(resolved) || !info.Mode().IsRegular() {
			return
		}
		now := s.Store.now()
		observation := recordingObservation{size: info.Size(), modified: info.ModTime(), stable: now.Sub(info.ModTime()) >= time.Duration(RecordingCompleteSeconds)*time.Second}
		s.mu.Lock()
		tracking := s.recordingFiles[resolved]
		if tracking != nil && tracking.FileSize != info.Size() {
			observation.stable = false
		}
		s.mu.Unlock()
		s.rememberRecordedAlias(original, resolved)
		mu.Lock()
		previous, exists := observed[resolved]
		if exists && !previous.stable && previous.size != observation.size {
			observation.stable = false
		}
		aliases[recordedPathKey(original)] = resolved
		aliases[recordedPathKey(resolved)] = resolved
		changed := force || !exists || previous != observation || removed[resolved] > 0
		if changed {
			observed[resolved] = observation
		}
		mu.Unlock()
		if !changed {
			return
		}
		// __handleFileChange と同じく状態は通知ごとに更新する。DB 上で Recording の
		// ファイルを再解析しない場合も、完了判定には最後に通知されたサイズを使う。
		s.updateRecordingObservation(resolved, info, now)
		if !initial || now.Sub(info.ModTime()) < 24*time.Hour {
			mu.Lock()
			pending[resolved] = recordingCandidate{observation: observation, priority: true}
			mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
		}
	}
	walkTree := func(root string, initial bool, seen map[string]bool) error {
		refreshWatches()
		return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return walkErr
			}
			if !eligible(name) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if watcher != nil {
					info, err := os.Stat(name)
					if err != nil {
						return err
					}
					_ = os.SameFile(info, info)
					// Windows でも置き換え前のディレクトリ ID を先に確定する。
					if watched[name] && watchInfo[name] != nil && !os.SameFile(watchInfo[name], info) {
						_ = watcher.Remove(name)
						watched[name] = false
					}
					if !watched[name] {
						if err := watcher.Add(name); err == nil {
							watched[name] = true
							watchInfo[name] = info
						} else {
							watchIncomplete = true
							s.logger().Warn("Failed to watch recording directory; polling continues.", "directory", name, "error", err)
						}
					}
				}
				return nil
			}
			if !isScanTarget(name) {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(name)
			if err != nil || !eligible(resolved) {
				return nil
			}
			info, err := entry.Info()
			if entry.Type()&fs.ModeSymlink != 0 {
				info, err = os.Stat(resolved)
			}
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if seen != nil {
				seen[resolved] = true
			}
			original := name
			if relative, err := filepath.Rel(walkRoot, name); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				original = filepath.Join(folder, relative)
			}
			observeFile(original, resolved, info, initial, false)
			return nil
		})
	}
	queueRemovals := func(seen map[string]bool) {
		rootInfo, err := os.Stat(folder)
		if err != nil || lastObservedRoot == nil || !os.SameFile(rootInfo, lastObservedRoot) {
			return
		}
		mu.Lock()
		snapshot := make(map[string]recordingObservation, len(observed))
		versions := make(map[string]uint64, len(removed))
		for name, observation := range observed {
			snapshot[name] = observation
		}
		for name, version := range removed {
			versions[name] = version
		}
		mu.Unlock()
		witness := ""
		if seen != nil {
			for name := range seen {
				witness = name
				break
			}
		} else {
			for name := range snapshot {
				if versions[name] == 0 {
					if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
						witness = name
						break
					}
				}
			}
		}
		mu.Lock()
		for name, observation := range snapshot {
			missing := seen != nil && !seen[name] || seen == nil && versions[name] > 0
			if !missing || (witness == "" && versions[name] == 0) {
				continue
			}
			if observed[name] != observation || removed[name] != versions[name] {
				continue
			}
			pending[name] = recordingCandidate{observation: observation, priority: true, removed: true, witness: witness, rootInfo: lastObservedRoot, removalVersion: versions[name]}
		}
		hasWork := len(pending) > 0
		mu.Unlock()
		if hasWork {
			select {
			case notify <- struct{}{}:
			default:
			}
		}
	}
	fullScan := func(initial bool) {
		rootInfo, rootErr := os.Stat(folder)
		rootUnavailable = rootErr != nil || !rootInfo.IsDir()
		if rootErr == nil {
			watchedRootInfo = rootInfo
		}
		if rootErr == nil {
			_ = os.SameFile(rootInfo, rootInfo)
		}
		walkRoot = ResolveRecordedPath(folder)
		seen := map[string]bool{}
		watchIncomplete = watcher == nil
		err := walkTree(walkRoot, initial, seen)
		if err != nil && ctx.Err() == nil {
			watchIncomplete = true
			s.logger().Warn("Failed to observe recorded folder; will retry.", "folder", folder, "error", err)
		}
		// 完全な観測だけで消失を確定する。空の保存先では削除通知も必要にする。
		if err == nil && rootErr == nil {
			if len(seen) > 0 || lastObservedRoot == nil {
				lastObservedRoot = rootInfo
			}
			queueRemovals(seen)
		}
	}
	// 共通の 5 秒時計を受けるが、全件列挙や重い解析には待たされないよう独立させる。
	// 観測済みの未完了ファイルとスキャンが追跡している録画だけを stat する。
	var completionWorker sync.WaitGroup
	completionWorker.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-completionChecks:
			}
			select {
			case rootProbes <- struct{}{}:
			default:
			}
			type target struct {
				size  int64
				retry bool
			}
			targets := map[string]target{}
			s.mu.Lock()
			for name, tracking := range s.recordingFiles {
				if (recordedWithin(name, plan.canonical) || recordedWithin(name, folder)) && eligible(name) {
					targets[name] = target{size: tracking.FileSize}
				}
			}
			s.mu.Unlock()
			mu.Lock()
			for name, observation := range observed {
				if !observation.stable {
					if _, known := targets[name]; !known {
						targets[name] = target{size: observation.size}
					}
				}
			}
			for name := range failed {
				targets[name] = target{retry: true}
			}
			mu.Unlock()
			for name, target := range targets {
				if ctx.Err() != nil {
					return
				}
				info, err := os.Stat(name)
				if os.IsNotExist(err) {
					s.forgetRecordingFile(name)
					continue
				}
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				if target.retry || (info.Size() == target.size && s.Store.now().Sub(info.ModTime()) >= time.Duration(RecordingCompleteSeconds)*time.Second) {
					observeFile(name, name, info, false, target.retry)
				}
			}
		}
	})
	defer completionWorker.Wait()
	s.logger().Info("Recorded folder monitoring started.", "folder", folder, "polling", plan.polling, "poll_interval", plan.pollInterval.String())
	fullScan(true)
	s.logger().Info("Initial recording folder observation completed.", "folder", folder)
	ticker := time.NewTicker(plan.pollInterval)
	defer ticker.Stop()
	// 取り外された通常監視のルートが再接続された場合は、短い stat で復帰を検出する。
	// 全件の巡回と同じ時計にはせず、正常時はルート一件だけを確認する。
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if plan.polling || watchIncomplete {
				fullScan(false)
			}
		case <-rootProbes:
			if !plan.polling && watcher != nil {
				info, err := os.Stat(folder)
				if err != nil || !info.IsDir() {
					rootUnavailable = true
					continue
				}
				if rootUnavailable || watchedRootInfo == nil || !os.SameFile(info, watchedRootInfo) {
					fullScan(false)
				}
			}
		case <-changes:
			// watchfiles の step と同じ 50ms 単位で連続する書き込み通知をまとめる。
			// TS の小さな追記一回ごとに stat / DB 照会を走らせない。
			debounce := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				debounce.Stop()
				return
			case <-debounce.C:
			}
			mu.Lock()
			paths := changed
			changed = map[string]string{}
			rescan := rescanRequested
			rescanRequested = false
			mu.Unlock()
			if rescan {
				fullScan(false)
				continue
			}
			for _, name := range paths {
				if !eligible(name) {
					continue
				}
				info, err := os.Stat(name)
				if err != nil {
					continue
				}
				if info.IsDir() {
					if err := walkTree(name, false, nil); err != nil && ctx.Err() == nil {
						watchIncomplete = true
						s.logger().Warn("Failed to observe changed recording directory.", "folder", name, "error", err)
					}
				} else if isScanTarget(name) {
					if resolved, err := filepath.EvalSymlinks(name); err == nil {
						observeFile(name, resolved, info, false, true)
					}
				}
			}
			queueRemovals(nil)
		}
	}
}

// updateRecordingObservation は本家 __handleFileChange の継続更新状態を反映する。
// 読み取り中の解析処理と競合しないよう、以前の構造体を変更せずコピーで入れ替える。
func (s *Service) updateRecordingObservation(name string, info os.FileInfo, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.recordingFiles[name]
	if previous == nil {
		return
	}
	next := *previous
	modified := info.ModTime().Round(time.Microsecond)
	if info.Size() != previous.FileSize {
		next.MTIMEContinuousStartAt = nil
	} else if modified.After(previous.LastModified) && previous.MTIMEContinuousStartAt == nil {
		next.MTIMEContinuousStartAt = &modified
	}
	next.LastModified = modified
	next.FileSize = info.Size()
	if now.Sub(previous.LastChecked) >= time.Duration(updateThrottleSeconds)*time.Second {
		next.LastChecked = now
	}
	s.recordingFiles[name] = &next
}
