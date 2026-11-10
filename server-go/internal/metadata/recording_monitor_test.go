package metadata

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// 新規ファイルの追加を、一括スキャン API を呼ばずに検出・登録できることを検証する。
func TestRecordingMonitorDiscoversNewFile(t *testing.T) {
	store := openTestDB(t)
	// テスト DB も書き込み用接続を直列化し、監視 goroutine と検証 SELECT の競合を避ける。
	store.DB.SetMaxOpenConns(1)
	root := t.TempDir()
	file := filepath.Join(root, "new.ts")
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{root}}, Analyzer: &fakeAnalyzer{program: buildTestProgram(file, 3600)}, Logger: testLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.RunRecordingMonitor(ctx, 10*time.Millisecond) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("monitor did not stop")
		}
	}()
	// 初回観測後に作ることで、起動時だけのスキャンでは通らない回帰とする。
	time.Sleep(40 * time.Millisecond)
	if err := os.WriteFile(file, []byte("recording data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := store.ListAllVideoSummaries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].FilePath == file {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("new recording was not registered automatically")
}

// 一覧の登録は完了させるが、サムネイル・CM 解析は取り込み worker の外へ残す。
func TestMetadataOnlyDefersBackgroundAnalysis(t *testing.T) {
	store := openTestDB(t)
	// テスト DB も書き込み用接続を直列化し、監視 goroutine と検証 SELECT の競合を避ける。
	store.DB.SetMaxOpenConns(1)
	file := filepath.Join(t.TempDir(), "metadata-only.ts")
	if err := os.WriteFile(file, []byte("recording data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Analyzer: &fakeAnalyzer{program: buildTestProgram(file, 3600)}, Logger: testLogger(t)}
	if err := service.ProcessRecordedFile(context.Background(), file, ProcessOptions{MetadataOnly: true}); err != nil {
		t.Fatal(err)
	}
	var sections sql.NullString
	if err := store.DB.QueryRow(`SELECT cm_sections FROM recorded_videos`).Scan(&sections); err != nil {
		t.Fatal(err)
	}
	if sections.Valid {
		t.Fatalf("background analysis ran inline: %s", sections.String)
	}
}

// ファイルへの書き込みが止まった後、追加変更がなくても Recording から Recorded へ進める。
func TestRecordingMonitorCompletesStableRecording(t *testing.T) {
	store := openTestDB(t)
	// テスト DB も書き込み用接続を直列化し、監視 goroutine と検証 SELECT の競合を避ける。
	store.DB.SetMaxOpenConns(1)
	file := filepath.Join(t.TempDir(), "active.ts")
	if err := os.WriteFile(file, []byte("recording data"), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := os.Chtimes(file, start, start); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	store.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{filepath.Dir(file)}}, Analyzer: &fakeAnalyzer{program: buildTestProgram(file, 3600)}, Logger: testLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.RunRecordingMonitor(ctx, 5*time.Millisecond) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("monitor did not stop")
		}
	}()
	await := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			rows, err := store.ListAllVideoSummaries(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 1 && rows[0].Status == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("recording did not transition to %s", want)
	}
	await("Recording")
	clock.Store(start.Add(20 * time.Second).UnixNano())
	await("Recorded")
}
