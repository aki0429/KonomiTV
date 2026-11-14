package metadata

import (
	"context"
	"crypto/md5"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// 監視の試験では ffprobe を使わず、実ファイルの現在の stat を反映する解析器にする。
type monitorFileAnalyzer struct{}

func (monitorFileAnalyzer) Analyze(ctx context.Context, path string) (*RecordedProgram, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	program := buildTestProgram(path, 3600)
	program.Video.FileSize = info.Size()
	program.Video.FileCreatedAt = fileCreationTime(info).Round(time.Microsecond)
	program.Video.FileModifiedAt = info.ModTime().Round(time.Microsecond)
	program.Video.FileHash = fmt.Sprintf("%x", md5.Sum(data))
	return program, nil
}

func startRemovalMonitor(t *testing.T, root string) (*Store, context.Context) {
	t.Helper()
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{root}}, Analyzer: monitorFileAnalyzer{}, Logger: testLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.RunRecordingMonitor(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recording monitor did not stop")
		}
	})
	return store, ctx
}

func writeRemovalRecording(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("recording monitor fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// Windows のディレクトリ列挙と改名が重なった瞬間の共有違反だけは短く再試行する。
func renameRemovalFolder(t *testing.T, source, target string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Rename(source, target)
		if err == nil {
			return
		}
		if runtime.GOOS != "windows" || !errors.Is(err, syscall.Errno(32)) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitRemovalState(t *testing.T, store *Store, path, status string) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := store.FindVideoSummariesByPaths(context.Background(), []string{path})
		if err != nil {
			t.Fatal(err)
		}
		if status == "deleted" && len(rows) == 0 {
			return 0
		}
		if len(rows) == 1 && rows[0].Status == status {
			return rows[0].ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := store.FindVideoSummariesByPaths(context.Background(), []string{path})
	t.Fatalf("wanted %s, rows=%+v", status, rows)
	return 0
}

func TestRecordingMonitorLastFileRemovalAndRestore(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "only.ts")
	writeRemovalRecording(t, file)
	store, _ := startRemovalMonitor(t, root)
	id := awaitRemovalState(t, store, file, "Recorded")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got := awaitRemovalState(t, store, file, "Missing"); got != id {
		t.Fatalf("identity changed: %d != %d", got, id)
	}
	var since sql.NullString
	if err := store.DB.QueryRow(`SELECT missing_since FROM recorded_videos WHERE id=?`, id).Scan(&since); err != nil || !since.Valid {
		t.Fatalf("missing_since=%v err=%v", since, err)
	}
	writeRemovalRecording(t, file)
	if got := awaitRemovalState(t, store, file, "Recorded"); got != id {
		t.Fatalf("restored identity changed: %d != %d", got, id)
	}
	if err := store.DB.QueryRow(`SELECT missing_since FROM recorded_videos WHERE id=?`, id).Scan(&since); err != nil || since.Valid {
		t.Fatalf("missing_since not cleared: %v %v", since, err)
	}
}

func TestRecordingMonitorRemovedSubdirectory(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "nested")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(folder, "only.ts")
	writeRemovalRecording(t, file)
	store, _ := startRemovalMonitor(t, root)
	awaitRemovalState(t, store, file, "Recorded")
	if err := os.Rename(folder, filepath.Join(t.TempDir(), "outside")); err != nil {
		t.Fatal(err)
	}
	awaitRemovalState(t, store, file, "Missing")
}

func TestRecordingMonitorUnavailableRootAndWatchRecreation(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "recordings")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "only.ts")
	writeRemovalRecording(t, file)
	store, _ := startRemovalMonitor(t, root)
	id := awaitRemovalState(t, store, file, "Recorded")
	offline := filepath.Join(parent, "offline")
	renameRemovalFolder(t, root, offline)
	// 空の別ディレクトリが見える、マウント切断時と同じ状態にする。
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	rows, err := store.FindVideoSummariesByPaths(context.Background(), []string{file})
	if err != nil || len(rows) != 1 || rows[0].Status != "Recorded" {
		t.Fatalf("unavailable root hid recording: %+v %v", rows, err)
	}
	// Windows の巡回中はディレクトリの列挙ハンドルが一瞬残るため、
	// 保存先の交換は上と同じ共有違反だけ再試行する改名で行う。
	renameRemovalFolder(t, root, filepath.Join(parent, "empty"))
	renameRemovalFolder(t, offline, root)
	// 戻った保存先の watch を張り直した後、最後のファイルの削除を検出する。
	time.Sleep(150 * time.Millisecond)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got := awaitRemovalState(t, store, file, "Missing"); got != id {
		t.Fatal("identity changed")
	}
}

func TestRecordingMonitorRemovesIncompleteRecord(t *testing.T) {
	for _, status := range []string{"Recording", "AnalysisFailed"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "only.ts")
			writeRemovalRecording(t, file)
			store, _ := startRemovalMonitor(t, root)
			id := awaitRemovalState(t, store, file, "Recorded")
			if err := store.UpdateVideoStatus(context.Background(), id, status); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			awaitRemovalState(t, store, file, "deleted")
			var count int
			if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_programs`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("parent remained: %d %v", count, err)
			}
		})
	}
}

func TestRecordingMonitorLeavesOldInitialFilesToBatchScan(t *testing.T) {
	root := t.TempDir()
	oldFile := filepath.Join(root, "old.ts")
	writeRemovalRecording(t, oldFile)
	old := testFixedNow.Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}
	recent := filepath.Join(root, "recent.ts")
	writeRemovalRecording(t, recent)
	store, _ := startRemovalMonitor(t, root)
	awaitRemovalState(t, store, recent, "Recorded")
	rows, err := store.FindVideoSummariesByPaths(context.Background(), []string{oldFile})
	if err != nil || len(rows) != 0 {
		t.Fatalf("old file was analyzed twice at startup: %+v %v", rows, err)
	}
	// 初回以降に中身が変わった古い録画は、日時が古くても監視で反映する。
	if err := os.WriteFile(oldFile, []byte("changed old recording"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}
	awaitRemovalState(t, store, oldFile, "Recorded")
}
