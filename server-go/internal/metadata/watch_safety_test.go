package metadata

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func makeRecordingSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		// Windows の symlink 特権がない端末では、既存ディレクトリへの junction も検証する。
		// PowerShell のコードは固定し、パスは一時テスト用環境変数で渡す。
		if info, statErr := os.Stat(target); runtime.GOOS == "windows" && statErr == nil && info.IsDir() {
			command := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", `New-Item -ItemType Junction -Path $env:KONOMITV_TEST_LINK -Target $env:KONOMITV_TEST_TARGET -ErrorAction Stop | Out-Null`)
			command.Env = append(os.Environ(), "KONOMITV_TEST_LINK="+path, "KONOMITV_TEST_TARGET="+target)
			if output, junctionErr := command.CombinedOutput(); junctionErr == nil {
				t.Log("using Windows junction instead of privileged symlink")
				return
			} else {
				t.Logf("junction unavailable: %v %s", junctionErr, output)
			}
		}
		t.Skipf("symlink unavailable: %v", err)
	}
}
func TestRecordingWalkSymlinkScopeLoopAndAliases(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(inside, "inside.ts"), filepath.Join(outside, "outside.ts")} {
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	makeRecordingSymlink(t, root, filepath.Join(inside, "loop"))
	makeRecordingSymlink(t, inside, filepath.Join(root, "alias"))
	makeRecordingSymlink(t, outside, filepath.Join(root, "outside"))
	visits := map[string]int{}
	err := walkRecordingFolders(context.Background(), root, []string{root}, nil, nil, func(path string) error { visits[filepath.Base(path)]++; return nil })
	if err != nil || visits["inside.ts"] != 1 || visits["outside.ts"] != 0 {
		t.Fatalf("scope/loop/alias: %v %v", visits, err)
	}
	visits = map[string]int{}
	err = walkRecordingFolders(context.Background(), root, []string{root, outside}, nil, nil, func(path string) error { visits[filepath.Base(path)]++; return nil })
	if err != nil || visits["outside.ts"] != 1 {
		t.Fatalf("explicit external recording root was not traversed: %v %v", visits, err)
	}
}
func TestRecordingWalkDepthLimitAndCancellation(t *testing.T) {
	root := t.TempDir()
	directory := root
	// 短い相対名にし Windows の long path への依存を減らす。
	for range maxRecordingWalkDepth {
		directory = filepath.Join(directory, "d")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := walkRecordingFolders(context.Background(), root, []string{root}, nil, nil, func(string) error { return nil }); !errors.Is(err, errRecordingWalkLimit) {
		t.Fatalf("depth was not bounded: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := walkRecordingFolders(ctx, root, []string{root}, nil, nil, func(string) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
func TestRecordingWalkBrokenLinkIsIncomplete(t *testing.T) {
	root := t.TempDir()
	makeRecordingSymlink(t, filepath.Join(root, "absent"), filepath.Join(root, "broken"))
	if err := walkRecordingFolders(context.Background(), root, []string{root}, nil, nil, func(string) error { return nil }); err == nil {
		t.Fatal("unavailable link was accepted as a complete scan")
	}
}

func TestRecordingDirectoryReadPacing(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	start := time.Now()
	for range 6 {
		_, _ = readRecordingDirectory(context.Background(), file)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("directory batches exceeded pacing: %v", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readRecordingDirectory(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("directory wait ignored cancellation: %v", err)
	}
}
func TestWatchNestedStorageReplacementKeepsMetadata(t *testing.T) {
	store := openTestDB(t)
	root := t.TempDir()
	nested := filepath.Join(root, "disk")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "saved.ts")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{root}}, Analyzer: &observedAnalyzer{}, Logger: testLogger(t)}
	identities := map[string]os.FileInfo{}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	// mount が外れ下層の空ディレクトリが見えた状況を inode の差し替えで再現する。
	if err := os.Rename(nested, nested+"-offline"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	service.Config.ExcludeScanPaths = []string{nested + "-offline"}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("metadata lost: %+v %v", rows, err)
	}
}

func TestWatchLinkedRootLifecycle(t *testing.T) {
	store := openTestDB(t)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "recordings")
	makeRecordingSymlink(t, target, link)
	path := filepath.Join(target, "saved.ts")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{link}}, Analyzer: &observedAnalyzer{}, Logger: testLogger(t)}
	identities := map[string]os.FileInfo{}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 || rows[0].FilePath != ResolveRecordedPath(path) {
		t.Fatalf("linked root was not indexed: %+v %v", rows, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("linked root did not reconcile a missing file: %+v %v", rows, err)
	}
}

type boundedIOAnalyzer struct {
	active, max atomic.Int32
	entered     chan struct{}
	release     chan struct{}
}

func (a *boundedIOAnalyzer) Analyze(ctx context.Context, path string) (*RecordedProgram, error) {
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		previous := a.max.Load()
		if previous >= active || a.max.CompareAndSwap(previous, active) {
			break
		}
	}
	a.entered <- struct{}{}
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return (&observedAnalyzer{}).Analyze(ctx, path)
}
func TestMetadataIOSharedLimitAndCanceledWaiter(t *testing.T) {
	store := openTestDB(t)
	root := t.TempDir()
	a := &boundedIOAnalyzer{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	services := []*Service{{Store: store, Analyzer: a, Logger: testLogger(t)}, {Store: store, Analyzer: a, Logger: testLogger(t)}}
	paths := []string{filepath.Join(root, "first.ts"), filepath.Join(root, "second.ts")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		old := testFixedNow.Add(-time.Hour)
		_ = os.Chtimes(path, old, old)
	}
	done := make(chan error, 2)
	go func() { done <- services[0].ProcessRecordedFile(context.Background(), paths[0], ProcessOptions{}) }()
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("first analyzer did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := acquireMetadataIO(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter did not cancel: %v", err)
	}
	if release, acquired, err := acquireMetadataIO(context.Background(), true); err != nil || acquired {
		if acquired {
			release()
		}
		t.Fatal("monitor waited or exceeded busy limit")
	}
	go func() { done <- services[1].ProcessRecordedFile(context.Background(), paths[1], ProcessOptions{}) }()
	select {
	case <-a.entered:
		t.Fatal("concurrent heavy I/O exceeded one")
	case <-time.After(20 * time.Millisecond):
	}
	a.release <- struct{}{}
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("second analyzer never resumed")
	}
	a.release <- struct{}{}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("analyzer leaked")
		}
	}
	if a.max.Load() != 1 {
		t.Fatalf("heavy I/O max=%d", a.max.Load())
	}
}
