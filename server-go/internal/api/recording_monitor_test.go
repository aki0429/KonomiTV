package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ブラウザーが切断しても、受理した一括スキャンはサーバーの生存期間で動作する。
func TestBatchScanSurvivesCanceledRequest(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.Video.RecordedFolders = []string{pg5NewSyntheticRecordingFolder(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/run-batch-scan", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	server.handleMetadataBatchScan(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s; client cancellation must not abort the scan", response.Code, response.Body.String())
	}
}
