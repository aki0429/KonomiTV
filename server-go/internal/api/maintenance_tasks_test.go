package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

func TestMaintenanceCaptureScanLifecycle(t *testing.T) {
	c, s, folder, _ := newCatalogTest(t)
	handler := s.Handler()
	if err := os.WriteFile(filepath.Join(folder, "image.jpg"), catalogJPEG(t, catalogMetadataFixture(), 12), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/maintenance/capture-scan/run", nil).WithContext(requestCtx))
		done <- response
	}()
	catalogEventually(t, func() bool {
		response := doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/capture-scan/status", "", "", "")
		var status catalogTaskStatus
		return response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &status) == nil && status.Running
	})
	response := doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/capture-scan/run", "", "", "")
	if response.Code != 429 {
		t.Fatalf("concurrent scan %d %s", response.Code, response.Body.String())
	}
	cancelRequest()
	select {
	case <-done:
		t.Fatal("request cancellation stopped scan")
	default:
	}
	_ = held.Close()
	select {
	case response = <-done:
		if response.Code != 204 {
			t.Fatal(response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not complete")
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/capture-scan/status", "", "", "")
	var status catalogTaskStatus
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Running || status.Processed != 1 || status.Total != 1 || status.Registered {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/capture-scan/run", "", "", "")
	if response.Code != 204 {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/capture-scan/status", "", "", "")
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || !status.Registered {
		t.Fatal("rescan lost registered state")
	}
	c.folders = append(c.folders, filepath.Join(folder, "unavailable"))
	response = doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/capture-scan/run", "", "", "")
	if response.Code != 500 {
		t.Fatalf("incomplete scan reported success: %d", response.Code)
	}
}

func TestMaintenanceVacuumLifecycle(t *testing.T) {
	_, s, _, _ := newCatalogTest(t)
	admin := createTestUser(t, s.db, "vacuum-admin", "password", true)
	user := createTestUser(t, s.db, "vacuum-user", "password", false)
	token := issueTestToken(t, admin)
	handler := s.Handler()
	for _, test := range []struct {
		token  string
		status int
	}{{"", 401}, {issueTestToken(t, user), 403}} {
		response := doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/database-vacuum/run", "", test.token, "")
		if response.Code != test.status {
			t.Fatal(response.Body.String())
		}
	}
	// 実サービスと同じく、認証の読み取りと別の単一書き込み接続を使用する。
	var sequence int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	writer, err := database.OpenReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	s.writeDB = writer
	held, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/database-vacuum/run", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { response := httptest.NewRecorder(); handler.ServeHTTP(response, request); done <- response }()
	catalogEventually(t, func() bool {
		response := doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/database-vacuum/status", "", "", "")
		var status catalogTaskStatus
		return response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &status) == nil && status.Running && status.Processed == 0 && status.Total == 0
	})
	response := doJSONRequest(t, handler, http.MethodPost, "/api/maintenance/database-vacuum/run", "", token, "")
	if response.Code != 429 {
		t.Fatalf("concurrent vacuum %d %s", response.Code, response.Body.String())
	}
	cancel()
	select {
	case <-done:
		t.Fatal("request cancellation stopped vacuum")
	default:
	}
	_ = held.Close()
	select {
	case response = <-done:
		if response.Code != 204 {
			t.Fatal(response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("vacuum did not finish")
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/maintenance/database-vacuum/status", "", "", "")
	var status catalogTaskStatus
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Running {
		t.Fatal(response.Body.String())
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil || count != 2 {
		t.Fatal("vacuum did not preserve users")
	}
}
