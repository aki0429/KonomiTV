package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// 旧キャッシュを一度に読み込まず、本家と同じ 50 件単位で変換する。
const keyFrameMigrationBatchSize = 50

type keyFrameMigrationRow struct {
	ID                    int64
	Path, Hash, Container string
	Duration, FrameRate   any
	KeyFrames, SegmentMap sql.NullString
}

type keyFrameMigrationUpdate struct {
	ClearFrames bool
	SegmentMap  *string // nil の場合は現在のキャッシュをそのまま保持する
}

func (s *Store) keyFrameMigrationBatch(ctx context.Context, after int64) ([]keyFrameMigrationRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,file_path,file_hash,container_format,duration,
		video_frame_rate,key_frames,segment_map FROM recorded_videos
		WHERE status='Recorded' AND id>? ORDER BY id LIMIT ?`, after, keyFrameMigrationBatchSize)
	if err != nil {
		return nil, fmt.Errorf("failed to read legacy keyframes: %w", err)
	}
	defer rows.Close()
	batch := []keyFrameMigrationRow{}
	for rows.Next() {
		var row keyFrameMigrationRow
		if err := rows.Scan(&row.ID, &row.Path, &row.Hash, &row.Container, &row.Duration,
			&row.FrameRate, &row.KeyFrames, &row.SegmentMap); err != nil {
			return nil, fmt.Errorf("failed to read legacy keyframe row: %w", err)
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// migrationJSONArray は ORM と同じく、配列でない JSON 値を空のキャッシュとして扱う。
func migrationJSONArray(raw string) ([]json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("invalid legacy cache JSON")
	}
	if raw[0] != '[' {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func migrationNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case int64:
		number = float64(value)
	case int:
		number = float64(value)
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

// planKeyFrameMigration は本家 RecordedScanTask.__migrateKeyFramesToSegmentMap の行単位の変換。
// 変換できない旧値は削除せず、ほかの録画の移行を継続できるようエラーで返す。
func planKeyFrameMigration(row keyFrameMigrationRow) (change keyFrameMigrationUpdate, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			change = keyFrameMigrationUpdate{}
			err = fmt.Errorf("failed to convert legacy keyframes: %v", recovered)
		}
	}()
	mapEntries, err := migrationJSONArray(row.SegmentMap.String)
	if err != nil {
		return change, err
	}
	broken := false
	if row.Container == "MPEG-TS" && len(mapEntries) > 0 {
		// 欠けたキーをゼロ値へ置き換えると、破損した行を正常と誤認して旧情報を失う。
		for _, raw := range mapEntries {
			var entry struct {
				Sequence *int   `json:"sequence_index"`
				Position *int64 `json:"source_file_position"`
				DTS      *int64 `json:"source_start_dts"`
			}
			if err := json.Unmarshal(raw, &entry); err != nil || entry.Sequence == nil || entry.Position == nil || entry.DTS == nil {
				return change, fmt.Errorf("invalid legacy segment map entry")
			}
		}
		entries, err := videostream.ParseSegmentMap(row.SegmentMap.String)
		if err != nil {
			return change, err
		}
		broken = videostream.IsSegmentMapProbablyBroken(entries)
	}
	frames, err := migrationJSONArray(row.KeyFrames.String)
	if err != nil {
		return change, err
	}
	if len(frames) == 0 {
		if broken {
			empty := "[]"
			change.SegmentMap = &empty
		}
		return change, nil
	}
	// MP4 は moov から再構成する。正常な TS キャッシュも値を変えず旧配列だけを除く。
	if row.Container != "MPEG-TS" || (len(mapEntries) > 0 && !broken) {
		change.ClearFrames = true
		return change, nil
	}
	frameRate, valid := migrationNumber(row.FrameRate)
	if !valid || frameRate <= 0 {
		return change, fmt.Errorf("invalid video frame rate in legacy recording")
	}
	if len(frames) == 1 {
		empty := "[]"
		return keyFrameMigrationUpdate{ClearFrames: true, SegmentMap: &empty}, nil
	}
	duration, valid := migrationNumber(row.Duration)
	if !valid {
		return change, fmt.Errorf("invalid duration in legacy recording")
	}
	if duration <= 0 {
		empty := "[]"
		return keyFrameMigrationUpdate{ClearFrames: true, SegmentMap: &empty}, nil
	}
	keyFrames := make([]videostream.KeyFrame, 0, len(frames))
	for _, raw := range frames[:len(frames)-1] {
		var frame struct {
			DTS    *int64 `json:"dts"`
			Offset *int64 `json:"offset"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil || frame.DTS == nil || frame.Offset == nil {
			return change, fmt.Errorf("invalid legacy keyframe entry")
		}
		keyFrames = append(keyFrames, videostream.KeyFrame{DTS: *frame.DTS, Offset: *frame.Offset})
	}
	// 末尾の番兵は変換側で除くため、値を参照せずに要素数だけを保つ。
	keyFrames = append(keyFrames, videostream.KeyFrame{})
	segmentDuration := videostream.ComputeSegmentDurationSeconds(frameRate)
	// 約 6 秒を整数フレームに丸めた正常な値は 4 秒超・12 秒未満になる。
	// 極端に低い fps のゼロフレームを Go 側が 1 に補った値は、本家の移行結果と異なるため使わない。
	if segmentDuration <= 4 || segmentDuration >= 12 || math.IsInf(segmentDuration, 0) || math.IsNaN(segmentDuration) || duration/segmentDuration >= float64(int64(math.MaxInt64)) {
		return change, fmt.Errorf("invalid legacy segment duration")
	}
	encoded := videostream.MarshalSegmentMap(videostream.ConvertKeyFramesToSegmentMap(keyFrames, frameRate, duration))
	change.ClearFrames, change.SegmentMap = true, &encoded
	return change, nil
}

