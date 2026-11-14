package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestServerSettingsBodyValidationAndAtomicSave(t *testing.T) {
	server, db, path := setupSettingsTestServer(t)
	admin := issueTestToken(t, createTestUser(t, db, "server-validation", "password", true))
	handler := server.Handler()
	initial := doJSONRequest(t, handler, http.MethodGet, "/api/settings/server", "", "", "").Body.String()
	before := []byte("# Keep this comment\ngeneral:\n    backend: EDCB # backend comment\n    extension_option: preserved\nserver:\n    port: 7000\niptv:\n    enabled: false\nunknown:\n    nested: [keep, me]\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	checks := 0
	options := server.serverSettingsValidation
	options.EDCBStatus = func(context.Context, string) string { checks++; return "Unknown" }
	options.RunEncoder = func(context.Context, string, string, bool) ([]byte, error) {
		checks++
		return []byte("ffmpeg version test Copyright test"), nil
	}
	for _, tc := range []struct {
		body, mime, token string
		status            int
	}{
		{"{", "application/json", "", 422}, {"null", "application/json", "", 401}, {`{"general":{"edcb_url":"tcp://host:4510"}}`, "application/json", "", 401},
		{"{}", "application/json", "", 401}, {"{}", "text/plain", admin, 422}, {"", "application/json", admin, 422}, {"null", "application/json", admin, 422},
		{`{"general":{"edcb_url":"tcp://host:4510"}}`, "application/json", admin, 422},
		{`{"video":{"recorded_folders_polling_interval":"5"}}`, "application/json", admin, 422},
		{`{"capture":{"upload_folders_polling_interval":NaN}}`, "application/json", admin, 500},
		{`{"server":{"port":0}}`, "application/json", admin, 422},
	} {
		response := doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", tc.body, tc.token, tc.mime)
		if response.Code != tc.status {
			t.Fatalf("%s => %d %s, want %d", tc.body, response.Code, response.Body.String(), tc.status)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("invalid request modified settings: %v", err)
		}
	}
	if checks != 1 {
		t.Fatalf("unauthorized, absent or invalid fields triggered environmental checks: %d", checks)
	}
	// 本家が出力した複数エラーの内容と順序を、実際の認証付き PUT でも照合する。
	data, err := os.ReadFile("../config/testdata/server_settings_validation_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Input string
			Detail      json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tc := range fixture.Cases {
		if tc.Name == "multiple errors in declared order" {
			found = true
			response := doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", tc.Input, admin, "application/json")
			var actual struct{ Detail json.RawMessage }
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			var got, want any
			json.Unmarshal(actual.Detail, &got)
			json.Unmarshal(tc.Detail, &want)
			if response.Code != 422 || !reflect.DeepEqual(got, want) {
				t.Fatalf("API errors differ: got=%s want=%s", actual.Detail, tc.Detail)
			}
		}
	}
	if !found {
		t.Fatal("upstream multi-error fixture is missing")
	}
	// 実際の HTTP 通信では録画に影響する /api/version を呼ばない。
	var endpoints []string
	var endpointsMu sync.Mutex
	mirakurun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpointsMu.Lock()
		endpoints = append(endpoints, r.URL.Path)
		endpointsMu.Unlock()
		w.Header().Set("Server", "mirakc/3.4.4")
		fmt.Fprint(w, "[]")
	}))
	defer mirakurun.Close()
	directory := t.TempDir()
	file := filepath.Join(directory, "sample.ts")
	if err := os.WriteFile(file, []byte("ts"), 0600); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"general": map[string]any{"backend": "Mirakurun", "mirakurun_url": mirakurun.URL + "/base///", "encoder": "FFmpeg", "program_update_interval": 0.0},
		"tv":      map[string]any{"max_alive_time": "15.0", "debug_mode_ts_path": file},
		"video":   map[string]any{"recorded_folders": []string{directory}, "exclude_scan_paths": []string{"a\nb", "a'b", "# : ", "a\x00b"}, "recorded_folders_polling_interval": 12.25},
		"capture": map[string]any{"upload_folders": []string{directory}, "upload_folders_polling_interval": 1.5},
		"iptv":    map[string]any{"enabled": true},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", string(body), admin, "application/json")
	if response.Code != 204 {
		t.Fatalf("valid request rejected: %d %s", response.Code, response.Body.String())
	}
	endpointsMu.Lock()
	if !reflect.DeepEqual(endpoints, []string{"/base/api/tuners"}) {
		t.Fatalf("unsafe or unexpected backend endpoint: %v", endpoints)
	}
	endpointsMu.Unlock()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := yaml.Unmarshal(after, &saved); err != nil {
		t.Fatalf("saved invalid YAML: %v\n%s", err, after)
	}
	video := saved["video"].(map[string]any)
	if video["recorded_folders_polling_interval"] != 12.25 || saved["capture"].(map[string]any)["upload_folders_polling_interval"] != 1.5 {
		t.Fatal(saved)
	}
	wantPaths := []any{"a\nb", "a'b", "# : ", "a\x00b"}
	if !reflect.DeepEqual(video["exclude_scan_paths"], wantPaths) {
		t.Fatalf("YAML changed string content: %v", video["exclude_scan_paths"])
	}
	if saved["general"].(map[string]any)["extension_option"] != "preserved" || saved["iptv"].(map[string]any)["enabled"] != false || saved["unknown"] == nil || !strings.Contains(string(after), "Keep this comment") || !strings.Contains(string(after), "backend comment") {
		t.Fatalf("unrelated settings or comments lost: %s", after)
	}
	if get := doJSONRequest(t, handler, http.MethodGet, "/api/settings/server", "", "", ""); get.Body.String() != initial {
		t.Fatalf("runtime settings changed before restart: %s", get.Body.String())
	}
	// 一時ファイルの競合を避け、並行 PUT のどれか一件を完全な形で保存する。
	var workers sync.WaitGroup
	statuses := make(chan int, 12)
	for i := range 12 {
		workers.Go(func() {
			request := httptest.NewRequest(http.MethodPut, "/api/settings/server", strings.NewReader(fmt.Sprintf(`{"server":{"port":%d}}`, 7100+i)))
			request.Header.Set("Authorization", "Bearer "+admin)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		})
	}
	workers.Wait()
	close(statuses)
	for status := range statuses {
		if status != 204 {
			t.Fatalf("parallel PUT failed: %d", status)
		}
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(after, &saved); err != nil {
		t.Fatal(err)
	}
	port := saved["server"].(map[string]any)["port"].(int)
	if port < 7100 || port > 7111 {
		t.Fatalf("incomplete parallel save: %s", after)
	}
	// 壊れた設定ファイルは保存失敗として返し、元の内容を保持する。
	broken := []byte("[broken")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodPut, "/api/settings/server", "{}", admin, "application/json")
	after, _ = os.ReadFile(path)
	if response.Code != 500 || string(after) != string(broken) {
		t.Fatalf("failed save destroyed original: %d %s", response.Code, after)
	}
}
