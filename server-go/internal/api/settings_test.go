package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupSettingsTestServer は設定 API のテスト用サーバーを構築する。
func setupSettingsTestServer(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()
	server, paths := newTestServer(t, "http://127.0.0.1:1/")
	db := server.db

	// config.yaml を一時ディレクトリに用意する
	configPath := filepath.Join(filepath.Dir(paths.ServerDir), "config.yaml")
	content := "general:\n    backend: 'EDCB'\n    debug: false\nserver:\n    port: 7000\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	server.paths.ConfigYAMLPath = configPath
	return server, db, configPath
}

// TestClientSettingsAPI はクライアント設定取得・更新 API を検証する。
func TestClientSettingsAPI(t *testing.T) {
	server, db, _ := setupSettingsTestServer(t)
	userID := createTestUser(t, db, "settings_user", "password", false)
	token := issueTestToken(t, userID)
	handler := server.Handler()

	// Python 版は response_model (Pydantic の ClientSettings) を通すため、DB の保存値が空でも
	// 既定値で補完された完全な設定が返る (空の {} ではない) 。
	response := doJSONRequest(t, handler, http.MethodGet, "/api/settings/client", "", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	initial := response.Body.String()
	// 先頭は Python 版 ClientSettings のフィールドの定義順 (last_synced_at → saved_twitter_hashtags → mylist → ...)
	if !strings.HasPrefix(initial, `{"last_synced_at":0.0,"saved_twitter_hashtags":[],"mylist":[],"watched_history":[],`+
		`"pinned_channel_ids":[],"timetable_channel_width":"Normal","timetable_hour_height":"Normal",`+
		`"timetable_hover_expand":false,"timetable_dim_shopping_programs":true,`) {
		t.Errorf("body (先頭) = %q", initial[:160])
	}
	var defaultSettings map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &defaultSettings); err != nil {
		t.Fatalf("failed to parse the response: %v", err)
	}
	// 既定値の項目数は Python 版 ClientSettings と同じ 51 個
	if len(defaultSettings) != 51 {
		t.Errorf("the default settings have %d keys, want 51", len(defaultSettings))
	}
	if defaultSettings["panel_display_state"] != "RestorePreviousState" {
		t.Errorf("panel_display_state = %v", defaultSettings["panel_display_state"])
	}
	if defaultSettings["video_watched_history_max_count"] != 50.0 {
		t.Errorf("video_watched_history_max_count = %v", defaultSettings["video_watched_history_max_count"])
	}
	if genreColors, ok := defaultSettings["timetable_genre_colors"].(map[string]any); !ok || genreColors["ドラマ"] != "Pink" {
		t.Errorf("timetable_genre_colors = %v", defaultSettings["timetable_genre_colors"])
	}

	// 未ログインの場合は 401
	response = doJSONRequest(t, handler, http.MethodGet, "/api/settings/client", "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.Code)
	}

	// クライアント設定を更新する
	body := `{"last_synced_at": 1700000000.0, "caption_font": "テストフォント", "mylist": [{"id": "1"}], "unknown_key": true}`
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", body, token, "application/json")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	// 保存された値はコンパクトな JSON で、日本語はエスケープされない (Tortoise の JSONField 互換) 。
	// また、不足している項目は Python 版 ClientSettings (Pydantic) と同じくデフォルト値で補完され、
	// モデルに存在しない項目は無視される。
	var stored string
	if err := db.QueryRow(`SELECT client_settings FROM users WHERE id = ?`, userID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// 先頭は Python 版 ClientSettings のフィールドの定義順 (last_synced_at → saved_twitter_hashtags → mylist → ...)
	if !strings.HasPrefix(stored, `{"last_synced_at":1700000000.0,"saved_twitter_hashtags":[],"mylist":[{"id":"1"}],`) {
		t.Errorf("client_settings (先頭) = %q", stored[:120])
	}
	if !strings.Contains(stored, `"caption_font":"テストフォント"`) {
		t.Errorf("client_settings = %q, want it to contain the caption_font", stored)
	}
	if strings.Contains(stored, "unknown_key") {
		t.Errorf("client_settings = %q, want it not to contain unknown_key", stored)
	}
	// すべての項目が保存されている (Python 版 ClientSettings のフィールド数と同じ 51 個)
	var storedSettings map[string]any
	if err := json.Unmarshal([]byte(stored), &storedSettings); err != nil {
		t.Fatalf("failed to parse the stored settings: %v", err)
	}
	if len(storedSettings) != 51 {
		t.Errorf("the stored settings have %d keys, want 51", len(storedSettings))
	}

	// 取得すると保存した値がそのまま返る
	response = doJSONRequest(t, handler, http.MethodGet, "/api/settings/client", "", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var fetched map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("failed to parse the response: %v", err)
	}
	if fetched["caption_font"] != "テストフォント" {
		t.Errorf("caption_font = %v", fetched["caption_font"])
	}
	if mylist, ok := fetched["mylist"].([]any); !ok || len(mylist) != 1 {
		t.Errorf("mylist = %v", fetched["mylist"])
	}

	// サーバーに保存されている設定より古い設定は 422 になる
	oldBody := `{"last_synced_at": 1600000000.0, "caption_font": "古いフォント"}`
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", oldBody, token, "application/json")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (body = %s)", response.Code, response.Body.String())
	}

	// 新しい設定は保存できる
	newBody := `{"last_synced_at": 1800000000.0, "caption_font": "新しいフォント"}`
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", newBody, token, "application/json")
	if response.Code != http.StatusNoContent {
		t.Errorf("status = %d, body = %s", response.Code, response.Body.String())
	}

	// 不正な JSON は 422 になる
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", "{invalid", token, "application/json")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", response.Code)
	}
}

