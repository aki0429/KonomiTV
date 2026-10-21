//go:build !windows

package metadata

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordingWalkPermissionFailureIsIncomplete(t *testing.T) {
	root := t.TempDir()
	denied := filepath.Join(root, "denied")
	if err := os.Mkdir(denied, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(denied, 0700)
	if file, err := os.Open(denied); err == nil {
		file.Close()
		t.Skip("privileged user can bypass directory permission")
	}
	if err := walkRecordingFolders(context.Background(), root, []string{root}, nil, nil, func(string) error { return nil }); err == nil {
		t.Fatal("permission failure was treated as complete")
	}
}

func TestWatchPermissionFailurePreservesMetadata(t *testing.T) {
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
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(nested, 0700)
	if file, err := os.Open(nested); err == nil {
		file.Close()
		t.Skip("privileged user can bypass directory permission")
	}
	if err := service.watchPass(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunBatchScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("permission failure deleted metadata: %+v %v", rows, err)
	}
}
