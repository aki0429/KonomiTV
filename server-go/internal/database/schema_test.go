package database

import (
	"context"
	"path/filepath"
	"testing"
)

// TestEnsureSchemaPreservesData は新規作成・旧版追加列・既存データの保持・再実行を検証する。
func TestEnsureSchemaPreservesData(t *testing.T) {
	db, err := OpenReadWrite(filepath.Join(t.TempDir(), "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO users(id,name,password,is_admin,client_settings) VALUES(1,'synthetic','hash',1,'{}');
		CREATE TABLE custom_preserved(value TEXT);
		INSERT INTO custom_preserved VALUES('keep');
		INSERT INTO recorded_programs(id,recording_start_margin,recording_end_margin,is_partially_recorded,
		 title,description,detail,start_time,end_time,duration,is_free,genres,primary_audio_type,primary_audio_language)
		 VALUES(1,0,0,0,'recording','description','{}','2026-11-14 09:00:00+09:00','2026-11-14 10:00:00+09:00',3600,1,'[]','stereo','日本語');
		INSERT INTO recorded_videos(id,recorded_program_id,status,file_path,file_hash,file_size,file_created_at,file_modified_at,
		 duration,container_format,video_codec,video_codec_profile,video_scan_type,video_frame_rate,video_resolution_width,
		 video_resolution_height,primary_audio_codec,primary_audio_channel,primary_audio_sampling_rate,key_frames,thumbnail_info)
		 VALUES(1,1,'Recorded','synthetic.ts','hash',100,'2026-11-14 09:00:00+09:00','2026-11-14 10:00:00+09:00',
		 3600,'MPEG-TS','MPEG-2','Main','Interlaced',29.97,1920,1080,'AAC-LC','Stereo',48000,'[]','{"representative":{"width":480}}');
		ALTER TABLE recorded_videos DROP COLUMN missing_since;
		ALTER TABLE recorded_videos DROP COLUMN has_video_stream_changes;
		ALTER TABLE recorded_videos DROP COLUMN segment_map;
		ALTER TABLE twitter_accounts DROP COLUMN cookie_browser_info;
	`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var name, custom, filePath, segmentMap string
	var revision, missing, changes int
	if err := db.QueryRow(`SELECT name FROM users WHERE id=1`).Scan(&name); err != nil || name != "synthetic" {
		t.Fatalf("user changed: %q %v", name, err)
	}
	if err := db.QueryRow(`SELECT value FROM custom_preserved`).Scan(&custom); err != nil || custom != "keep" {
		t.Fatalf("custom data changed: %q %v", custom, err)
	}
	if err := db.QueryRow(`SELECT file_path,segment_map,has_video_stream_changes,missing_since IS NULL,
		json_extract(thumbnail_info,'$.representative.revision') FROM recorded_videos WHERE id=1`).Scan(&filePath, &segmentMap, &changes, &missing, &revision); err != nil {
		t.Fatal(err)
	}
	if filePath != "synthetic.ts" || segmentMap != "[]" || changes != 0 || missing != 1 || revision != 1 {
		t.Fatalf("recording changed: %s %s %d %d %d", filePath, segmentMap, changes, missing, revision)
	}
	// 本家の新しいキャプチャ表を利用でき、外部キーも有効なことを確かめる。
	if _, err := db.Exec(`INSERT INTO capture_favorites(user_id,capture_id) VALUES(1,999)`); err == nil {
		t.Fatal("capture foreign key was not enforced")
	}
	if _, err := db.Exec(`UPDATE recorded_videos SET thumbnail_info=json_set(thumbnail_info,'$.representative.revision',9)`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT json_extract(thumbnail_info,'$.representative.revision') FROM recorded_videos`).Scan(&revision); err != nil || revision != 9 {
		t.Fatalf("existing revision replaced: %d %v", revision, err)
	}
}

// TestEnsureSchemaFailureRollsBack は未知の非互換スキーマで部分適用を残さないことを確認する。
func TestEnsureSchemaFailureRollsBack(t *testing.T) {
	db, err := OpenReadWrite(filepath.Join(t.TempDir(), "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE recorded_programs(id INT PRIMARY KEY); INSERT INTO recorded_programs VALUES(42)`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(context.Background(), db); err == nil {
		t.Fatal("incompatible schema was accepted")
	}
	var id, createdTables int
	if err := db.QueryRow(`SELECT id FROM recorded_programs`).Scan(&id); err != nil || id != 42 {
		t.Fatal("existing data lost on failure")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'`).Scan(&createdTables); err != nil || createdTables != 0 {
		t.Fatal("partial schema was not rolled back")
	}
}
