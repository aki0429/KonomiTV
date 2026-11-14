package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestWALReadersAndWriter は一覧を読み取り中でも更新を確定でき、既存スナップショットを保つことを検証する。
func TestWALReadersAndWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.sqlite")
	writer, err := OpenReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	var version string
	if err := writer.QueryRow(`SELECT sqlite_version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite engine %s", version)
	if _, err := writer.Exec(`CREATE TABLE counter(value INTEGER); INSERT INTO counter VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var mode string
	if err := reader.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %s %v", mode, err)
	}
	rows, err := reader.Query(`SELECT value FROM counter`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("no initial row")
	}
	var first int
	if err := rows.Scan(&first); err != nil || first != 1 {
		t.Fatal("unexpected initial snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE counter SET value=2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("reader blocked writer: %v", err)
	}
	_ = rows.Close()
	var value int
	if err := reader.QueryRow(`SELECT value FROM counter`).Scan(&value); err != nil || value != 2 {
		t.Fatal("committed value was unavailable")
	}
	if _, err := reader.Exec(`UPDATE counter SET value=3`); err == nil {
		t.Fatal("read-only connection became writable")
	}
}
