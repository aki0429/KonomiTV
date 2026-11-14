package constants

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestVersionMatchesClient は Go 側のバージョン定数がクライアントと一致することを検証する。
// バージョンを更新する際にどちらかの更新を忘れるのを防ぐための回帰テスト。
func TestVersionMatchesClient(t *testing.T) {
	packagePath := filepath.Join("..", "..", "..", "client", "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		t.Skipf("client/package.json not found (%s), skipping", packagePath)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if got, want := Version, metadata.Version; got != want {
		t.Fatalf("Version = %q, but client/package.json says %q", got, want)
	}
}
