package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceToolNeverCreatesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if err := run(context.Background(), []string{"-database", path, "-migrate"}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("maintenance tool created a database")
	}
	if err := run(context.Background(), []string{"-database", path, "-migrate", "-restore", "backup.json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("conflicting operations accepted")
	}
}

func TestMaintenanceToolMigrationAndRestore(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "existing.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE recorded_videos(id INTEGER PRIMARY KEY,status TEXT,file_path TEXT,file_hash TEXT,duration REAL,video_frame_rate REAL,container_format TEXT,key_frames TEXT,segment_map TEXT); INSERT INTO recorded_videos VALUES(1,'Recorded','synthetic.ts','synthetic',6,30,'MPEG-TS','[{"dts":0,"offset":0},{"dts":540000,"offset":188000}]','[]')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	output := &bytes.Buffer{}
	backupDir := filepath.Join(directory, "backups")
	if err := run(context.Background(), []string{"-database", path, "-backup-dir", backupDir, "-migrate"}, output); err != nil {
		t.Fatalf("migrate failed: %v %s", err, output.String())
	}
	backups, err := filepath.Glob(filepath.Join(backupDir, "video-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatal("backup missing")
	}
	if err := run(context.Background(), []string{"-database", path, "-restore", backups[0]}, output); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var keys, segment string
	if err := db.QueryRow("SELECT key_frames,segment_map FROM recorded_videos WHERE id=1").Scan(&keys, &segment); err != nil {
		t.Fatal(err)
	}
	if keys != `[{"dts":0,"offset":0},{"dts":540000,"offset":188000}]` || segment != "[]" {
		t.Fatal("CLI did not restore original cache bytes")
	}
}
