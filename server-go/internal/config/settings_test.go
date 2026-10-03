package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestServerSettingsDefaultsMatchPython はデフォルト設定が Python 版と一致することを検証する。
func TestServerSettingsDefaultsMatchPython(t *testing.T) {
	fixture := serverSettingsFixtureData
	defaults := DefaultServerSettings()
	if !reflect.DeepEqual(defaults, fixture.Defaults) {
		t.Errorf("defaults = %+v, want %+v", defaults, fixture.Defaults)
	}
	// デフォルト値の変更がフィクスチャに影響しないことを確認する
	defaults["general"].(map[string]any)["backend"] = "Mirakurun"
	if DefaultServerSettings()["general"].(map[string]any)["backend"] != "EDCB" {
		t.Errorf("DefaultServerSettings() returns a shared map")
	}
}

// TestLoadServerSettingsMatchesPython はリポジトリの config.yaml を読み込んだ結果が
// Python 版 LoadConfig() と一致することを検証する。
func TestLoadServerSettingsMatchesPython(t *testing.T) {
	// リポジトリの config.yaml が存在しない場合はスキップする
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(mustGetwd(t))))
	configPath := filepath.Join(repoRoot, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("config.yaml was not found: %s", configPath)
	}

	settings, err := LoadServerSettings(configPath)
	if err != nil {
		t.Fatalf("LoadServerSettings() failed: %v", err)
	}
	expected := ServerSettingsFixtureLoaded()
	if !settingsEqual(settings, expected) {
		for section, value := range expected {
			if !settingsEqual(settings[section], value) {
				t.Errorf("section %q:\nactual = %+v\nwant   = %+v", section, settings[section], value)
			}
		}
		t.Errorf("settings = %+v, want %+v", settings, expected)
	}
}

// TestLoadServerSettingsMergesDefaults は config.yaml に存在しない設定値がデフォルト値で補完されることを検証する。
func TestLoadServerSettingsMergesDefaults(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	content := "general:\n    backend: 'IPTV'\n    debug: true\nserver:\n    port: 7100\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadServerSettings(configPath)
	if err != nil {
		t.Fatalf("LoadServerSettings() failed: %v", err)
	}
	general := settings["general"].(map[string]any)
	if general["backend"] != "IPTV" || general["debug"] != true {
		t.Errorf("general = %+v", general)
	}
	// config.yaml に存在しない値はデフォルト値になる
	if general["encoder"] != "FFmpeg" {
		t.Errorf("encoder = %v, want FFmpeg", general["encoder"])
	}
	if general["edcb_url"] != "tcp://127.0.0.1:4510/" {
		t.Errorf("edcb_url = %v", general["edcb_url"])
	}
	server := settings["server"].(map[string]any)
	if port, ok := toFloat64(server["port"]); !ok || port != 7100 {
		t.Errorf("port = %v, want 7100", server["port"])
	}
	if server["custom_https_certificate"] != nil {
		t.Errorf("custom_https_certificate = %v, want nil", server["custom_https_certificate"])
	}
	// 存在しないセクションもデフォルト値で補完される
	iptv := settings["iptv"].(map[string]any)
	if cacheTTL, ok := toFloat64(iptv["cache_ttl"]); !ok || cacheTTL != 3600 {
		t.Errorf("cache_ttl = %v, want 3600", iptv["cache_ttl"])
	}
}