// TestServerSettingsAPI はサーバー設定取得・更新 API を検証する。
func TestServerSettingsAPI(t *testing.T) {
	server, db, configPath := setupSettingsTestServer(t)
	adminID := createTestUser(t, db, "settings_admin", "password", true)
	adminToken := issueTestToken(t, adminID)
	userID := createTestUser(t, db, "settings_user2", "password", false)
	userToken := issueTestToken(t, userID)
	handler := server.Handler()

	// サーバー設定を取得する (デフォルト値で補完される)
	response := doJSONRequest(t, handler, http.MethodGet, "/api/settings/server", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var settings map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("failed to parse the response: %v", err)
	}
	general := settings["general"].(map[string]any)
	if general["backend"] != "EDCB" {
		t.Errorf("backend = %v, want EDCB", general["backend"])
	}
	if general["encoder"] != "FFmpeg" {
		t.Errorf("encoder = %v, want FFmpeg", general["encoder"])
	}
	if general["program_update_interval"] != 5.0 {
		t.Errorf("program_update_interval = %v, want 5", general["program_update_interval"])
	}
	if settings["iptv"].(map[string]any)["enabled"] != true {
		t.Errorf("iptv.enabled = %v, want true", settings["iptv"].(map[string]any)["enabled"])
	}

	// 一般ユーザーは更新できない
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", `{}`, userToken, "application/json")
	if response.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", response.Code)
	}

	// 管理者は更新できる
	updated := map[string]any{
		"general": map[string]any{"backend": "IPTV", "debug": true},
		"server":  map[string]any{"port": 7200},
		"iptv":    map[string]any{"sources": []any{"https://example.com/playlist.m3u"}, "cache_ttl": 60},
	}
	body, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", string(body), adminToken, "application/json")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	// 保存された内容を確認する
	response = doJSONRequest(t, handler, http.MethodGet, "/api/settings/server", "", "", "")
	settings = map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	general = settings["general"].(map[string]any)
	if general["backend"] != "IPTV" || general["debug"] != true {
		t.Errorf("general = %+v", general)
	}
	// 送信しなかった項目はデフォルト値で補完される
	if general["encoder"] != "FFmpeg" {
		t.Errorf("encoder = %v", general["encoder"])
	}
	if port := settings["server"].(map[string]any)["port"]; port != 7200.0 {
		t.Errorf("port = %v", port)
	}
	sources := settings["iptv"].(map[string]any)["sources"].([]any)
	if len(sources) != 1 || sources[0] != "https://example.com/playlist.m3u" {
		t.Errorf("sources = %+v", sources)
	}

	// config.yaml が書き換えられ、コメントや他の項目が保持されている
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "backend: 'IPTV'") {
		t.Errorf("config.yaml = %s", data)
	}

	// バリデーションエラーの場合は 422 になり、config.yaml は変更されない
	invalid := map[string]any{
		"general": map[string]any{"backend": "Invalid"},
	}
	body, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", string(body), adminToken, "application/json")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", response.Code)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config.yaml was modified on a validation error")
	}
}
