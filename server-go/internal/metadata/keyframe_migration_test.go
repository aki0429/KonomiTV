package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

func seedLegacyCache(t *testing.T, s *Store, container, keys, segment string) int64 {
	t.Helper()
	program := buildTestProgram(filepath.Join(t.TempDir(), "synthetic.ts"), 30)
	if _, err := s.SaveRecordedMetadata(context.Background(), program, 0); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB.QueryRow("SELECT max(id) FROM recorded_videos").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE recorded_videos SET status='Recorded',container_format=?,duration=30,video_frame_rate=30,key_frames=?,segment_map=? WHERE id=?`, container, keys, segment, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLegacyRestoreRejectsRecordingAndTamperedBackup(t *testing.T) {
	for _, kind := range []string{"recording", "tampered"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestDB(t)
			id := seedLegacyCache(t, s, "MPEG-TS", legacyFrames(), "[]")
			dir := t.TempDir()
			if result, err := s.MigrateKeyFrameCaches(context.Background(), dir); err != nil || result.Migrated != 1 {
				t.Fatal("migration failed")
			}
			files, _ := filepath.Glob(filepath.Join(dir, "video-*.json"))
			if len(files) != 1 {
				t.Fatal("backup missing")
			}
			beforeKeys, beforeMap := readLegacyCache(t, s, id)
			if kind == "recording" {
				if _, err := s.DB.Exec("UPDATE recorded_videos SET status='Recording' WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				var backup CacheMigrationBackup
				if err := json.Unmarshal(data, &backup); err != nil {
					t.Fatal(err)
				}
				backup.BeforeKeyFrames = "[]"
				data, _ = json.Marshal(backup)
				if err := os.WriteFile(files[0], data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RestoreKeyFrameCache(context.Background(), files[0]); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			afterKeys, afterMap := readLegacyCache(t, s, id)
			if beforeKeys != afterKeys || beforeMap != afterMap {
				t.Fatal("rejected restore modified cache")
			}
		})
	}
}
func legacyFrames() string {
	keys := "["
	for i := 0; i <= 15; i++ {
		if i > 0 {
			keys += ","
		}
		keys += fmt.Sprintf(`{"dts":%d,"offset":%d}`, i*180000, i*188000)
	}
	return keys + "]"
}
func readLegacyCache(t *testing.T, s *Store, id int64) (string, string) {
	t.Helper()
	var keys, segment string
	if err := s.DB.QueryRow("SELECT key_frames,segment_map FROM recorded_videos WHERE id=?", id).Scan(&keys, &segment); err != nil {
		t.Fatal(err)
	}
	return keys, segment
}

func TestLegacyCacheMigrationAndRestoration(t *testing.T) {
	s := openTestDB(t)
	keys := legacyFrames()
	id := seedLegacyCache(t, s, "MPEG-TS", keys, "[]")
	backupDir := t.TempDir()
	result, err := s.MigrateKeyFrameCaches(context.Background(), backupDir)
	if err != nil || result.Migrated != 1 || result.Skipped != 0 {
		t.Fatalf("result=%+v %v", result, err)
	}
	after, raw := readLegacyCache(t, s, id)
	entries, err := videostream.ParseSegmentMap(raw)
	if err != nil || after != "[]" || len(entries) != 5 {
		t.Fatalf("migration=%s %s %v", after, raw, err)
	}
	for i, entry := range entries {
		if entry.SequenceIndex != i || entry.SourceFilePosition != int64(i*3*188000) || entry.SourceStartDTS != int64(i*540000) {
			t.Fatalf("segment %d = %+v", i, entry)
		}
	}
	backups, err := filepath.Glob(filepath.Join(backupDir, "video-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatal("durable backup missing")
	}
	if err := s.RestoreKeyFrameCache(context.Background(), backups[0]); err != nil {
		t.Fatal(err)
	}
	restoredKeys, restoredMap := readLegacyCache(t, s, id)
	if restoredKeys != keys || restoredMap != "[]" {
		t.Fatal("original bytes were not restored")
	}
	// 再移行後のキャッシュ更新は復元で上書きしない。
	if _, err := s.MigrateKeyFrameCaches(context.Background(), backupDir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE recorded_videos SET segment_map='[]' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreKeyFrameCache(context.Background(), backups[0]); err == nil {
		t.Fatal("restore overwrote a later cache change")
	}
}
func TestLegacyCacheFailureLeavesOriginal(t *testing.T) {
	for _, kind := range []string{"backup", "write", "invalid", "timing", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestDB(t)
			keys := legacyFrames()
			if kind == "invalid" {
				keys = `[{"offset":1}]`
			}
			id := seedLegacyCache(t, s, "MPEG-TS", keys, "[]")
			if kind == "timing" {
				if _, err := s.DB.Exec("UPDATE recorded_videos SET video_frame_rate=? WHERE id=?", 1e-10, id); err != nil {
					t.Fatal(err)
				}
			}
			backupDir := t.TempDir()
			if kind == "backup" {
				backupDir = filepath.Join(backupDir, "file")
				if err := os.WriteFile(backupDir, []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "write" {
				if _, err := s.DB.Exec(`CREATE TRIGGER refuse_cache_migration BEFORE UPDATE OF key_frames ON recorded_videos BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			result, err := s.MigrateKeyFrameCaches(ctx, backupDir)
			if kind == "cancel" {
				if err == nil {
					t.Fatal("canceled migration succeeded")
				}
			} else if err != nil || result.Skipped != 1 {
				t.Fatalf("failure not reported: %+v %v", result, err)
			}
			after, segment := readLegacyCache(t, s, id)
			if after != keys || segment != "[]" {
				t.Fatal("failed migration changed original cache")
			}
		})
	}
}
func TestLegacyCacheMP4ExistingMapRepairAndIdempotence(t *testing.T) {
	s := openTestDB(t)
	keys := legacyFrames()
	valid := `[{"sequence_index":0,"source_file_position":0,"source_start_dts":90000}]`
	mp4 := seedLegacyCache(t, s, "MPEG-4", keys, valid)
	ts := seedLegacyCache(t, s, "MPEG-TS", keys, valid)
	broken := `[{"sequence_index":0,"source_file_position":0,"source_start_dts":90000},{"sequence_index":1,"source_file_position":0,"source_start_dts":90000}]`
	repair := seedLegacyCache(t, s, "MPEG-TS", "[]", broken)
	dir := t.TempDir()
	result, err := s.MigrateKeyFrameCaches(context.Background(), dir)
	if err != nil || result.Migrated != 2 || result.Repaired != 1 || result.Skipped != 0 {
		t.Fatalf("result=%+v %v", result, err)
	}
	for _, id := range []int64{mp4, ts} {
		after, segment := readLegacyCache(t, s, id)
		if after != "[]" || segment != valid {
			t.Fatal("existing map was changed")
		}
	}
	if _, segment := readLegacyCache(t, s, repair); segment != "[]" {
		t.Fatal("broken map was not cleared")
	}
	result, err = s.MigrateKeyFrameCaches(context.Background(), dir)
	if err != nil || result.Migrated != 0 || result.Repaired != 0 {
		t.Fatalf("not idempotent: %+v %v", result, err)
	}
}

