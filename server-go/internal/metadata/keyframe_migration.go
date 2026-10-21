package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

const maxLegacyCacheBytes = 16 << 20

// CacheMigrationBackup は移行前後の値を完全に保存する。復元時は後続更新を上書きしない。
type CacheMigrationBackup struct {
	Version          int    `json:"version"`
	VideoID          int64  `json:"video_id"`
	FilePath         string `json:"file_path"`
	FileHash         string `json:"file_hash"`
	BeforeKeyFrames  string `json:"before_key_frames"`
	BeforeSegmentMap string `json:"before_segment_map"`
	AfterKeyFrames   string `json:"after_key_frames"`
	AfterSegmentMap  string `json:"after_segment_map"`
}

type CacheMigrationResult struct {
	Migrated, Repaired, Skipped int
	Issues                      []CacheMigrationIssue
}
type CacheMigrationIssue struct {
	VideoID int64
	Reason  string
}

// MigrateKeyFrameCaches は一度に1録画を処理する。backupDir 空の場合は DB 横に保存する。
// schema を変更せず、バックアップの永続化成功後だけ条件付き UPDATE をコミットする。
func (s *Store) MigrateKeyFrameCaches(ctx context.Context, backupDir string) (CacheMigrationResult, error) {
	result := CacheMigrationResult{}
	var after int64
	for {
		rows, err := s.DB.QueryContext(ctx, `SELECT id FROM recorded_videos WHERE status='Recorded' AND id>? ORDER BY id LIMIT 50`, after)
		if err != nil {
			return result, err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return result, err
		}
		if len(ids) == 0 {
			return result, nil
		}
		for _, id := range ids {
			after = id
			if err := ctx.Err(); err != nil {
				return result, err
			}
			migrated, repaired, err := s.migrateOneKeyFrameCache(ctx, id, backupDir)
			if err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				result.Skipped++
				if len(result.Issues) < 20 {
					result.Issues = append(result.Issues, CacheMigrationIssue{id, err.Error()})
				}
				continue
			}
			if migrated {
				result.Migrated++
			}
			if repaired {
				result.Repaired++
			}
		}
	}
}

