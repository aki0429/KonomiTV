package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
)

type catalogTaskStatus struct {
	Running    bool `json:"is_running"`
	Processed  int  `json:"processed_count"`
	Total      int  `json:"total_count"`
	Registered bool `json:"has_registered_files"`
}

type captureCatalog struct {
	db, writeDB        *sql.DB
	logger             *slog.Logger
	folders            []string
	locks              [128]sync.Mutex
	scanMu             sync.Mutex
	statusMu           sync.Mutex
	status             catalogTaskStatus
	now                func() time.Time
	sectionsGeneration atomic.Uint64
	sectionsMu         sync.Mutex
	sectionsCache      map[string]catalogCachedSections
}

type catalogFile struct {
	ID                        int64
	CaptureID                 sql.NullInt64
	Path, Name                string
	Size                      int64
	Created, Modified, Status string
	Missing                   sql.NullString
}

const catalogFileColumns = `id,capture_id,file_path,file_name,file_size,CAST(file_created_at AS TEXT),CAST(file_modified_at AS TEXT),status,CAST(missing_since AS TEXT)`

func scanCatalogFile(scan func(...any) error) (catalogFile, error) {
	var row catalogFile
	err := scan(&row.ID, &row.CaptureID, &row.Path, &row.Name, &row.Size, &row.Created, &row.Modified, &row.Status, &row.Missing)
	return row, err
}

