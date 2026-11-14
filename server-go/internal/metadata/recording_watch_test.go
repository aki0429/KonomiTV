package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordingWatchPolicyMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/recording_watch_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Platform, Force, Kernel string
			Folders, Native, Polling      []string
			EffectivePolling              []string `json:"effective_polling"`
			Mounts                        [][2]string
			Resolve                       map[string]*string
			MountError                    bool   `json:"mount_error"`
			PollDelay                     string `json:"poll_delay"`
			Minutes                       *float64
			PollDelayMS                   int64 `json:"poll_delay_ms"`
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			environment := recordingWatchEnvironment{platform: tc.Platform, forcePolling: tc.Force, pollDelay: tc.PollDelay, kernelRelease: tc.Kernel,
				resolve: func(name string) (string, error) {
					if next, ok := tc.Resolve[name]; ok {
						if next == nil {
							return "", fmt.Errorf("failed to resolve")
						}
						return *next, nil
					}
					return name, nil
				},
			}
			if tc.MountError {
				environment.mountError = fmt.Errorf("mount failure")
			}
			for _, mount := range tc.Mounts {
				environment.mounted = append(environment.mounted, recordingMount{mount[0], mount[1]})
			}
			minutes := 5.0
			if tc.Minutes != nil {
				minutes = *tc.Minutes
			}
			plans := groupRecordingWatchPaths(tc.Folders, minutes, environment)
			polling := []string{}
			for _, plan := range plans {
				if plan.polling {
					polling = append(polling, plan.folder)
				}
				period := time.Duration(tc.PollDelayMS) * time.Millisecond
				if period == 0 {
					period = time.Nanosecond
				}
				if plan.pollInterval != period {
					t.Errorf("poll interval=%s, upstream=%s", plan.pollInterval, period)
				}
				isNetwork := false
				for _, name := range tc.Polling {
					if name == plan.folder {
						isNetwork = true
					}
				}
				if isNetwork {
					for _, name := range tc.Native {
						if !plan.excludes(name) {
							t.Errorf("network parent did not exclude native root %s", name)
						}
					}
				}
			}
			if !reflect.DeepEqual(polling, tc.EffectivePolling) {
				t.Errorf("effective polling roots=%v, upstream=%v", polling, tc.EffectivePolling)
			}
		})
	}
}

func TestRecordingWatchCompactsOverlappingCoverage(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	parent := recordingWatchPlan{folder: root, canonical: root}
	sub := recordingWatchPlan{folder: child, canonical: child}
	if got := compactRecordingWatchPlans([]recordingWatchPlan{sub, parent, parent}); len(got) != 1 || got[0].folder != root {
		t.Fatalf("redundant watchers remain: %v", got)
	}
	parent.polling = true
	parent.excluded = []string{child}
	sub.polling = true
	if got := compactRecordingWatchPlans([]recordingWatchPlan{parent, sub}); len(got) != 2 {
		t.Fatalf("excluded child lost its own watcher: %v", got)
	}
	if parent.excludes(child + "-adjacent") {
		t.Fatal("native root exclusion used a string prefix instead of path components")
	}
}

// 初回の全件列挙の終了をログで待ち、通知と巡回の試験を起動速度に依存させない。
type recordingWatchReady struct {
	once  sync.Once
	ready chan struct{}
}

func (w *recordingWatchReady) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("Initial recording folder observation completed.")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(data), nil
}

func startWatchTest(t *testing.T, service *Service, completionInterval time.Duration) context.Context {
	t.Helper()
	ready := &recordingWatchReady{ready: make(chan struct{})}
	service.Logger = slog.New(slog.NewTextHandler(ready, nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); service.RunRecordingMonitor(ctx, completionInterval) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recording watcher did not stop")
		}
	})
	select {
	case <-ready.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initial recording observation did not finish")
	}
	return ctx
}

func TestRecordingWatchPollingDelayAndIndependentCompletion(t *testing.T) {
	t.Setenv("WATCHFILES_FORCE_POLLING", "1")
	t.Setenv("WATCHFILES_POLL_DELAY_MS", "650")
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	root := t.TempDir()
	file := filepath.Join(root, "delayed.ts")
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	store.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{root}, RecordedFoldersPollingInterval: 5}, Analyzer: monitorFileAnalyzer{}}
	ctx := startWatchTest(t, service, 10*time.Millisecond)
	if err := os.WriteFile(file, []byte("first data"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if rows, err := store.ListAllVideoSummaries(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("forced polling was bypassed by filesystem notifications: %v %v", rows, err)
	}
	awaitRemovalState(t, store, file, "Recording")
	data := []byte("first data plus appended recording bytes")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		service.mu.Lock()
		tracking := service.recordingFiles[file]
		updated := tracking != nil && tracking.FileSize == int64(len(data))
		service.mu.Unlock()
		if updated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file changes did not update completion tracking")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 次の 650ms の全件巡回を待たず、共通の完了チェックで Recorded へ進める。
	started := time.Now()
	clock.Store(start.Add(30 * time.Second).UnixNano())
	awaitRemovalState(t, store, file, "Recorded")
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("recording completion waited for a full folder poll: %s", elapsed)
	}
}

func TestRecordingWatchNativeNestedDirectoryAndSymlinkRoot(t *testing.T) {
	t.Setenv("WATCHFILES_FORCE_POLLING", "disabled")
	t.Setenv("WATCHFILES_POLL_DELAY_MS", "600000")
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "recordings")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{alias, root}}, Analyzer: monitorFileAnalyzer{}}
	startWatchTest(t, service, 20*time.Millisecond)
	directory := filepath.Join(alias, "new-folder")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "native.ts")
	writeRemovalRecording(t, file)
	resolved := filepath.Join(root, "new-folder", "native.ts")
	awaitRemovalState(t, store, resolved, "Recorded")
	rows, err := store.ListAllVideoSummaries(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("overlapping symlink roots registered duplicates: %v %v", rows, err)
	}
}

func TestRecordingWatchTrackingPreservesReaderSnapshot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "recording.ts")
	if err := os.WriteFile(file, []byte("growing recording"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := info.ModTime().Add(-time.Minute)
	previous := &FileRecordingInfo{LastModified: oldTime, LastChecked: oldTime, FileSize: 1, MTIMEContinuousStartAt: &oldTime}
	service := &Service{recordingFiles: map[string]*FileRecordingInfo{file: previous}}
	service.updateRecordingObservation(file, info, info.ModTime())
	if previous.FileSize != 1 || previous.MTIMEContinuousStartAt == nil {
		t.Fatal("writer mutated the snapshot held by a metadata reader")
	}
	next := service.recordingFiles[file]
	if next == previous || next.FileSize != info.Size() || next.MTIMEContinuousStartAt != nil || !next.LastChecked.Equal(info.ModTime()) {
		t.Fatalf("tracking update differs: %v", next)
	}
}
