package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaults はデフォルト値が Python 版の Pydantic モデルと一致することを検証する。
func TestDefaults(t *testing.T) {
	cfg := Default()
	if cfg.General.Backend != "EDCB" {
		t.Errorf("default backend = %q, want EDCB", cfg.General.Backend)
	}
	if cfg.General.Encoder != "FFmpeg" {
		t.Errorf("default encoder = %q, want FFmpeg", cfg.General.Encoder)
	}
	if cfg.Server.Port != 7000 {
		t.Errorf("default port = %d, want 7000", cfg.Server.Port)
	}
	if cfg.General.ProgramUpdateInterval != 5.0 {
		t.Errorf("default program_update_interval = %v, want 5.0", cfg.General.ProgramUpdateInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should be valid: %v", err)
	}
}

// TestLoad は config.yaml の読み込みとデフォルト値補完を検証する。
func TestLoad(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	yaml := `
general:
    backend: 'IPTV'
    encoder: 'NVEncC'
    debug: true
server:
    port: 7100
iptv:
    enabled: true
    sources: ['https://example.com/playlist.m3u']
    user_agent: 'test-agent'
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned an error: %v", err)
	}
	if cfg.General.Backend != "IPTV" {
		t.Errorf("backend = %q, want IPTV", cfg.General.Backend)
	}
	if cfg.General.Encoder != "NVEncC" {
		t.Errorf("encoder = %q, want NVEncC", cfg.General.Encoder)
	}
	if cfg.General.Debug != true {
		t.Errorf("debug = %v, want true", cfg.General.Debug)
	}
	if cfg.Server.Port != 7100 {
		t.Errorf("port = %d, want 7100", cfg.Server.Port)
	}
	if !cfg.IPTV.Enabled {
		t.Errorf("iptv.enabled = false, want true")
	}
	if len(cfg.IPTV.Sources) != 1 || cfg.IPTV.Sources[0] != "https://example.com/playlist.m3u" {
		t.Errorf("iptv.sources = %v", cfg.IPTV.Sources)
	}
	// config.yaml に存在しない値はデフォルトで補完される
	if cfg.General.ProgramUpdateInterval != 5.0 {
		t.Errorf("program_update_interval = %v, want default 5.0", cfg.General.ProgramUpdateInterval)
	}
}

// TestLoadRealConfig はリポジトリに実際に存在する config.yaml を読み込めることを検証する。
func TestLoadRealConfig(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("config.yaml not found (%s), skipping", path)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load the repository config.yaml: %v", err)
	}
	if cfg.Server.Port < 1 {
		t.Errorf("unexpected port: %d", cfg.Server.Port)
	}
}

// TestValidateRejectsInvalidValues は不正な設定値が拒否されることを検証する。
func TestValidateRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"backend", func(c *Config) { c.General.Backend = "Unknown" }},
		{"encoder", func(c *Config) { c.General.Encoder = "Unknown" }},
		{"port", func(c *Config) { c.Server.Port = 0 }},
		{"interval", func(c *Config) { c.General.ProgramUpdateInterval = 0 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := Default()
			testCase.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() should fail for invalid %s", testCase.name)
			}
		})
	}
}

// TestBackendAPIURL は Python 版サーバーの内部リッスン URL の算出を検証する。
func TestBackendAPIURL(t *testing.T) {
	cfg := Default()
	if got, want := cfg.BackendAPIURL(), "http://127.0.0.77:7010/"; got != want {
		t.Errorf("BackendAPIURL() = %q, want %q", got, want)
	}
	cfg.Server.Port = 7100
	if got, want := cfg.BackendAPIURL(), "http://127.0.0.77:7110/"; got != want {
		t.Errorf("BackendAPIURL() = %q, want %q", got, want)
	}
}
