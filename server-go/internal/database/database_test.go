package database

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenReadOnlyRealDatabase は実際の KonomiTV データベースを読み取り専用で開けることを検証する。
func TestOpenReadOnlyRealDatabase(t *testing.T) {
	path := filepath.Join("..", "..", "..", "server", "data", "database.sqlite")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("database.sqlite not found (%s), skipping", path)
	}

	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly() returned an error: %v", err)
	}
	defer func() { _ = db.Close() }()

	// channels テーブルが読めること
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM channels").Scan(&count); err != nil {
		t.Fatalf("failed to query channels: %v", err)
	}

	// 書き込みは query_only により拒否されること
	if _, err := db.Exec("INSERT INTO channels (id, display_channel_id, network_id, service_id, remocon_id, channel_number, type, name, is_subchannel, is_radiochannel, is_watchable) VALUES ('test', 'test', 1, 1, 1, '1', 'GR', 'test', 0, 0, 0)"); err == nil {
		t.Fatal("write to a read-only database should fail")
	}
}
