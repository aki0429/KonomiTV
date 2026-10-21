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

const maxRecordingWalkDepth = 64
const maxRecordingWalkDirectories = 65536

var errRecordingWalkLimit = errors.New("recording directory traversal limit exceeded")

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
		err = walkRecordingFolders(ctx, folder, s.Config.RecordedFolders, excluded, identities, func(path string) error {
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
		if err == nil && statErr == nil && os.SameFile(identities[root], after) && os.SameFile(info, after) && verifyRecordingDirectories(ctx, root, identities) {
			complete[root] = after
		} else {
			s.logger().Warn("Recording folder could not be verified; deletion reconciliation skipped.", "folder", folder, "error", err)
		}
	}
	return s.reconcileMissingRecordings(ctx, complete, identities)
}

// walkRecordingFolders は見つけた録画から逐次処理する。設定済み root の外へは辿らない。
// 既知ディレクトリの集合は上限付き。全録画ファイルの集合やソートは作らない。
func walkRecordingFolders(ctx context.Context, directory string, configured, excluded []string, identities map[string]os.FileInfo, visit func(string) error) error {
	roots := make([]string, 0, len(configured))
	for _, folder := range configured {
		if root, err := strictRecordingPath(folder); err == nil {
			roots = append(roots, root)
		}
	}
	if identities == nil {
		identities = map[string]os.FileInfo{}
	}
	return walkRecordingDirectory(ctx, directory, roots, excluded, map[string]bool{}, identities, 0, visit)
}

func strictRecordingPath(path string) (string, error) {
	resolved, err := resolveRecordingFilesystemPath(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func withinRecordingRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func withinRecordingRoots(path string, roots []string) bool {
	for _, root := range roots {
		if withinRecordingRoot(path, root) {
			return true
		}
	}
	return false
}

func verifyRecordingDirectories(ctx context.Context, root string, identities map[string]os.FileInfo) bool {
	for path, before := range identities {
		if ctx.Err() != nil {
			return false
		}
		if !withinRecordingRoot(path, root) {
			continue
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			return false
		}
	}
	return true
}

func walkRecordingDirectory(ctx context.Context, directory string, roots, excluded []string, seen map[string]bool, identities map[string]os.FileInfo, depth int, visit func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= maxRecordingWalkDepth {
		return errRecordingWalkLimit
	}
	canonical, err := strictRecordingPath(directory)
	if err != nil {
		return err
	}
	if !withinRecordingRoots(canonical, roots) || isExcluded(normalizePathForPrefixMatch(directory), excluded) || isExcluded(normalizePathForPrefixMatch(canonical), excluded) || seen[canonical] {
		return nil
	}
	if len(seen) >= maxRecordingWalkDirectories || (identities[canonical] == nil && len(identities) >= maxRecordingWalkDirectories) {
		return errRecordingWalkLimit
	}
	file, err := os.Open(canonical)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if before := identities[canonical]; before != nil && !os.SameFile(before, info) {
		return errors.New("recording directory identity changed")
	}
	identities[canonical] = info
	seen[canonical] = true
	for {
		entries, readErr := readRecordingDirectory(ctx, file)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.HasPrefix(entry.Name(), "._") {
				continue
			}
			path := filepath.Join(canonical, entry.Name())
			resolved, resolveErr := strictRecordingPath(path)
			if resolveErr != nil {
				if os.IsNotExist(resolveErr) && entry.Type()&os.ModeSymlink == 0 {
					continue
				}
				return resolveErr
			}
			if !withinRecordingRoots(resolved, roots) {
				continue
			}
			if isExcluded(normalizePathForPrefixMatch(path), excluded) || isExcluded(normalizePathForPrefixMatch(resolved), excluded) {
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil {
				// 消えた直後のファイルは次の照合へ。読取拒否等は root の削除照合を止める。
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if info.IsDir() {
				if err := walkRecordingDirectory(ctx, resolved, roots, excluded, seen, identities, depth+1, visit); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() && isScanTarget(path) {
				if err := visit(resolved); err != nil {
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

func (s *Service) reconcileMissingRecordings(ctx context.Context, complete map[string]os.FileInfo, snapshots ...map[string]os.FileInfo) error {
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
			// 親ディレクトリが消失/差し替えされた場合は、削除とストレージ切断を区別できない。
			// メタデータを残し、復旧または明示的な次回 scan を待つ。
			parentsValid := true
			for parent := filepath.Dir(path); withinRecordingRoot(parent, verifiedRoot); parent = filepath.Dir(parent) {
				current, parentErr := os.Stat(parent)
				if parentErr != nil {
					parentsValid = false
					break
				}
				for _, snapshot := range snapshots {
					if before := snapshot[parent]; before != nil && !os.SameFile(before, current) {
						parentsValid = false
					}
				}
				if parent == verifiedRoot {
					break
				}
			}
			if !parentsValid {
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
