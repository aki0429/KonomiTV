package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// newTestServer はテスト用の Server を生成する。
// テスト用ディレクトリ以下に server/ と client/dist/ の構造を作成する。
func newTestServer(t *testing.T, backendURL string) (*Server, constants.Paths) {
	t.Helper()
	root := t.TempDir()
	paths := constants.NewPaths(filepath.Join(root, "server"))
	for _, directory := range []string{
		paths.ServerDir,
		paths.ClientDistDir,
		filepath.Join(paths.ClientDistDir, "assets"),
		filepath.Join(paths.StaticDir, "account-icons"),
		filepath.Join(paths.StaticDir, "logos"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// SPA の index.html とアセットを配置する
	writeFile(t, filepath.Join(paths.ClientDistDir, "index.html"), "<!DOCTYPE html><html><body>index</body></html>")
	writeFile(t, filepath.Join(paths.ClientDistDir, "assets", "app.js"), "console.log('app');")
	// デフォルトのアカウントアイコンを配置する
	writeFile(t, filepath.Join(paths.StaticDir, "account-icons", "default.png"), "default-icon")

	cfg := config.Default()
	cfg.General.Debug = true

	testDatabase := createTestDatabase(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(Options{
		Config:           cfg,
		Paths:            paths,
		DB:               testDatabase,
		Auth:             auth.NewWithSecret(testJWTSecret, testDatabase, true, logger),
		Logger:           logger,
		PythonBackendURL: backendURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// テスト中に GitHub API へアクセスしないよう、最新バージョンのキャッシュを固定値に差し替える
	latest := "0.15.0"
	server.latestVersion = &latestVersionCache{
		latestVersion: &latest,
		updatedAt:     time.Now(),
		httpClient:    http.DefaultClient,
		logger:        server.logger,
	}
	return server, paths
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVersionEndpoint は GET /api/version のレスポンスを検証する。
func TestVersionEndpoint(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/version", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}

	var body VersionInformation
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if body.Version != constants.Version {
		t.Errorf("version = %q, want %q", body.Version, constants.Version)
	}
	if body.LatestVersion == nil || *body.LatestVersion != "0.15.0" {
		t.Errorf("latest_version = %v, want 0.15.0", body.LatestVersion)
	}
	if body.Environment == nil {
		t.Errorf("environment should not be null on Windows/Linux")
	}
	if body.Backend != "EDCB" || body.Encoder != "FFmpeg" {
		t.Errorf("backend/encoder = %q/%q, want EDCB/FFmpeg", body.Backend, body.Encoder)
	}
}

// TestUnknownAPIReturns404 はプロキシ無効時に未実装 API が 404 を返すことを検証する。
func TestUnknownAPIReturns404(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/unimplemented", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if recorder.Body.String() != "{\"detail\":\"Not Found\"}\n" {
		t.Errorf("body = %q", recorder.Body.String())
	}
}

// TestProxyForwardsUnimplementedAPIs は未実装 API が Python 版サーバーへ転送されることを検証する。
func TestProxyForwardsUnimplementedAPIs(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"python","path":"` + r.URL.Path + `"}`))
	}))
	defer backend.Close()

	server, _ := newTestServer(t, backend.URL)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/unimplemented-for-proxy-test", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); body != `{"from":"python","path":"/api/unimplemented-for-proxy-test"}` {
		t.Errorf("body = %q", body)
	}

	// Go 実装済みのルートはプロキシされず、ネイティブに処理される
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if strings.Contains(recorder.Body.String(), `"from":"python"`) {
		t.Errorf("/api/version should not be proxied")
	}
}

// TestStaticClient は client/dist の静的配信 (SPA フォールバック含む) を検証する。
func TestStaticClient(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	cases := []struct {
		path        string
		wantStatus  int
		wantContent string
	}{
		{"/", http.StatusOK, "index"},
		{"/some/spa/route", http.StatusOK, "index"},
		{"/assets/app.js", http.StatusOK, "console.log"},
		{"/assets/missing.js", http.StatusNotFound, "Not Found"},
		{"/local/virtual.js", http.StatusNotFound, "Not Found"},
		{"/api/unimplemented", http.StatusNotFound, "Not Found"},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s: status = %d, want %d", testCase.path, recorder.Code, testCase.wantStatus)
		}
		if !strings.Contains(recorder.Body.String(), testCase.wantContent) {
			t.Errorf("%s: body = %q, want to contain %q", testCase.path, recorder.Body.String(), testCase.wantContent)
		}
	}

	// assets の JavaScript は application/javascript で配信される
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/javascript" {
		t.Errorf("assets Content-Type = %q, want application/javascript", contentType)
	}
}

// TestCORSPreflight は Starlette 互換のプリフライト応答を検証する。
func TestCORSPreflight(t *testing.T) {
	server, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodOptions, "/api/version", nil)
	request.Header.Set("Origin", "https://my.local.konomi.tv:7001")
	request.Header.Set("Access-Control-Request-Method", "GET")
	request.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "https://my.local.konomi.tv:7001" {
		t.Errorf("Access-Control-Allow-Origin = %q", origin)
	}
	if credentials := recorder.Header().Get("Access-Control-Allow-Credentials"); credentials != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q", credentials)
	}
	if methods := recorder.Header().Get("Access-Control-Allow-Methods"); methods != corsAllowMethods {
		t.Errorf("Access-Control-Allow-Methods = %q", methods)
	}
	if headers := recorder.Header().Get("Access-Control-Allow-Headers"); headers != "authorization,content-type" {
		t.Errorf("Access-Control-Allow-Headers = %q", headers)
	}
}

// TestCORSSimpleRequest は通常リクエストへの CORS ヘッダー付与を検証する。
func TestCORSSimpleRequest(t *testing.T) {
	server, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	request.Header.Set("Origin", "https://my.local.konomi.tv:7001")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "https://my.local.konomi.tv:7001" {
		t.Errorf("Access-Control-Allow-Origin = %q", origin)
	}
	if expose := recorder.Header().Get("Access-Control-Expose-Headers"); expose != corsExposeHeaders {
		t.Errorf("Access-Control-Expose-Headers = %q", expose)
	}
}

// TestMethodNotAllowedOnRoot は Root が GET/HEAD 以外に 405 を返すことを検証する。
func TestMethodNotAllowedOnRoot(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/some/path", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}
