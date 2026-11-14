package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func catalogEventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("capture watcher did not converge")
}

// TestCatalogFolderMonitoring は初回走査・新規フォルダ・改名・欠落・再作成と終了を実 OS 通知で検証する。
func TestCatalogFolderMonitoring(t *testing.T) {
	_, s, folder, _ := newCatalogTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	initial := filepath.Join(folder, "initial.jpg")
	data := catalogJPEG(t, catalogMetadataFixture(), 50)
	if err := os.WriteFile(initial, data, 0600); err != nil {
		t.Fatal(err)
	}
	go func() { defer close(done); s.RunCaptureFolderMonitoring(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("capture watcher did not stop")
		}
	})
	has := func(path, status string) bool {
		var got string
		err := s.db.QueryRow(`SELECT status FROM capture_files WHERE file_path=?`, path).Scan(&got)
		return err == nil && got == status
	}
	catalogEventually(t, func() bool { return has(initial, "Available") })
	nested := filepath.Join(folder, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(nested, "new.png")
	if err := os.WriteFile(image, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalogEventually(t, func() bool { return has(image, "Available") })
	renamed := filepath.Join(nested, "renamed.png")
	if err := os.Rename(image, renamed); err != nil {
		t.Fatal(err)
	}
	catalogEventually(t, func() bool { return has(renamed, "Available") })
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	catalogEventually(t, func() bool { return has(renamed, "Missing") })
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalogEventually(t, func() bool { return has(image, "Available") })
	// 再作成したディレクトリにも OS の監視が戻っていることを確認する。
	second := filepath.Join(nested, "second.jpg")
	if err := os.WriteFile(second, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalogEventually(t, func() bool { return has(second, "Available") })
}
