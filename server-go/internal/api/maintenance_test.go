package api

import (
	"bufio"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setupMaintenanceTestServer はメンテナンス API のテスト用サーバーを用意する。
func setupMaintenanceTestServer(t *testing.T, backend string) (*Server, *sql.DB) {
	t.Helper()
	server, paths := newTestServer(t, backend)
	// ログファイルを一時ディレクトリに置き換える
	server.paths.ServerLogPath = filepath.Join(paths.LogsDir, "KonomiTV-Server.log")
	server.paths.AccessLogPath = filepath.Join(paths.LogsDir, "KonomiTV-Access.log")
	if err := os.MkdirAll(paths.LogsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return server, server.db
}

// TestMaintenanceLogsSSE はサーバーログストリーミング API の挙動を検証する。
func TestMaintenanceLogsSSE(t *testing.T) {
	server, db := setupMaintenanceTestServer(t, "")
	adminID := createTestUser(t, db, "maintenance_admin", "password", true)
	userID := createTestUser(t, db, "maintenance_user", "password", false)
	adminToken := issueTestToken(t, adminID)
	userToken := issueTestToken(t, userID)
	handler := server.Handler()

	// ログファイルを用意する (空行は送信されない)
	logContent := "[2026/09/22 12:53:30.229] INFO:     IPTV playlists updating...\n\n[2026/09/22 12:53:32.143] INFO:     IPTV playlists update complete.\n"
	if err := os.WriteFile(server.paths.ServerLogPath, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 未ログインの場合は 401
	response := doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/logs/server", "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	// 一般ユーザーの場合は 403
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/logs/server", "", userToken, "")
	if response.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	// 不正な log_type の場合は 422
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/logs/invalid", "", adminToken, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	// ログファイルが存在しない場合は 404
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/logs/access", "", adminToken, "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}

	// SSE を読み取る
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodGet, "/api/maintenance/logs/server", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	recorder := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, request)
	}()

	// initial_log_update イベントを待つ
	lines := recorder.readLines(t, 2, 5*time.Second)
	if len(lines) < 2 {
		t.Fatalf("initial_log_update が受信できませんでした: %v", lines)
	}
	if lines[0] != "event: initial_log_update" {
		t.Errorf("lines[0] = %q", lines[0])
	}
	if lines[1] != `data: ["[2026/09/22 12:53:30.229] INFO:     IPTV playlists updating...","[2026/09/22 12:53:32.143] INFO:     IPTV playlists update complete."]` {
		t.Errorf("lines[1] = %q", lines[1])
	}

	// ログファイルに追記すると log_update イベントが送信される
	file, err := os.OpenFile(server.paths.ServerLogPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[2026/09/22 12:53:50.929] INFO:     IPTV playlists updating... (2 source(s))\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	updateLines := recorder.readLines(t, 2, 5*time.Second)
	if len(updateLines) < 2 {
		t.Fatalf("log_update が受信できませんでした: %v", updateLines)
	}
	if updateLines[0] != "event: log_update" {
		t.Errorf("updateLines[0] = %q", updateLines[0])
	}
	if updateLines[1] != `data: "[2026/09/22 12:53:50.929] INFO:     IPTV playlists updating... (2 source(s))"` {
		t.Errorf("updateLines[1] = %q", updateLines[1])
	}

	// レスポンスヘッダーを確認する
	headers := recorder.header()
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", headers.Get("Content-Type"))
	}
	cancelRequest()
	recorder.close()
	<-done
}