// saveKeyFrameMigration は読んだときの値が残っている場合だけ変更する。
// 再生中に保存されたシーク位置や、再解析・移動後の録画情報を古い値で上書きしない。
func (s *Store) saveKeyFrameMigration(ctx context.Context, row keyFrameMigrationRow, change keyFrameMigrationUpdate) (bool, error) {
	assignments := []string{}
	args := []any{}
	if change.ClearFrames {
		assignments = append(assignments, "key_frames='[]'")
	}
	if change.SegmentMap != nil {
		assignments = append(assignments, "segment_map=?")
		args = append(args, *change.SegmentMap)
	}
	if len(assignments) == 0 {
		return false, nil
	}
	args = append(args, row.ID, row.Path, row.Hash, row.Container, row.Duration, row.FrameRate, row.KeyFrames, row.SegmentMap)
	result, err := s.writeDB().ExecContext(ctx, `UPDATE recorded_videos SET `+strings.Join(assignments, ",")+`
		WHERE id=? AND status='Recorded' AND file_path=? AND file_hash=? AND container_format=?
		AND duration IS ? AND video_frame_rate IS ? AND key_frames IS ? AND segment_map IS ?`, args...)
	if err != nil {
		return false, fmt.Errorf("failed to save migrated keyframes: %w", err)
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Service) migrateKeyFramesToSegmentMap(ctx context.Context) error {
	s.logger().Info("Starting keyframe to segment map migration.")
	var lastID int64
	migrated, repaired, skipped, nextLog := 0, 0, 0, 500
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := s.Store.keyFrameMigrationBatch(ctx, lastID)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			lastID = row.ID
			change, err := planKeyFrameMigration(row)
			if err == nil && (change.ClearFrames || change.SegmentMap != nil) {
				var saved bool
				saved, err = s.Store.saveKeyFrameMigration(ctx, row, change)
				if err == nil {
					if !saved {
						skipped++
					} else if change.ClearFrames {
						migrated++
					} else {
						repaired++
					}
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				skipped++
				s.logger().Error("Failed to migrate legacy recording cache; keeping original data.", "video_id", row.ID, "file_path", row.Path, "error", err)
			}
		}
		if migrated+repaired+skipped >= nextLog {
			s.logger().Info("Keyframe to segment map migration progress.", "migrated", migrated, "repaired", repaired, "skipped", skipped)
			nextLog += 500
		}
	}
	s.logger().Info("Keyframe to segment map migration completed.", "migrated", migrated, "repaired", repaired, "skipped", skipped)
	return nil
}