func normalizeCapturePath(path string) string {
	value, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(value)
}
func capturePathKey(path string) string {
	path = normalizeCapturePath(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func (c *captureCatalog) lockIndex(path string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(capturePathKey(path)))
	return int(h.Sum32() % uint32(len(c.locks)))
}
func (c *captureCatalog) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func catalogFileTimes(info os.FileInfo) (string, string) {
	return database.FormatDBTime(metadata.FileCreationTime(info).Round(time.Microsecond)), database.FormatDBTime(info.ModTime().Round(time.Microsecond))
}

// beginWrite は読み取り前に書き込みロックを取り、別プロセスとのロック昇格競合を避ける。
func (c *captureCatalog) beginWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := c.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE capture_files SET id=id WHERE 0`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// register は保存場所のロックを取り、登録・監視・アップロードの重複を防ぐ。
func (c *captureCatalog) register(ctx context.Context, path string, data []byte, force bool) (int64, error) {
	path = normalizeCapturePath(path)
	lock := &c.locks[c.lockIndex(path)]
	lock.Lock()
	defer lock.Unlock()
	return c.registerLocked(ctx, path, data, force)
}

func (c *captureCatalog) registerLocked(ctx context.Context, path string, data []byte, force bool) (int64, error) {
	before, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !before.Mode().IsRegular() {
		return 0, fmt.Errorf("capture path is not a regular file")
	}
	created, modified := catalogFileTimes(before)
	existing, err := scanCatalogFile(c.db.QueryRowContext(ctx, `SELECT `+catalogFileColumns+` FROM capture_files WHERE file_path=?`, path).Scan)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && !force && existing.Size == before.Size() && existing.Created == created && existing.Modified == modified {
		if existing.Status == "Missing" {
			status := "Invalid"
			if existing.CaptureID.Valid {
				status = "Available"
			}
			if _, err := c.writeDB.ExecContext(ctx, `UPDATE capture_files SET status=?,missing_since=NULL WHERE id=?`, status, existing.ID); err != nil {
				return 0, err
			}
			c.sectionsGeneration.Add(1)
		}
		return existing.CaptureID.Int64, nil
	}
	defer c.sectionsGeneration.Add(1)
	if data == nil {
		data, err = os.ReadFile(path)
		if err != nil {
			return 0, err
		}
	}
	info, imageErr := readCatalogImage(data, filepath.Base(path), before.ModTime().Round(time.Microsecond))
	after, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return 0, fmt.Errorf("capture changed while being read")
	}
	tx, err := c.beginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := database.FormatDBTime(c.clock())
	var captureID any
	id := int64(0)
	if imageErr == nil {
		err = tx.QueryRowContext(ctx, `SELECT id FROM captures WHERE file_hash=?`, info.Hash[:]).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			var programID, broadcast, position, caption any
			captionComposited, commentComposited := false, false
			if meta := info.Metadata; meta != nil {
				position, caption = meta.PlaybackPosition, meta.CaptionText
				captionComposited, commentComposited = meta.CaptionComposited, meta.CommentComposited
				if meta.NetworkID >= 0 && meta.ServiceID >= 0 && meta.EventID >= 0 {
					var end any
					if meta.EndTime != nil {
						end = database.FormatDBTime(*meta.EndTime)
					}
					_, err = tx.ExecContext(ctx, `INSERT INTO capture_programs(network_id,service_id,event_id,start_time,end_time,duration,title,description,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(network_id,service_id,event_id,start_time) DO NOTHING`, meta.NetworkID, meta.ServiceID, meta.EventID, database.FormatDBTime(meta.StartTime), end, meta.Duration, meta.Title, meta.Description, now, now)
					if err != nil {
						return 0, err
					}
					var pid int64
					if err = tx.QueryRowContext(ctx, `SELECT id FROM capture_programs WHERE network_id=? AND service_id=? AND event_id=? AND start_time=?`, meta.NetworkID, meta.ServiceID, meta.EventID, database.FormatDBTime(meta.StartTime)).Scan(&pid); err != nil {
						return 0, err
					}
					if meta.Duration != nil {
						if _, err = tx.ExecContext(ctx, `UPDATE capture_programs SET end_time=?,duration=?,updated_at=? WHERE id=? AND duration IS NULL`, end, meta.Duration, now, pid); err != nil {
							return 0, err
						}
					}
					value, err := captureBroadcastTime(meta.StartTime, meta.PlaybackPosition)
					if err != nil {
						return 0, err
					}
					programID, broadcast = pid, database.FormatDBTime(value)
				}
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO captures(file_hash,file_size,captured_at,broadcast_at,image_width,image_height,metadata_status,playback_position,caption_text,is_caption_composited,is_comment_composited,capture_program_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, info.Hash[:], len(data), database.FormatDBTime(info.CapturedAt), broadcast, info.Width, info.Height, info.MetadataStatus, position, caption, captionComposited, commentComposited, programID, now, now)
			if err != nil {
				return 0, err
			}
			id, err = result.LastInsertId()
			if err != nil {
				return 0, err
			}
		} else if err != nil {
			return 0, err
		}
		captureID = id
		if _, err = tx.ExecContext(ctx, `UPDATE captures SET detached_since=NULL WHERE id=? AND detached_since IS NOT NULL`, id); err != nil {
			return 0, err
		}
	}
	status := "Invalid"
	if id != 0 {
		status = "Available"
	}
	// 内容のハッシュまで一致する移動は、元の場所の ID を引き継ぐ。
	if existing.ID == 0 && id != 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id,file_path FROM capture_files WHERE capture_id=? AND file_name=? AND file_size=? AND file_modified_at=? ORDER BY id`, id, filepath.Base(path), before.Size(), modified)
		if err != nil {
			return 0, err
		}
		type location struct {
			id   int64
			path string
		}
		var candidates []location
		for rows.Next() {
			var value location
			if err = rows.Scan(&value.id, &value.path); err != nil {
				_ = rows.Close()
				return 0, err
			}
			candidates = append(candidates, value)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return 0, err
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate.path); os.IsNotExist(err) {
				if _, err := tx.ExecContext(ctx, `UPDATE capture_files SET file_path=?,file_created_at=?,status='Available',missing_since=NULL,updated_at=? WHERE id=?`, path, created, now, candidate.id); err != nil {
					return 0, err
				}
				if err := tx.Commit(); err != nil {
					return 0, err
				}
				return id, nil
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO capture_files(capture_id,file_path,file_name,file_size,file_created_at,file_modified_at,status,missing_since,created_at,updated_at) VALUES(?,?,?,?,?,?,?,NULL,?,?) ON CONFLICT(file_path) DO UPDATE SET capture_id=excluded.capture_id,file_name=excluded.file_name,file_size=excluded.file_size,file_created_at=excluded.file_created_at,file_modified_at=excluded.file_modified_at,status=excluded.status,missing_since=NULL,updated_at=excluded.updated_at`, captureID, path, filepath.Base(path), before.Size(), created, modified, status, now, now)
	if err != nil {
		return 0, err
	}
	if existing.CaptureID.Valid && existing.CaptureID.Int64 != id {
		if _, err := tx.ExecContext(ctx, `UPDATE captures SET detached_since=? WHERE id=? AND NOT EXISTS(SELECT 1 FROM capture_files WHERE capture_id=captures.id)`, now, existing.CaptureID.Int64); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (c *captureCatalog) files(ctx context.Context, id int64) ([]catalogFile, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+catalogFileColumns+` FROM capture_files WHERE capture_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []catalogFile{}
	for rows.Next() {
		value, err := scanCatalogFile(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// image はハッシュの一致した内容だけを配信し、置き換わった保存場所をその場で再登録する。
func (c *captureCatalog) image(ctx context.Context, id int64, hash string) ([]byte, error) {
	var digest []byte
	if err := c.db.QueryRowContext(ctx, `SELECT file_hash FROM captures WHERE id=?`, id).Scan(&digest); err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", digest) != hash {
		return nil, sql.ErrNoRows
	}
	files, err := c.files(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.Status != "Available" {
			continue
		}
		lock := &c.locks[c.lockIndex(file.Path)]
		lock.Lock()
		data, err := os.ReadFile(file.Path)
		if err == nil {
			actual := sha256.Sum256(data)
			if fmt.Sprintf("%x", actual) == hash {
				lock.Unlock()
				return data, nil
			}
			_, err = c.registerLocked(ctx, file.Path, data, true)
			if err != nil {
				lock.Unlock()
				return nil, err
			}
		}
		lock.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, sql.ErrNoRows
}

// delete は全保存場所の内容を照合し、削除中に場所が変わっていない場合だけ DB も削除する。
func (c *captureCatalog) delete(ctx context.Context, id int64) (bool, error) {
	defer c.sectionsGeneration.Add(1)
	var digest []byte
	err := c.db.QueryRowContext(ctx, `SELECT file_hash FROM captures WHERE id=?`, id).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	files, err := c.files(ctx, id)
	if err != nil {
		return false, err
	}
	indices := map[int]bool{}
	for _, file := range files {
		indices[c.lockIndex(file.Path)] = true
	}
	ordered := []int{}
	for index := range indices {
		ordered = append(ordered, index)
	}
	sort.Ints(ordered)
	for _, index := range ordered {
		c.locks[index].Lock()
	}
	defer func() {
		for i := len(ordered) - 1; i >= 0; i-- {
			c.locks[ordered[i]].Unlock()
		}
	}()
	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		actual := sha256.Sum256(data)
		if string(actual[:]) != string(digest) {
			_, err = c.registerLocked(ctx, file.Path, data, true)
			return false, err
		}
		if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	tx, err := c.beginWrite(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,file_path FROM capture_files WHERE capture_id=? ORDER BY id`, id)
	if err != nil {
		return false, err
	}
	same, index := true, 0
	for rows.Next() {
		var fid int64
		var path string
		if err = rows.Scan(&fid, &path); err != nil {
			_ = rows.Close()
			return false, err
		}
		if index >= len(files) || files[index].ID != fid || files[index].Path != path {
			same = false
		}
		index++
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	if !same || index != len(files) {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM captures WHERE id=?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
