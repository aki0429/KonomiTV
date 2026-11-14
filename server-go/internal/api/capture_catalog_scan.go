package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

var errCaptureScanRunning = errors.New("capture batch scan is already running")

func captureImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png":
		return !strings.HasPrefix(filepath.Base(path), "._")
	}
	return false
}

func captureWithin(path, root string) bool {
	rel, err := filepath.Rel(capturePathKey(root), capturePathKey(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (c *captureCatalog) scanStatus() catalogTaskStatus {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status
}
func (c *captureCatalog) progress(processed bool, total int, registered bool) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if processed {
		c.status.Processed++
		if c.status.Processed > c.status.Total {
			c.status.Total = c.status.Processed
		}
	}
	if total >= 0 {
		c.status.Total = total
		c.status.Registered = registered
	}
}

type captureRootResult struct {
	root                                  string
	complete                              bool
	found, original, newlyMissing, unseen int
}

// runBatch は保存先の読み取り失敗や大量欠落時に期限削除を見送り、Missing を一週間保持する。
func (c *captureCatalog) runBatch(ctx context.Context) (bool, error) {
	if !c.scanMu.TryLock() {
		return false, errCaptureScanRunning
	}
	defer c.scanMu.Unlock()
	defer c.sectionsGeneration.Add(1)
	c.statusMu.Lock()
	c.status = catalogTaskStatus{Running: true}
	c.statusMu.Unlock()
	defer func() { c.statusMu.Lock(); c.status.Running = false; c.statusMu.Unlock() }()
	rows, err := c.db.QueryContext(ctx, `SELECT `+catalogFileColumns+` FROM capture_files ORDER BY id`)
	if err != nil {
		return false, err
	}
	snapshots := []catalogFile{}
	byPath := map[string]catalogFile{}
	for rows.Next() {
		row, err := scanCatalogFile(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return false, err
		}
		snapshots = append(snapshots, row)
		byPath[capturePathKey(row.Path)] = row
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	c.progress(false, len(snapshots), len(snapshots) > 0)
	started := c.clock()
	roots := []captureRootResult{}
	keys := map[string]bool{}
	for _, folder := range c.folders {
		root := normalizeCapturePath(folder)
		key := capturePathKey(root)
		if !keys[key] {
			keys[key] = true
			roots = append(roots, captureRootResult{root: root, complete: true})
		}
	}
	// 設定から外した保存場所も、実体がある間は保持する。
	roots = append(roots, captureRootResult{complete: true})
	rootIndex := func(path string) int {
		best := len(roots) - 1
		length := -1
		for i := 0; i < len(roots)-1; i++ {
			if captureWithin(path, roots[i].root) && len(roots[i].root) > length {
				best = i
				length = len(roots[i].root)
			}
		}
		return best
	}
	for _, file := range snapshots {
		if file.Status != "Missing" {
			roots[rootIndex(file.Path)].original++
		}
	}
	seen := map[string]bool{}
	for index := 0; index < len(roots)-1; index++ {
		root := &roots[index]
		if info, err := os.Stat(root.root); err != nil || !info.IsDir() {
			root.complete = false
			continue
		}
		err := filepath.WalkDir(root.root, func(path string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				root.complete = false
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), "._") {
					return filepath.SkipDir
				}
				return nil
			}
			if !captureImagePath(path) {
				return nil
			}
			path = normalizeCapturePath(path)
			key := capturePathKey(path)
			if seen[key] {
				return nil
			}
			seen[key] = true
			root.found++
			c.progress(true, -1, false)
			info, err := os.Stat(path)
			if err != nil {
				root.complete = false
				return nil
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			created, modified := catalogFileTimes(info)
			if old, exists := byPath[key]; exists && old.Size == info.Size() && old.Created == created && old.Modified == modified && old.Status != "Missing" {
				return nil
			}
			if _, err := c.register(ctx, path, nil, false); err != nil {
				root.complete = false
				c.logger.Warn("Failed to register capture.", "path", path, "error", err)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			root.complete = false
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	for _, file := range snapshots {
		if seen[capturePathKey(file.Path)] {
			continue
		}
		index := rootIndex(file.Path)
		if _, err := os.Stat(file.Path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			roots[index].complete = false
			continue
		}
		roots[index].unseen++
		if file.Status != "Missing" {
			roots[index].newlyMissing++
		}
	}
	canDelete := len(roots) > 1
	complete := true
	for _, root := range roots {
		if !root.complete {
			canDelete = false
			complete = false
		}
		if root.root != "" && root.found == 0 && root.unseen > 0 {
			canDelete = false
		}
		if root.original > 0 && float64(root.newlyMissing)/float64(root.original) > 0.2 {
			canDelete = false
		}
	}
	for _, file := range snapshots {
		if seen[capturePathKey(file.Path)] || !roots[rootIndex(file.Path)].complete {
			continue
		}
		lock := &c.locks[c.lockIndex(file.Path)]
		lock.Lock()
		err := func() error {
			if _, err := os.Stat(file.Path); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			tx, err := c.beginWrite(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			current, err := scanCatalogFile(tx.QueryRowContext(ctx, `SELECT `+catalogFileColumns+` FROM capture_files WHERE id=? AND file_path=?`, file.ID, file.Path).Scan)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if current.Size != file.Size || current.Created != file.Created || current.Modified != file.Modified {
				return nil
			}
			if !current.Missing.Valid {
				_, err = tx.ExecContext(ctx, `UPDATE capture_files SET status='Missing',missing_since=? WHERE id=?`, database.FormatDBTime(started), file.ID)
			} else if canDelete {
				missing, parseErr := database.ParseDBTime(current.Missing.String)
				if parseErr != nil {
					return parseErr
				}
				if !missing.After(started.Add(-7 * 24 * time.Hour)) {
					if _, err = tx.ExecContext(ctx, `DELETE FROM capture_files WHERE id=?`, file.ID); err == nil && current.CaptureID.Valid {
						_, err = tx.ExecContext(ctx, `DELETE FROM captures WHERE id=? AND NOT EXISTS(SELECT 1 FROM capture_files WHERE capture_id=captures.id)`, current.CaptureID.Int64)
					}
				}
			}
			if err != nil {
				return err
			}
			return tx.Commit()
		}()
		lock.Unlock()
		if err != nil {
			return false, err
		}
	}
	if complete && canDelete {
		if _, err := c.writeDB.ExecContext(ctx, `DELETE FROM captures WHERE detached_since<? AND NOT EXISTS(SELECT 1 FROM capture_files WHERE capture_id=captures.id)`, database.FormatDBTime(started.Add(-7*24*time.Hour))); err != nil {
			return false, err
		}
	}
	c.logger.Info("Capture batch scan complete.", "complete", complete, "files", len(seen))
	return complete, nil
}

// requireCompleteCaptureScan はメンテナンス API が未完了を成功と伝えないための共通処理。
func (c *captureCatalog) requireCompleteCaptureScan(ctx context.Context) error {
	complete, err := c.runBatch(ctx)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("some capture folders could not be scanned completely")
	}
	return nil
}
