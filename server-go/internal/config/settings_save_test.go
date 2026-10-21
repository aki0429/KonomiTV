package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSaveServerSettingsMultiline は起動不能になった User-Agent 継続行の回帰を検証する。
func TestSaveServerSettingsMultiline(t *testing.T) {
	for _, oldValue := range []string{
		"|\n        stale-one\n        stale-two\n        stale-three\n        stale-four\n        stale-five\n",
		">-\n        stale-one\n        stale-two\n",
		"'stale-one\n        stale-two\n        stale-three'\n",
		"\"stale-one\n        stale-two\"\n",
	} {
		t.Run(oldValue[:1], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := "# config\niptv: # IPTV\n    # agent comment\n    user_agent: " + oldValue + "    cache_ttl: 3600 # keep\ncustom:\n    nested: [one, two]\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, agent := range []string{"New agent: 'quoted' # literal", "first\nsecond\n", "single again"} {
				if err := SaveServerSettings(path, map[string]any{"iptv": map[string]any{"user_agent": agent}}); err != nil {
					t.Fatal(err)
				}
				loaded, err := Load(path)
				if err != nil || loaded.IPTV.UserAgent != agent {
					t.Fatalf("startup load: config=%+v err=%v", loaded, err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "stale-") {
					t.Fatalf("old scalar remnants: %s", data)
				}
				for _, preserved := range []string{"# config", "# IPTV", "# agent comment", "# keep", "nested: [one, two]"} {
					if !strings.Contains(string(data), preserved) {
						t.Fatalf("lost %q: %s", preserved, data)
					}
				}
			}
		})
	}
}

// TestSaveServerSettingsFailurePreservesFile は検証失敗時の元ファイル保護を確認する。
func TestSaveServerSettingsFailurePreservesFile(t *testing.T) {
	for _, content := range []string{"iptv:\n  user_agent: old\n", "iptv: [broken\n", "iptv: {}\n---\nother: secret\n", "iptv:\n  enabled: true\n  enabled: false\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		before := []byte(content)
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := SaveServerSettings(path, map[string]any{"server": map[string]any{"port": "invalid"}}); err == nil {
			t.Fatal("invalid configuration was saved")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("failed save changed original: err=%v data=%s", err, after)
		}
		files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*.tmp"))
		if len(files) != 0 {
			t.Fatalf("left temporary files: %v", files)
		}
	}
}

// TestSaveServerSettingsConcurrent は並列部分更新の欠落と一時ファイル衝突を検証する。
func TestSaveServerSettingsConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("iptv: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, section := range []string{"one", "two", "three", "four"} {
		group.Go(func() {
			if err := SaveServerSettings(path, map[string]any{section: map[string]any{"value": section}}); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	loaded, err := LoadServerSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"one", "two", "three", "four"} {
		if loaded[section].(map[string]any)["value"] != section {
			t.Fatalf("lost concurrent update: %s", section)
		}
	}
}
