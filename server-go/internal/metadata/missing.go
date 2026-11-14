package metadata

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

const MissingRetention = 7 * 24 * time.Hour

type recordedRoot struct {
	path, canonical               string
	complete                      bool
	found, original, newlyMissing int
}

func recordedWithin(path, root string) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// scanRecordedFolders は保存先全体の状態を確認してから、欠落の確定・期限削除を行う。
func (s *Service) scanRecordedFolders(ctx context.Context, existing map[string]VideoSummary) error {
	roots := []recordedRoot{}
	seenRoots := map[string]bool{}
	for _, folder := range s.Config.RecordedFolders {
		absolute, err := filepath.Abs(folder)
		if err != nil {
			return err
		}
		absolute = filepath.Clean(absolute)
		if !seenRoots[absolute] {
			seenRoots[absolute] = true
			roots = append(roots, recordedRoot{path: absolute, canonical: ResolveRecordedPath(absolute), complete: true})
		}
	}
	// 設定の保存先から外れた既存レコードも、実体があれば保持する。
	roots = append(roots, recordedRoot{complete: true})
	rootFor := func(path string) int {
		for i, root := range roots[:len(roots)-1] {
			if recordedWithin(path, root.path) || recordedWithin(path, root.canonical) {
				return i
			}
		}
		return len(roots) - 1
	}
	for path, row := range existing {
		if row.Status != "Missing" {
			roots[rootFor(path)].original++
		}
	}
	excluded := normalizeExcludePaths(s.Config.ExcludeScanPaths)
	processed := map[string]bool{}
	for i := range roots[:len(roots)-1] {
		root := &roots[i]
		if info, err := os.Stat(root.path); err != nil || !info.IsDir() {
			root.complete = false
			continue
		}
		err := filepath.WalkDir(root.canonical, func(path string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				root.complete = false
				return nil
			}
			if strings.HasPrefix(entry.Name(), "._") || isExcluded(normalizePathForPrefixMatch(path), excluded) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			canonical := ResolveRecordedPath(path)
			if isExcluded(normalizePathForPrefixMatch(canonical), excluded) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !isScanTarget(canonical) {
				return nil
			}
			info, err := os.Stat(canonical)
			if err != nil {
				root.complete = false
				return nil
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			root.found++
			if processed[canonical] {
				return nil
			}
			processed[canonical] = true
			updateBatchProgress(true, -1)
			originalPath := path
			if relative, err := filepath.Rel(root.canonical, path); err == nil {
				originalPath = filepath.Join(root.path, relative)
			}
			if err := s.ProcessRecordedFile(ctx, canonical, ProcessOptions{OriginalPath: originalPath, ExistingDBRecordedVideos: existing, MetadataOnly: true}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.logger().Error("Failed to process recorded file.", "file_path", path, "error", err)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			root.complete = false
			s.logger().Warn("Recorded folder scan was incomplete.", "folder", root.path, "error", err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	missing := map[int][]VideoSummary{}
	for canonical, row := range existing {
		if processed[canonical] {
			continue
		}
		info, err := os.Stat(canonical)
		if err == nil && info.Mode().IsRegular() {
			continue
		}
		index := rootFor(canonical)
		if err != nil && !os.IsNotExist(err) {
			roots[index].complete = false
			continue
		}
		missing[index] = append(missing[index], row)
		if row.Status != "Missing" {
			roots[index].newlyMissing++
		}
	}
	canDelete := true
	for i, root := range roots {
		if !root.complete || root.path != "" && root.found == 0 && len(missing[i]) > 0 || root.original > 0 && float64(root.newlyMissing)/float64(root.original) > 0.2 {
			canDelete = false
		}
	}
	for index, rows := range missing {
		root := roots[index]
		if !root.complete || root.path != "" && root.found == 0 {
			continue
		}
		for _, row := range rows {
			if err := s.Store.handleMissingVideo(ctx, row, canDelete); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.logger().Error("Failed to synchronize missing recording.", "file_path", row.FilePath, "error", err)
			}
		}
	}
	return nil
}

// handleMissingVideo は同じ ID・場所・状態のままの場合だけ更新し、移動や復帰した行を削除しない。
func (s *Store) handleMissingVideo(ctx context.Context, snapshot VideoSummary, canDelete bool) error {
	canonical := ResolveRecordedPath(snapshot.FilePath)
	unlock, err := acquireRecordedPath(ctx, canonical)
	if err != nil {
		return err
	}
	defer unlock()
	if info, err := os.Stat(canonical); err == nil && info.Mode().IsRegular() {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tx, err := s.writeDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 外部プロセスの DB 更新も待ってから、現在の状態を読む。
	if _, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET id=id WHERE 0`); err != nil {
		return err
	}
	var missing sql.NullString
	current, err := scanVideoSummary(func(dest ...any) error {
		return tx.QueryRowContext(ctx, `SELECT `+videoSummaryColumns+`,missing_since FROM recorded_videos WHERE id=?`, snapshot.ID).Scan(append(dest, &missing)...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameRecordedSnapshot(*current, snapshot) {
		return nil
	}
	if info, err := os.Stat(canonical); err == nil && info.Mode().IsRegular() {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	remove := false
	switch current.Status {
	case "Recorded":
		_, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET status='Missing',missing_since=?,updated_at=? WHERE id=?`, database.FormatDBTime(s.now()), database.FormatDBTime(s.now()), current.ID)
	case "Recording", "AnalysisFailed":
		remove = canDelete
	case "Missing":
		if canDelete && missing.Valid {
			since, parseErr := database.ParseDBTime(missing.String)
			if parseErr != nil {
				return parseErr
			}
			remove = !since.After(s.now().Add(-MissingRetention))
		}
	}
	if err != nil {
		return err
	}
	if remove {
		if _, err = tx.ExecContext(ctx, `DELETE FROM recorded_videos WHERE id=?`, current.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM recorded_programs WHERE id=? AND NOT EXISTS(SELECT 1 FROM recorded_videos WHERE recorded_program_id=?)`, current.RecordedProgramID, current.RecordedProgramID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sameRecordedSnapshot(current, snapshot VideoSummary) bool {
	return current.ID == snapshot.ID && current.RecordedProgramID == snapshot.RecordedProgramID && current.FilePath == snapshot.FilePath && current.Status == snapshot.Status && current.FileSize == snapshot.FileSize && current.FileModifiedAt.Equal(snapshot.FileModifiedAt) && current.FileCreatedAt.Equal(snapshot.FileCreatedAt)
}

// restoreUnchangedVideo は監視が移動・再解析した行を上書きせず、日時補正と Missing の解除を行う。
func (s *Store) restoreUnchangedVideo(ctx context.Context, snapshot VideoSummary, created, modified time.Time) (bool, error) {
	tx, err := s.writeDB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET id=id WHERE 0`); err != nil {
		return false, err
	}
	current, err := scanVideoSummary(tx.QueryRowContext(ctx, `SELECT `+videoSummaryColumns+` FROM recorded_videos WHERE id=?`, snapshot.ID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameRecordedSnapshot(*current, snapshot) {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET status='Recorded',missing_since=NULL,file_created_at=?,file_modified_at=?,updated_at=? WHERE id=?`, database.FormatDBTime(created), database.FormatDBTime(modified), database.FormatDBTime(s.now()), snapshot.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
