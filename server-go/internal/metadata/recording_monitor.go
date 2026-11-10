package metadata

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// recordingObservation は変更検出に必要な stat 値と録画完了判定を保持する。
// 同じサイズでも更新日時が変わった場合と、書き込み終了後の安定化を別々に検出する。
type recordingObservation struct {
	size     int64
	modified time.Time
	stable   bool
}

type recordingCandidate struct {
	observation recordingObservation
	priority    bool
}

// RunRecordingMonitor は録画フォルダを継続監視し、新規・変更・録画完了を DB へ反映する。
// OS 固有の通知に依存しないため、Windows・Linux・NAS 上でも同じ動作になる。
// フォルダごとに解析を一つへ制限し、過去録画の取り込み中も別 goroutine で変更を検出する。
// ctx の終了時には走行中の解析をキャンセルし、全監視 goroutine の終了を待つ。
func (s *Service) RunRecordingMonitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	var workers sync.WaitGroup
	roots := map[string]bool{}
	for _, folder := range s.Config.RecordedFolders {
		folder = filepath.Clean(folder)
		if roots[folder] {
			continue
		}
		roots[folder] = true
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
	notify := make(chan struct{}, 1)
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
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
			// サイズが止まった録画を Recording のまま固定しないよう、完了時には追跡状態を外す。
			if candidate.observation.stable {
				s.forgetRecordingFile(name)
			}
			if err := s.ProcessRecordedFile(ctx, name, ProcessOptions{MetadataOnly: true}); err != nil && ctx.Err() == nil {
				s.logger().Error("Failed to synchronize recorded file.", "file_path", name, "error", err)
				// 一時的な DB / IO エラーは次回観測で再試行する。後から観測した変更は消さない。
				mu.Lock()
				if observed[name] == candidate.observation {
					delete(observed, name)
				}
				mu.Unlock()
			}
		}
	})
	defer worker.Wait()
	s.logger().Info("Recorded folder monitoring started.", "folder", folder, "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	firstPass := true
	for {
		now := s.Store.now()
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
			if entry.IsDir() || !isScanTarget(path) {
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
			observation := recordingObservation{size: info.Size(), modified: info.ModTime(), stable: now.Sub(info.ModTime()) >= time.Duration(RecordingCompleteSeconds)*time.Second}
			mu.Lock()
			previous, exists := observed[resolved]
			if !exists || previous != observation {
				observed[resolved] = observation
				pending[resolved] = recordingCandidate{observation: observation, priority: !firstPass || now.Sub(info.ModTime()) < 24*time.Hour}
				select {
				case notify <- struct{}{}:
				default:
				}
			}
			mu.Unlock()
			return nil
		})
		if err != nil && ctx.Err() == nil {
			s.logger().Warn("Failed to observe recorded folder; will retry.", "folder", folder, "error", err)
		}
		firstPass = false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