func TestBatchScanAutomaticallyMigratesLegacyCache(t *testing.T) {
	s := openTestDB(t)
	id := seedLegacyCache(t, s, "MPEG-TS", legacyFrames(), "[]")
	service := &Service{Store: s, Logger: testLogger(t)}
	if _, err := service.RunBatchScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, segment := readLegacyCache(t, s, id)
	if keys != "[]" || segment == "[]" {
		t.Fatal("batch scan did not migrate legacy cache")
	}
	dir, err := s.cacheMigrationBackupDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "video-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("automatic migration did not retain a backup")
	}
}

func TestLegacyConversionLongRecordingBoundaries(t *testing.T) {
	// 12時間・25fps・2秒GOP。各6秒セグメントの入力位置を独立した式で検証する。
	frames := make([]videostream.KeyFrame, 21601)
	for i := range frames {
		frames[i] = videostream.KeyFrame{DTS: 90000 + int64(i)*180000, Offset: int64(i) * 188000}
	}
	entries := videostream.ConvertKeyFramesToSegmentMap(frames, 25, 12*60*60)
	if len(entries) != 7200 {
		t.Fatalf("segment count=%d", len(entries))
	}
	for i, entry := range entries {
		if entry.SequenceIndex != i || entry.SourceFilePosition != int64(i)*3*188000 || entry.SourceStartDTS != 90000+int64(i)*540000 {
			t.Fatalf("segment %d=%+v", i, entry)
		}
	}
}