func (s *Store) migrateOneKeyFrameCache(ctx context.Context, id int64, backupDir string) (bool, bool, error) {
	var b CacheMigrationBackup
	b.Version = 1
	b.VideoID = id
	var duration, fps float64
	var container string
	// 巨大/異常な JSON を取り出す前にサイズを調べる。元データは変更しない。
	var keyLength, mapLength int64
	err := s.DB.QueryRowContext(ctx, `SELECT length(key_frames),length(segment_map) FROM recorded_videos WHERE id=? AND status='Recorded'`, id).Scan(&keyLength, &mapLength)
	if err != nil {
		return false, false, err
	}
	if keyLength > maxLegacyCacheBytes || mapLength > maxLegacyCacheBytes {
		return false, false, errors.New("legacy cache exceeds safe migration size")
	}
	err = s.DB.QueryRowContext(ctx, `SELECT file_path,file_hash,duration,video_frame_rate,container_format,key_frames,segment_map FROM recorded_videos WHERE id=? AND status='Recorded'`, id).Scan(&b.FilePath, &b.FileHash, &duration, &fps, &container, &b.BeforeKeyFrames, &b.BeforeSegmentMap)
	if err != nil {
		return false, false, err
	}
	if len(b.BeforeKeyFrames) > maxLegacyCacheBytes || len(b.BeforeSegmentMap) > maxLegacyCacheBytes {
		return false, false, errors.New("legacy cache changed beyond safe migration size")
	}
	entries, err := videostream.ParseSegmentMap(b.BeforeSegmentMap)
	if err != nil {
		return false, false, err
	}
	broken := container == "MPEG-TS" && videostream.IsSegmentMapProbablyBroken(entries)
	var legacy []struct {
		DTS    *int64 `json:"dts"`
		Offset *int64 `json:"offset"`
	}
	if err := json.Unmarshal([]byte(b.BeforeKeyFrames), &legacy); err != nil {
		return false, false, err
	}
	b.AfterKeyFrames = b.BeforeKeyFrames
	b.AfterSegmentMap = b.BeforeSegmentMap
	migrated, repaired := false, false
	if len(legacy) == 0 {
		if !broken {
			return false, false, nil
		}
		b.AfterSegmentMap = "[]"
		repaired = true
	} else {
		if container == "MPEG-TS" && (len(entries) == 0 || broken) {
			if math.IsNaN(fps) || math.IsInf(fps, 0) || fps < 0.001 || fps > 1000 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 6e6 {
				return false, false, errors.New("invalid legacy video timing")
			}
			frames := make([]videostream.KeyFrame, 0, len(legacy))
			for i, key := range legacy {
				if key.DTS == nil || key.Offset == nil || *key.Offset < 0 || (i > 0 && (*key.DTS < frames[i-1].DTS || *key.Offset < frames[i-1].Offset)) {
					return false, false, errors.New("invalid legacy keyframe")
				}
				frames = append(frames, videostream.KeyFrame{DTS: *key.DTS, Offset: *key.Offset})
			}
			entries = videostream.ConvertKeyFramesToSegmentMap(frames, fps, duration)
			// 変換結果が空/不正なら元 key_frames を消さない。
			if len(entries) == 0 || videostream.IsSegmentMapProbablyBroken(entries) {
				return false, false, errors.New("legacy keyframes could not produce a segment map")
			}
			b.AfterSegmentMap = videostream.MarshalSegmentMap(entries)
		}
		b.AfterKeyFrames = "[]"
		migrated = true
	}
	if backupDir == "" {
		backupDir, err = s.cacheMigrationBackupDir(ctx)
		if err != nil {
			return false, false, err
		}
	}
	if _, err := writeCacheMigrationBackup(backupDir, b); err != nil {
		return false, false, err
	}
	tx, err := s.writeDB().BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	update, err := tx.ExecContext(ctx, `UPDATE recorded_videos SET key_frames=?,segment_map=? WHERE id=? AND status='Recorded' AND file_path=? AND file_hash=? AND key_frames=? AND segment_map=? AND duration=? AND video_frame_rate=?`, b.AfterKeyFrames, b.AfterSegmentMap, id, b.FilePath, b.FileHash, b.BeforeKeyFrames, b.BeforeSegmentMap, duration, fps)
	if err != nil {
		return false, false, err
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if affected != 1 {
		return false, false, errors.New("recording changed during cache migration")
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return migrated, repaired, nil
}

func (s *Store) cacheMigrationBackupDir(ctx context.Context) (string, error) {
	rows, err := s.DB.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			return "", err
		}
		if name == "main" && path != "" {
			return filepath.Join(filepath.Dir(path), "keyframe-migration-backups"), nil
		}
	}
	return "", errors.New("persistent migration backup directory is required")
}

func writeCacheMigrationBackup(directory string, b CacheMigrationBackup) (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	name := filepath.Join(directory, fmt.Sprintf("video-%d-%s.json", b.VideoID, hex.EncodeToString(digest[:])))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	if old, err := os.ReadFile(name); err == nil {
		if string(old) != string(data) {
			return "", errors.New("cache backup content mismatch")
		}
		return name, syncCacheBackupDirectory(directory)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	file, err := os.CreateTemp(directory, ".keyframes-*.tmp")
	if err != nil {
		return "", err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := finalizeCacheBackup(temporary, name); err != nil {
		return "", err
	}
	// Linux等はディレクトリも同期。Windows のディレクトリ Sync は未対応のため別実装。
	if err := syncCacheBackupDirectory(directory); err != nil {
		return "", err
	}
	return name, nil
}

// RestoreKeyFrameCache は成功済み移行を明示的に戻す。後続の再解析/キャッシュ更新があれば拒否する。
func (s *Store) RestoreKeyFrameCache(ctx context.Context, backupPath string) error {
	file, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 128<<20 {
		return errors.New("migration backup too large")
	}
	var b CacheMigrationBackup
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	if b.Version != 1 || b.VideoID <= 0 {
		return errors.New("invalid cache migration backup")
	}
	digest := sha256.Sum256(data)
	if filepath.Base(backupPath) != fmt.Sprintf("video-%d-%s.json", b.VideoID, hex.EncodeToString(digest[:])) {
		return errors.New("migration backup checksum mismatch")
	}
	tx, err := s.writeDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE recorded_videos SET key_frames=?,segment_map=? WHERE id=? AND status='Recorded' AND file_path=? AND file_hash=? AND key_frames=? AND segment_map=?`, b.BeforeKeyFrames, b.BeforeSegmentMap, b.VideoID, b.FilePath, b.FileHash, b.AfterKeyFrames, b.AfterSegmentMap)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("recording changed since migration; restore refused")
	}
	return tx.Commit()
}
