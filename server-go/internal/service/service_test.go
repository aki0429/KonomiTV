package service

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

func fixtureDefinition(t *testing.T) *Definition {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root with spaces & 日本語")
	listener, err := net.Listen("tcp", "127.0.0.77:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	for _, directory := range []string{"server/data", "server/static", "client/dist"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(fmt.Sprintf("general:\n  backend: EDCB\n  encoder: FFmpeg\nserver:\n  port: %d\nvideo:\n  recorded_folders: []\ncapture:\n  upload_folders: []\n", port-10)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server/static/interlaced.dat", "client/dist/index.html"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	backend := "kgo"
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(self, filepath.Join(root, backend), 0755); err != nil {
		t.Fatal(err)
	}
	d, err := NewDefinition(context.Background(), root, "native-service-fixture", backend, "", true)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDefinitionReloadsPortsAndKeepsArgumentBoundaries(t *testing.T) {
	d := fixtureDefinition(t)
	first, err := d.Launch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Backend.Args) != 5 || first.Backend.Args[4] != filepath.Join(d.Root, "server") || first.HTTPS != nil || first.Backend.Args[0] != "-no-proxy" {
		t.Fatalf("invalid backend arguments: %+v", first)
	}
	if err := os.WriteFile(filepath.Join(d.Root, "config.yaml"), []byte("server:\n  port: 17290\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := d.Launch(context.Background())
	if err != nil || second.HealthURL != "http://127.0.0.77:17300" || second.InternalHost != "127.0.0.77:17300" {
		t.Fatalf("configuration reload: %+v %v", second, err)
	}
	d.ListenOverride = "0.0.0.0:17301"
	override, err := d.Launch(context.Background())
	if err != nil || override.HealthURL != "http://127.0.0.1:17301" || override.InternalHost != "127.0.0.77:17300" {
		t.Fatalf("explicit listen override: %+v %v", override, err)
	}
}

func TestDefinitionRejectsForeignRootAndEscapingExecutables(t *testing.T) {
	d := fixtureDefinition(t)
	d.ManagerBinary = filepath.Join(d.Root, "server", "go-native", "Service", "fixture", "manager")
	if err := save(d); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := copyFile(d.StatePath(), filepath.Join(foreign, "server", "data", "native-service.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(foreign); err == nil {
		t.Fatal("foreign installation state was accepted")
	}
	for _, modify := range []func(*Definition){
		func(d *Definition) { d.Name = "--all" },
		func(d *Definition) { d.BackendFile = "../kgo" },
		func(d *Definition) { d.ManagerBinary = filepath.Join(d.Root, "foreign-manager") },
		func(d *Definition) { d.ListenOverride = "remote.invalid:7002" },
		func(d *Definition) { d.ListenOverride = "203.0.113.1:7002" },
	} {
		bad := *d
		modify(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid definition was accepted: %+v", bad)
		}
	}
}

func TestOperationLockIsExclusiveAndReleases(t *testing.T) {
	file := filepath.Join(t.TempDir(), "operation.lock")
	unlock, err := acquireLock(file)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireLock(file); err == nil {
		second()
		unlock()
		t.Fatal("second operation acquired the same lock")
	}
	unlock()
	third, err := acquireLock(file)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	third()
}

func TestOnlineBackupIncludesWALAndExcludesUncommittedWrites(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.sqlite")
	db, err := database.OpenReadWrite(source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE fixture (id INTEGER PRIMARY KEY, value TEXT); INSERT INTO fixture VALUES (1, 'committed')"); err != nil {
		t.Fatal(err)
	}
	transaction, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec("INSERT INTO fixture VALUES (2, 'uncommitted')"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no WAL data: %v", err)
	}
	target := filepath.Join(root, "backup.sqlite")
	if err := backupDatabase(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", "file:"+filepath.ToSlash(target)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var rows int
	var value string
	if err := backup.QueryRow("SELECT COUNT(*), MIN(value) FROM fixture").Scan(&rows, &value); err != nil || rows != 1 || value != "committed" {
		t.Fatalf("backup is not a committed snapshot: count=%d value=%q err=%v", rows, value, err)
	}
	if err := backupDatabase(context.Background(), source, target); err == nil {
		t.Fatal("existing backup was overwritten")
	}
}

func TestVersionOutputIsBounded(t *testing.T) {
	var output versionOutput
	if _, err := output.Write([]byte(strings.Repeat("x", 4097))); err == nil || output.buffer.Len() != 0 {
		t.Fatal("oversized version output was retained")
	}
}
