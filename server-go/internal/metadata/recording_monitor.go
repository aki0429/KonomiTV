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
		interval = 10 * time.Second
	}
	var workers sync.WaitGroup
	// 別の手動スキャンが保留した移動も、通知の有無にかかわらず継続確認する。
	workers.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.retryPendingMoves(ctx)
			}
		}
	})
	roots := map[string]bool{}
	for _, folder := range s.Config.RecordedFolders {
		folder = filepath.Clean(folder)
		if absolute, err := filepath.Abs(folder); err == nil {
			folder = absolute
		}
		rootKey := recordedPathKey(folder)
		if roots[rootKey] {
			continue
		}
		roots[rootKey] = true
		workers.Go(func() { s.monitorRecordedFolder(ctx, folder, interval) })
	}
	workers.Wait()
}

// monitorRecordedFolder は一つの録画保存先に対する発見と解析のキューを管理する。
// 発見時点で直ちに worker へ通知するため、全件収集・ソートによる固定待ち時間を作らない。
func (s *Service) monitorRecordedFolder(ctx context.Context, folder string, interval time.Duration) {
	var mu sync.Mutex
	observed := map[string]recordingObservation{}
	pending := map[string]recordingCandidate{}
	aliases := map[string]string{}
	removed := map[string]uint64{}
	var removalVersion uint64
	notify := make(chan struct{}, 1)
	changes := make(chan struct{}, 1)
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
	// 通知を使える保存先では、最後のファイルの削除も空のマウントと区別する。
	// 通知されない NAS などでは従来の巡回を続け、空・読み取り失敗の保存先は保持する。
	watcher, watchErr := fsnotify.NewWatcher()
	watched := map[string]bool{}
	watchInfo := map[string]os.FileInfo{}
	if watchErr != nil {
		s.logger().Warn("Failed to create recording folder watcher; using polling.", "folder", folder, "error", watchErr)
	} else {
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
					if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
						continue
					}
					mu.Lock()
					removalVersion++
					key := recordedPathKey(event.Name)
					prefix := key + string(filepath.Separator)
					for alias, path := range aliases {
						if alias == key || strings.HasPrefix(alias, prefix) {
							removed[path] = removalVersion
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
				}
				mu.Unlock()
			} else if err == nil {
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
	s.logger().Info("Recorded folder monitoring started.", "folder", folder, "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	firstPass := true
	var lastObservedRoot os.FileInfo
	for {
		now := s.Store.now()
		seen := map[string]bool{}
		rootInfo, rootErr := os.Stat(folder)
		if rootErr == nil {
			_ = os.SameFile(rootInfo, rootInfo)
		}
		if watcher != nil {
			// ディレクトリの削除で OS が外した watch は、再作成されたときに追加し直す。
			watched = map[string]bool{}
			for _, path := range watcher.WatchList() {
				watched[path] = true
			}
			for path := range watchInfo {
				if !watched[path] {
					delete(watchInfo, path)
				}
			}
		}
		err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return walkErr
			}
			// 除外はリンク解決前と解決後の双方へ適用する。録画自体の検査も既存の解析処理へ委譲する。
			if isExcluded(normalizePathForPrefixMatch(path), excluded) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if watcher != nil {
					info, err := os.Stat(path)
					if err != nil {
						return err
					}
					// Windows でも置き換え前のディレクトリ ID を先に確定させる。
					_ = os.SameFile(info, info)
					if watched[path] && watchInfo[path] != nil && !os.SameFile(watchInfo[path], info) {
						_ = watcher.Remove(path)
						watched[path] = false
					}
					if !watched[path] {
						if err := watcher.Add(path); err == nil {
							watched[path] = true
							watchInfo[path] = info
						} else {
							s.logger().Warn("Failed to watch recording directory; polling continues.", "directory", path, "error", err)
						}
					}
				}
				return nil
			}
			if !isScanTarget(path) {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil
			}
			if isExcluded(normalizePathForPrefixMatch(resolved), excluded) {
				return nil
			}
			info, err := entry.Info()
			if entry.Type()&fs.ModeSymlink != 0 {
				info, err = os.Stat(resolved)
			}
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			seen[resolved] = true
			s.rememberRecordedAlias(path, resolved)
			observation := recordingObservation{size: info.Size(), modified: info.ModTime(), stable: now.Sub(info.ModTime()) >= time.Duration(RecordingCompleteSeconds)*time.Second}
			mu.Lock()
			aliases[recordedPathKey(path)] = resolved
			previous, exists := observed[resolved]
			if !exists || previous != observation || removed[resolved] > 0 {
				observed[resolved] = observation
				// 古い録画の初回取り込みは同時起動する一括スキャンへ任せる。
				// 監視の初回観測でも解析すると、失敗・短尺ファイルなどを二度続けて読み直してしまう。
				// 初回以降の変更と直近の録画は、一括スキャンを待たず反映する。
				if !firstPass || now.Sub(info.ModTime()) < 24*time.Hour {
					pending[resolved] = recordingCandidate{observation: observation, priority: true}
					select {
					case notify <- struct{}{}:
					default:
					}
				}
			}
			mu.Unlock()
			return nil
		})
		if err != nil && ctx.Err() == nil {
			s.logger().Warn("Failed to observe recorded folder; will retry.", "folder", folder, "error", err)
		}
		// 完全に観測できた保存先で消失を検出する。空の場合は OS の削除通知も必要とする。
		// 一括スキャンを待たず一覧から隠すが、録画済みレコードは一週間保持する。
		if err == nil && rootErr == nil {
			if len(seen) > 0 {
				lastObservedRoot = rootInfo
			}
			var witness string
			for path := range seen {
				witness = path
				break
			}
			mu.Lock()
			for path, observation := range observed {
				if !seen[path] && (len(seen) > 0 || removed[path] > 0) {
					pending[path] = recordingCandidate{observation: observation, priority: true, removed: true, witness: witness, rootInfo: lastObservedRoot, removalVersion: removed[path]}
				}
			}
			if len(pending) > 0 {
				select {
				case notify <- struct{}{}:
				default:
				}
			}
			mu.Unlock()
		}
		firstPass = false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-changes:
		}
	}
}
