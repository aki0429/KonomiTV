package metadata

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type observedAnalyzer struct{ calls atomic.Int32 }

func (a *observedAnalyzer) Analyze(ctx context.Context, path string) (*RecordedProgram, error) {
	a.calls.Add(1)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	program := buildTestProgram(path, 120)
	program.Video.FileSize = info.Size()
	program.Video.FileCreatedAt = fileCreationTime(info)
	program.Video.FileModifiedAt = info.ModTime()
	program.Video.FileHash = fmt.Sprintf("%x", sha256.Sum256(data))[:32]
	return program, nil
}

func TestWatchRecordingLifecycle(t *testing.T) {
	store := openTestDB(t)
	now := testFixedNow
	store.Now = func() time.Time { return now }
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.ts")
	if err := os.WriteFile(path, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	analyzer := &observedAnalyzer{}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{dir}}, Analyzer: analyzer, Logger: testLogger(t)}
	identities := map[string]os.FileInfo{}
	pass := func(want string) {
		t.Helper()
		if err := service.watchPass(context.Background(), identities); err != nil {
			t.Fatal(err)
		}
		rows, err := store.ListAllVideoSummaries(context.Background())
		if err != nil || len(rows) != 1 || rows[0].Status != want {
			t.Fatalf("rows=%+v err=%v want=%s", rows, err, want)
		}
	}
	pass("Recording")
	now = now.Add(time.Second)
	if err := os.WriteFile(path, []byte("growing with new packets"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	pass("Recording")
	now = now.Add(20 * time.Second)
	pass("Recorded")
	if analyzer.calls.Load() != 2 {
		t.Fatalf("initial and final analysis count=%d", analyzer.calls.Load())
	}
	pass("Recorded")
	if analyzer.calls.Load() != 2 {
		t.Fatal("unchanged recording analyzed again")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted file retained: %+v err=%v", rows, err)
	}
}

func TestWatchUnavailableRootPreservesMetadata(t *testing.T) {
	store := openTestDB(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "recordings")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "present.ts")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{dir}}, Analyzer: &observedAnalyzer{}, Logger: testLogger(t)}
	identities := map[string]os.FileInfo{}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	// ルートが見えなくなった場合、watch と起動時 scan の両方が既存レコードを保持する。
	if err := os.Rename(dir, dir+"-offline"); err != nil {
		t.Fatal(err)
	}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunBatchScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("metadata lost on disconnected storage: %+v err=%v", rows, err)
	}
}

func TestWatchStopAndBusyFile(t *testing.T) {
	store := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.ts")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{dir}}, Analyzer: &observedAnalyzer{}, Logger: testLogger(t)}
	lock := recordedPathLock(ResolveRecordedPath(path))
	lock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.WatchRecordedFolders(ctx, time.Millisecond) }()
	// 同じファイルの startup 解析が未完了でも監視の終了を待たせない。
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		lock.Unlock()
		t.Fatal("watch did not stop")
	}
	lock.Unlock()
}