// TestMaintenanceRestartShutdown はサーバー再起動・終了 API の挙動を検証する。
func TestMaintenanceRestartShutdown(t *testing.T) {
	server, db := setupMaintenanceTestServer(t, "")
	adminID := createTestUser(t, db, "maintenance_admin", "password", true)
	userID := createTestUser(t, db, "maintenance_user", "password", false)
	adminToken := issueTestToken(t, adminID)
	userToken := issueTestToken(t, userID)
	handler := server.Handler()

	var shutdownCount atomic.Int32
	server.shutdown = func() { shutdownCount.Add(1) }

	// 未ログインの場合は 401
	response := doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/shutdown", "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	// 一般ユーザーの場合は 403
	response = doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/shutdown", "", userToken, "")
	if response.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	// 管理者ユーザーの場合は 204 で、バックグラウンドでサーバーが終了する
	response = doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/shutdown", "", adminToken, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	deadline := time.Now().Add(2 * time.Second)
	for shutdownCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if shutdownCount.Load() != 1 {
		t.Errorf("shutdown が呼び出されていません: %d", shutdownCount.Load())
	}

	// 再起動の場合はロックファイルが作成される
	server.restart = func() { shutdownCount.Add(1) }
	response = doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/restart", "", adminToken, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	lockPath := filepath.Join(server.paths.DataDir, "restart_required.lock")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(lockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("restart_required.lock が作成されていません: %v", err)
	}

	// 内部ポートからのアクセス (Host ヘッダーが 127.0.0.77:<server.port + 10>) は認証不要
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/shutdown", nil)
	request.Host = "127.0.0.77:7010"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

// TestMaintenanceUpdateDatabase はデータベース更新 API の挙動を検証する。
func TestMaintenanceUpdateDatabase(t *testing.T) {
	// IPTV バックエンドの場合は認証不要で 204 を返す (チャンネル情報を DB に保存しないため何もしない)
	server, _ := setupMaintenanceTestServer(t, "")
	server.config.General.Backend = "IPTV"
	response := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/maintenance/update-database", "", "", "")
	if response.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNoContent)
	}

	// EDCB バックエンドの場合は Python 版へプロキシする
	backend := newProxyTestBackend(t)
	server, _ = setupMaintenanceTestServer(t, backend)
	server.config.General.Backend = "EDCB"
	response = doJSONRequest(t, server.Handler(), http.MethodPost, "/api/maintenance/update-database", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"from":"python"`) {
		t.Errorf("body = %s", response.Body.String())
	}

	// 一括スキャン・バックグラウンド解析も Python 版へプロキシする
	for _, path := range []string{"/api/maintenance/run-batch-scan", "/api/maintenance/run-background-analysis"} {
		response = doJSONRequest(t, server.Handler(), http.MethodPost, path, "", "", "")
		if response.Code != http.StatusOK {
			t.Errorf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

// streamRecorder はストリーミングレスポンスの内容を逐次読み取るための ResponseWriter。
type streamRecorder struct {
	headerMap http.Header
	lines     chan string
	body      io.ReadCloser
	writer    io.WriteCloser
}

// newStreamRecorder はストリーミング用の ResponseWriter を生成する。
func newStreamRecorder() *streamRecorder {
	reader, writer := io.Pipe()
	return &streamRecorder{
		headerMap: http.Header{},
		lines:     make(chan string, 1024),
		body:      reader,
		writer:    writer,
	}
}

func (r *streamRecorder) Header() http.Header {
	return r.headerMap
}

func (r *streamRecorder) WriteHeader(statusCode int) {
	r.headerMap.Set("X-Status-Code", http.StatusText(statusCode))
}

func (r *streamRecorder) Write(data []byte) (int, error) {
	return r.writer.Write(data)
}

func (r *streamRecorder) Flush() {}

func (r *streamRecorder) header() http.Header {
	return r.headerMap
}

// close はストリームを閉じる (ハンドラーを終了させる) 。
func (r *streamRecorder) close() {
	_ = r.writer.Close()
	_ = r.body.Close()
}

// readLines は指定した行数が受信できるか、タイムアウトまで待機する。
func (r *streamRecorder) readLines(t *testing.T, count int, timeout time.Duration) []string {
	t.Helper()
	scanner := bufio.NewScanner(r.body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lines := []string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			r.lines <- line
		}
	}()
	deadline := time.After(timeout)
	for len(lines) < count {
		select {
		case line := <-r.lines:
			lines = append(lines, line)
		case <-deadline:
			return lines
		}
	}
	return lines
}
