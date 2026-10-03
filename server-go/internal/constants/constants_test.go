package constants

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestVersionMatchesPyproject は Go 側のバージョン定数が server/pyproject.toml と一致することを検証する。
// バージョンを更新する際にどちらかの更新を忘れるのを防ぐための回帰テスト。
func TestVersionMatchesPyproject(t *testing.T) {
	pyprojectPath := filepath.Join("..", "..", "..", "server", "pyproject.toml")
	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		t.Skipf("server/pyproject.toml not found (%s), skipping", pyprojectPath)
	}
	pattern := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`)
	matches := pattern.FindSubmatch(data)
	if matches == nil {
		t.Fatalf("failed to find version in %s", pyprojectPath)
	}
	if got, want := Version, string(matches[1]); got != want {
		t.Fatalf("Version = %q, but server/pyproject.toml says %q", got, want)
	}
}
