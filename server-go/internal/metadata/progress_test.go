package metadata

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type taskTestRenderer func(context.Context, ThumbnailRenderParams) error

func (f taskTestRenderer) Render(ctx context.Context, params ThumbnailRenderParams) error {
	return f(ctx, params)
}

func TestRecordedPathsDoNotShareUnrelatedWaits(t *testing.T) {
	folder := t.TempDir()
	buckets := map[uint32]string{}
	var first, second string
	// 旧方式で同じ枠に入った二つの別ファイルを、確実に再現する。
	for i := 0; i < 1024; i++ {
		path := filepath.Join(folder, fmt.Sprintf("recording-%d.ts", i))
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(recordedPathKey(path)))
		bucket := hash.Sum32() % 256
		if old, ok := buckets[bucket]; ok {
			first, second = old, path
			break
		}
		buckets[bucket] = path
	}
	if first == "" {
		t.Fatal("failed to construct regression paths")
	}
	unlock, err := acquireRecordedPath(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	otherUnlock, err := acquireRecordedPath(ctx, second)
	if err != nil {
		t.Fatal("unrelated thumbnail extraction blocked recording registration")
	}
	otherUnlock()
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelBlocked()
	if release, err := acquireRecordedPath(blocked, first); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("same-file exclusion failed: %v", err)
	}
}

func TestBackgroundAnalysisProgressGuardAndPartialFailure(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 2)
	entered := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{Store: store, Config: ServiceConfig{ThumbnailsDir: folder}, Logger: testLogger(t)}
	service.Thumbnails = &ThumbnailGenerator{Store: store, ThumbnailsDir: folder, Logger: testLogger(t), Renderer: taskTestRenderer(func(ctx context.Context, p ThumbnailRenderParams) error {
		if p.FilePath == paths[0] {
			close(entered)
			select {
			case <-release:
				return fmt.Errorf("synthetic damaged video")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := os.WriteFile(p.TilePath, []byte("tile"), 0600); err != nil {
			return err
		}
		return os.WriteFile(p.RepresentativePath, []byte("representative"), 0600)
	})}
	done := make(chan error, 1)
	go func() { done <- service.RunBackgroundAnalysis(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not start")
	}
	status := BackgroundAnalysisStatus()
	if !status.Running || status.Processed != 1 || status.Total != 2 {
		t.Fatalf("in-flight status %#v", status)
	}
	second := &Service{Store: store, Logger: testLogger(t)}
	if err := second.RunBackgroundAnalysis(ctx); !errors.Is(err, ErrBackgroundAnalysisRunning) {
		t.Fatalf("concurrent analysis %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrBackgroundAnalysisFailed) {
			t.Fatalf("partial failure reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not finish")
	}
	status = BackgroundAnalysisStatus()
	if status.Running || status.Processed != 2 || status.Total != 2 {
		t.Fatalf("finished status %#v", status)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos WHERE cm_sections IS NOT NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatal("failure prevented another file or CM task from finishing")
	}
	tile, representative := ThumbnailPaths(folder, fmt.Sprintf("%032d", 11))
	for _, path := range []string{tile, representative} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackgroundAnalysisCancellationWhileWaitingForFile(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	unlock, err := acquireRecordedPath(context.Background(), paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{Store: store, Logger: testLogger(t)}
	done := make(chan error, 1)
	go func() { done <- service.RunBackgroundAnalysis(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for BackgroundAnalysisStatus().Processed == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown was blocked by another file task")
	}
	if BackgroundAnalysisStatus().Running {
		t.Fatal("analysis remained running after cancellation")
	}
}

func TestBackgroundAnalysisUnwindsOnRendererPanic(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	service := &Service{Store: store, Config: ServiceConfig{ThumbnailsDir: folder}, Logger: testLogger(t), Thumbnails: &ThumbnailGenerator{Store: store, ThumbnailsDir: folder, Renderer: taskTestRenderer(func(context.Context, ThumbnailRenderParams) error { panic("synthetic renderer defect") })}}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = service.RunBackgroundAnalysis(context.Background())
	}()
	if recovered == nil {
		t.Fatal("implementation defect was hidden")
	}
	if BackgroundAnalysisStatus().Running {
		t.Fatal("failed task left the execution guard set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := acquireRecordedPath(ctx, paths[0])
	if err != nil {
		t.Fatal("failed task left its file locked")
	}
	unlock()
}

func TestOrphanThumbnailCleanupPreservesNewFiles(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	old := filepath.Join(folder, "old.webp")
	newer := filepath.Join(folder, "new.webp")
	for _, path := range []string{old, newer} {
		if err := os.WriteFile(path, []byte("synthetic image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, testFixedNow.Add(-2*time.Minute), testFixedNow.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, testFixedNow, testFixedNow); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Config: ServiceConfig{ThumbnailsDir: folder}, Logger: testLogger(t)}
	if err := service.deleteOrphanedThumbnails(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old orphan was not removed")
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatal("thumbnail created during catalog refresh was removed")
	}
}