// TestValidateServerSettings はサーバー設定のバリデーションを検証する。
func TestValidateServerSettings(t *testing.T) {
	buildSettings := func(mutate func(settings map[string]any)) map[string]any {
		settings := DefaultServerSettings()
		mutate(settings)
		return settings
	}

	testCases := []struct {
		name       string
		settings   map[string]any
		wantDetail string
	}{
		{
			name:     "デフォルト値は有効",
			settings: buildSettings(func(map[string]any) {}),
		},
		{
			name: "backend が不正",
			settings: buildSettings(func(settings map[string]any) {
				settings["general"].(map[string]any)["backend"] = "Invalid"
			}),
			wantDetail: "backend には",
		},
		{
			name: "encoder が不正",
			settings: buildSettings(func(settings map[string]any) {
				settings["general"].(map[string]any)["encoder"] = "Invalid"
			}),
			wantDetail: "encoder には",
		},
		{
			name: "program_update_interval が小さすぎる",
			settings: buildSettings(func(settings map[string]any) {
				settings["general"].(map[string]any)["program_update_interval"] = 0.05
			}),
			wantDetail: "program_update_interval",
		},
		{
			name: "edcb_url のスキームが不正",
			settings: buildSettings(func(settings map[string]any) {
				settings["general"].(map[string]any)["edcb_url"] = "http://127.0.0.1:4510/"
			}),
			wantDetail: "edcb_url",
		},
		{
			name: "mirakurun_url のスキームが不正",
			settings: buildSettings(func(settings map[string]any) {
				settings["general"].(map[string]any)["mirakurun_url"] = "tcp://127.0.0.1:40772/"
			}),
			wantDetail: "mirakurun_url",
		},
		{
			name: "port が範囲外",
			settings: buildSettings(func(settings map[string]any) {
				settings["server"].(map[string]any)["port"] = 80
			}),
			wantDetail: "ポート番号の設定が不正",
		},
		{
			name: "max_alive_time が 0",
			settings: buildSettings(func(settings map[string]any) {
				settings["tv"].(map[string]any)["max_alive_time"] = 0
			}),
			wantDetail: "max_alive_time",
		},
		{
			name: "cache_ttl が 0",
			settings: buildSettings(func(settings map[string]any) {
				settings["iptv"].(map[string]any)["cache_ttl"] = 0
			}),
			wantDetail: "cache_ttl",
		},
		{
			name: "request_timeout が小さすぎる",
			settings: buildSettings(func(settings map[string]any) {
				settings["iptv"].(map[string]any)["request_timeout"] = 0.5
			}),
			wantDetail: "request_timeout",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, detail := ValidateServerSettings(testCase.settings)
			if testCase.wantDetail == "" {
				if detail != "" {
					t.Errorf("detail = %q, want empty", detail)
				}
				return
			}
			if !strings.Contains(detail, testCase.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", detail, testCase.wantDetail)
			}
		})
	}
}

// TestValidateServerSettingsNormalizesURLs は URL の末尾のスラッシュが統一されることを検証する。
func TestValidateServerSettingsNormalizesURLs(t *testing.T) {
	settings := DefaultServerSettings()
	settings["general"].(map[string]any)["edcb_url"] = "tcp://127.0.0.1:4510"
	settings["general"].(map[string]any)["mirakurun_url"] = "http://127.0.0.1:40772"
	normalized, detail := ValidateServerSettings(settings)
	if detail != "" {
		t.Fatalf("detail = %q", detail)
	}
	general := normalized["general"].(map[string]any)
	if general["edcb_url"] != "tcp://127.0.0.1:4510/" {
		t.Errorf("edcb_url = %v", general["edcb_url"])
	}
	if general["mirakurun_url"] != "http://127.0.0.1:40772/" {
		t.Errorf("mirakurun_url = %v", general["mirakurun_url"])
	}
}

