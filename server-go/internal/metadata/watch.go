package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WatchRecordedFolders はディレクトリを逐次走査し、新規・変更・完了・削除を反映する。
// イベント通知を利用できない NAS/SMB でも働くよう polling を使う。全件パスの収集・ソートや
// 全件分の worker は作らず、各 pass は ReadDir(128) と DB keyset page(256) で処理する。
func (s *Service) WatchRecordedFolders(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	identities := map[string]os.FileInfo{}
	for ctx.Err() == nil {
		if err := s.watchPass(ctx, identities); err != nil && !errors.Is(err, context.Canceled) {
			s.logger().Error("Recording watch pass failed.", "error", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// watchPass は正常に走査し終えた root だけを削除照合の対象にする。
// mount 切断・権限エラー・停止中は録画情報を消さない。
func (s *Service) watchPass(ctx context.Context, identities map[string]os.FileInfo) error {
	complete := map[string]os.FileInfo{}
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
	for _, folder := range s.Config.RecordedFolders {
		if err := ctx.Err(); err != nil {
			return err
		}
		root := ResolveRecordedPath(folder)
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		if identities[root] == nil {
			identities[root] = info
		}
		err = walkRecordingDirectory(ctx, folder, excluded, map[string]bool{}, func(path string) error {
			if err := s.ProcessRecordedFile(ctx, path, ProcessOptions{OriginalPath: path, SkipIfBusy: true}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.logger().Error("Failed to process observed recording.", "file_path", path, "error", err)
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		after, statErr := os.Stat(root)
		if err == nil && statErr == nil && os.SameFile(identities[root], after) && os.SameFile(info, after) {
			complete[root] = after
		} else {
			s.logger().Warn("Recording folder could not be verified; deletion reconciliation skipped.", "folder", folder, "error", err)
		}
	}
	return s.reconcileMissingRecordings(ctx, complete)
}

// walkRecordingDirectory はディレクトリ単位の全件 allocation も避け、見つけたファイルから処理する。
// ancestors は symlink による循環だけを検出し、録画全件の集合は保持しない。
func walkRecordingDirectory(ctx context.Context, directory string, excluded []string, ancestors map[string]bool, visit func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical := ResolveRecordedPath(directory)
	if isExcluded(normalizePathForPrefixMatch(directory), excluded) || isExcluded(normalizePathForPrefixMatch(canonical), excluded) || ancestors[canonical] {
		return nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	ancestors[canonical] = true
	defer delete(ancestors, canonical)
	for {
		entries, readErr := file.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.HasPrefix(entry.Name(), "._") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			resolved := ResolveRecordedPath(path)
			if isExcluded(normalizePathForPrefixMatch(path), excluded) || isExcluded(normalizePathForPrefixMatch(resolved), excluded) {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				// 消えた直後のファイルは次の照合へ。読取拒否等は root の削除照合を止める。
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if info.IsDir() {
				if err := walkRecordingDirectory(ctx, path, excluded, ancestors, visit); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() && isScanTarget(path) {
				if err := visit(path); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (s *Service) reconcileMissingRecordings(ctx context.Context, complete map[string]os.FileInfo) error {
	if len(complete) == 0 {
		return nil
	}
	var afterID int64
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
	for {
		rows, err := s.Store.ListVideoSummariesPage(ctx, afterID, 256)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			afterID = row.ID
			if err := ctx.Err(); err != nil {
				return err
			}
			path := ResolveRecordedPath(row.FilePath)
			if isExcluded(normalizePathForPrefixMatch(row.FilePath), excluded) || isExcluded(normalizePathForPrefixMatch(path), excluded) {
				continue
			}
			verified := false
			var verifiedRoot string
			for root := range complete {
				relative, err := filepath.Rel(root, path)
				if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
					verified = true
					verifiedRoot = root
					break
				}
			}
			if !verified {
				continue
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				continue
			}
			lock := recordedPathLock(path)
			if !lock.TryLock() {
				continue // 起動時の解析を待たず、次の pass で削除を確認する。
			}
			rootInfo, rootErr := os.Stat(verifiedRoot)
			_, statErr := os.Stat(path)
			if rootErr == nil && os.SameFile(complete[verifiedRoot], rootInfo) && os.IsNotExist(statErr) {
				err = s.Store.DeleteRecordedProgram(ctx, row.RecordedProgramID)
				s.forgetRecordingFile(path)
			}
			lock.Unlock()
			if err != nil {
				return fmt.Errorf("delete missing recording metadata: %w", err)
			}
		}
	}
}
