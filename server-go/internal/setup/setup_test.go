package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestFreshDatabaseAndExistingDataProtection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "database.sqlite")
	if err := CreateDatabase(p); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 最新の録画再生カラム、番組、チャンネル、ユーザーを実際に問い合わせてスキーマ互換を確認する。
	for _, q := range []string{"SELECT segment_map,has_video_stream_changes FROM recorded_videos LIMIT 1", "SELECT * FROM programs LIMIT 1", "SELECT * FROM channels LIMIT 1", "SELECT * FROM capture_bookmarks LIMIT 1"} {
		r, e := db.Query(q)
		if e != nil {
			t.Fatal(e)
		}
		r.Close()
	}
	if _, err = db.Exec(`INSERT INTO users (name,password,is_admin,client_settings) VALUES ('retain-me','test-only',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	if err = CreateDatabase(p); err == nil {
		t.Fatal("existing database was accepted")
	}
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM users WHERE name='retain-me'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing user data lost: %d %v", n, err)
	}
}

func TestInstallRefusesExistingDirectoryAndCleansFailedNewInstall(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	os.Mkdir(existing, 0700)
	sentinel := filepath.Join(existing, "config.yaml")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	if err := Install(context.Background(), nil, Options{Directory: existing}); err == nil {
		t.Fatal("existing directory accepted")
	}
	if b, _ := os.ReadFile(sentinel); string(b) != "keep" {
		t.Fatal("existing configuration changed")
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("client-noiptv/index.html")
	f.Write([]byte("isolated"))
	w.Close()
	z, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	badArchive := filepath.Join(parent, "bad.zip")
	os.WriteFile(badArchive, []byte("wrong checksum"), 0600)
	target := filepath.Join(parent, "new")
	if err := Install(context.Background(), z, Options{Directory: target, Backend: "Mirakurun", DependencyArchive: badArchive}); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("failed install left a destination")
	}
	left, _ := filepath.Glob(filepath.Join(parent, ".konomitv-*"))
	if len(left) != 0 {
		t.Fatalf("staging not removed: %v", left)
	}
}

func TestPayloadSelectsOnlyRequestedClientAndRejectsTraversal(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for n, v := range map[string]string{"client-iptv/index.html": "enabled", "client-noiptv/index.html": "disabled", "bin/konomitv-go": "app"} {
			f, _ := w.Create(n)
			f.Write([]byte(v))
		}
		w.Close()
		z, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		root := t.TempDir()
		if err := ExtractPayload(z, root, enabled); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(root, "client", "dist", "index.html"))
		want := "disabled"
		if enabled {
			want = "enabled"
		}
		if string(b) != want {
			t.Fatal(string(b))
		}
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("../escape")
	f.Write([]byte("bad"))
	w.Close()
	z, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err := ExtractPayload(z, t.TempDir(), false); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestUninstallRetainsUserAndModifiedFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "server", "data"), 0700)
	for _, n := range []string{"config.yaml", "server/data/database.sqlite", "recording.ts", "modified"} {
		os.WriteFile(filepath.Join(root, n), []byte("keep"), 0600)
	}
	os.WriteFile(filepath.Join(root, "app"), []byte("app"), 0600)
	os.WriteFile(filepath.Join(root, "install-manifest.json"), []byte(`{"version":"test","files":{"app":"a172cedcae47474b615c54d510a5d84a8dea3032e958587430b413538be3f333","modified":"wrong"}}`), 0600)
	if err := Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatal("app was retained")
	}
	for _, n := range []string{"config.yaml", "server/data/database.sqlite", "recording.ts", "modified"} {
		b, e := os.ReadFile(filepath.Join(root, n))
		if e != nil || string(b) != "keep" {
			t.Fatalf("user file lost: %s", n)
		}
	}
}