// TestSaveServerSettingsPreservesComments は config.yaml のコメントを保持したまま値が更新されることを検証する。
func TestSaveServerSettingsPreservesComments(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	// 実際の config.yaml に近い形式 (コメント・空行・フローシーケンス) を用意する
	content := `# KonomiTV の設定ファイル

general:

    # バックエンド
    backend: 'EDCB'

    # デバッグモード
    debug: false

server:

    # ポート番号
    port: 7000

iptv:

    # IPTV 機能を有効にするか
    enabled: true

    # プレイリストの一覧
    sources: [
        'https://iptv-org.github.io/iptv/index.m3u',
    ]

    # キャッシュする時間 (秒)
    cache_ttl: 3600
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := DefaultServerSettings()
	settings["general"].(map[string]any)["backend"] = "IPTV"
	settings["general"].(map[string]any)["debug"] = true
	settings["server"].(map[string]any)["port"] = 7100
	settings["iptv"].(map[string]any)["sources"] = []any{"https://example.com/playlist.m3u", "https://example.com/other.m3u"}
	settings["iptv"].(map[string]any)["cache_ttl"] = 60
	settings["tv"].(map[string]any)["max_alive_time"] = 30
	if err := SaveServerSettings(configPath, settings); err != nil {
		t.Fatalf("SaveServerSettings() failed: %v", err)
	}

	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	// コメントが保持されている
	for _, comment := range []string{
		"# KonomiTV の設定ファイル",
		"# バックエンド",
		"# デバッグモード",
		"# ポート番号",
		"# IPTV 機能を有効にするか",
		"# プレイリストの一覧",
		"# キャッシュする時間 (秒)",
	} {
		if !strings.Contains(text, comment) {
			t.Errorf("the comment %q was not preserved:\n%s", comment, text)
		}
	}

	// 更新した値が反映されている
	settings2, err := LoadServerSettings(configPath)
	if err != nil {
		t.Fatalf("LoadServerSettings() failed: %v", err)
	}
	general := settings2["general"].(map[string]any)
	if general["backend"] != "IPTV" || general["debug"] != true {
		t.Errorf("general = %+v", general)
	}
	if port, ok := toFloat64(settings2["server"].(map[string]any)["port"]); !ok || port != 7100 {
		t.Errorf("port = %v", settings2["server"].(map[string]any)["port"])
	}
	iptv := settings2["iptv"].(map[string]any)
	sources := iptv["sources"].([]any)
	if len(sources) != 2 || sources[0] != "https://example.com/playlist.m3u" || sources[1] != "https://example.com/other.m3u" {
		t.Errorf("sources = %+v", sources)
	}
	if cacheTTL, ok := toFloat64(iptv["cache_ttl"]); !ok || cacheTTL != 60 {
		t.Errorf("cache_ttl = %v", iptv["cache_ttl"])
	}
	// 既存の config.yaml に存在しなかったキーが追加されている
	if maxAliveTime, ok := toFloat64(settings2["tv"].(map[string]any)["max_alive_time"]); !ok || maxAliveTime != 30 {
		t.Errorf("max_alive_time = %v", settings2["tv"].(map[string]any)["max_alive_time"])
	}
}

// TestSaveServerSettingsRoundTrip はリポジトリの config.yaml を保存しても値が変わらないことを検証する。
func TestSaveServerSettingsRoundTrip(t *testing.T) {
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(mustGetwd(t))))
	sourcePath := filepath.Join(repoRoot, "config.yaml")
	if _, err := os.Stat(sourcePath); err != nil {
		t.Skipf("config.yaml was not found: %s", sourcePath)
	}
	original, err := LoadServerSettings(sourcePath)
	if err != nil {
		t.Fatalf("LoadServerSettings() failed: %v", err)
	}

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveServerSettings(configPath, original); err != nil {
		t.Fatalf("SaveServerSettings() failed: %v", err)
	}
	after, err := LoadServerSettings(configPath)
	if err != nil {
		t.Fatalf("LoadServerSettings() failed: %v", err)
	}
	if !settingsEqual(original, after) {
		for section, value := range original {
			if !settingsEqual(after[section], value) {
				t.Errorf("section %q:\nbefore = %+v\nafter  = %+v", section, value, after[section])
			}
		}
	}
}

// settingsEqual は設定値を比較する (数値は int / float64 の違いを無視する) 。
func settingsEqual(actual any, expected any) bool {
	switch expectedTyped := expected.(type) {
	case map[string]any:
		actualTyped, ok := actual.(map[string]any)
		if !ok || len(actualTyped) != len(expectedTyped) {
			return false
		}
		for key, value := range expectedTyped {
			if !settingsEqual(actualTyped[key], value) {
				return false
			}
		}
		return true
	case []any:
		actualTyped, ok := actual.([]any)
		if !ok || len(actualTyped) != len(expectedTyped) {
			return false
		}
		for index, value := range expectedTyped {
			if !settingsEqual(actualTyped[index], value) {
				return false
			}
		}
		return true
	case float64:
		actualNumber, ok := toFloat64(actual)
		return ok && actualNumber == expectedTyped
	case int:
		actualNumber, ok := toFloat64(actual)
		return ok && actualNumber == float64(expectedTyped)
	case json.Number:
		expectedNumber, err := expectedTyped.Float64()
		if err != nil {
			return false
		}
		actualNumber, ok := toFloat64(actual)
		return ok && actualNumber == expectedNumber
	default:
		return reflect.DeepEqual(actual, expected)
	}
}

// mustGetwd はカレントディレクトリを返す (失敗した場合はテストを終了する) 。
func mustGetwd(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// TestMarshalServerSettingsMatchesPython はサーバー設定の JSON 出力が Python 版とバイト単位で一致することを検証する。
func TestMarshalServerSettingsMatchesPython(t *testing.T) {
	serialized, err := MarshalServerSettings(ServerSettingsFixtureLoaded())
	if err != nil {
		t.Fatalf("MarshalServerSettings() failed: %v", err)
	}
	if serialized != serverSettingsFixtureData.LoadedJSON {
		t.Errorf("MarshalServerSettings() =\n%s\nwant:\n%s", serialized, serverSettingsFixtureData.LoadedJSON)
	}
}

// TestMarshalClientSettingsMatchesPython はクライアント設定の JSON 出力が
// Python 版 (Tortoise の JSONField) とバイト単位で一致することを検証する。
func TestMarshalClientSettingsMatchesPython(t *testing.T) {
	fixture := struct {
		Input    map[string]any `json:"input"`
		Expected string         `json:"expected"`
	}{}
	decoder := json.NewDecoder(strings.NewReader(string(ClientSettingsSampleJSON)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("failed to parse client_settings_sample.json: %v", err)
	}
	serialized, err := MarshalClientSettings(MergeClientSettings(fixture.Input))
	if err != nil {
		t.Fatalf("MarshalClientSettings() failed: %v", err)
	}
	if serialized != fixture.Expected {
		t.Errorf("MarshalClientSettings() =\n%s\nwant:\n%s", serialized, fixture.Expected)
	}
}

// TestFormatPythonFloat は浮動小数点数の出力形式が Python と同じであることを検証する。
func TestFormatPythonFloat(t *testing.T) {
	testCases := []struct {
		value    float64
		expected string
	}{
		{0.0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1.0, "1.0"},
		{0.5, "0.5"},
		{1.0 / 3.0, "0.3333333333333333"},
		{1700000000.0, "1700000000.0"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e-4, "0.0001"},
		{1e-5, "1e-05"},
		{123456789.123456789, "123456789.12345679"},
	}
	for _, testCase := range testCases {
		if actual := formatPythonFloat(testCase.value); actual != testCase.expected {
			t.Errorf("formatPythonFloat(%v) = %q, want %q", testCase.value, actual, testCase.expected)
		}
	}
}

// TestEncodeJSONString は文字列の JSON エスケープが Python と同じであることを検証する。
func TestEncodeJSONString(t *testing.T) {
	testCases := []struct {
		value    string
		expected string
	}{
		{`plain`, `"plain"`},
		{`日本語`, `"日本語"`},
		{"tab\tnewline\n", `"tab\tnewline\n"`},
		{"backspace\bformfeed\f", `"backspace\bformfeed\f"`},
		{"quote\"backslash\\", `"quote\"backslash\\"`},
		{"control\x01\x1f", `"control\u0001\u001f"`},
	}
	for _, testCase := range testCases {
		if actual := encodeJSONString(testCase.value); actual != testCase.expected {
			t.Errorf("encodeJSONString(%q) = %s, want %s", testCase.value, actual, testCase.expected)
		}
	}
}
