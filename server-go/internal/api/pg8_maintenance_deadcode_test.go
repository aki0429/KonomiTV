package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// このファイルは maintenance 系の死コード整理 (PG-8) の回帰テストを提供する。
//
// 背景: maintenance.go にはルート登録されないまま残った旧ハンドラー
// (handleMaintenanceBatchScan / handleMaintenanceBackgroundAnalysis) が存在し、
// コメントも現状 (metadata.go のネイティブ実装 + registerMetadataRoutes) と
// 食い違っていた。実際のルートは metadata.go 側のネイティブ実装が処理する。

// TestPG8NoDeadMaintenanceProxyHandlers は、internal/api パッケージの非テストソースに
// 未登録の死ハンドラーが残っていないことを検証する。
func TestPG8NoDeadMaintenanceProxyHandlers(t *testing.T) {
	// パッケージのソースディレクトリで実行されていることを確認する (走査の前提) 。
	if _, err := os.Stat("maintenance.go"); err != nil {
		t.Fatalf("test must run in the internal/api package directory: %v", err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	deadHandlers := []string{"handleMaintenanceBatchScan", "handleMaintenanceBackgroundAnalysis"}
	violations := []string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, dead := range deadHandlers {
			if strings.Contains(string(source), dead) {
				violations = append(violations, file+": "+dead)
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("dead maintenance handlers still present in package sources: %v", violations)
	}
}

// TestPG8NativeMetadataRoutesNoProxy は、Python バックエンド無し (no-proxy) でも
// run-batch-scan / run-background-analysis がネイティブ実装 (metadata.go) で
// 204 を返し続けることを検証する (死コード削除の回帰ガード) 。
func TestPG8NativeMetadataRoutesNoProxy(t *testing.T) {
	s, _ := newTestServer(t, "")
	for _, path := range []string{"/api/maintenance/run-batch-scan", "/api/maintenance/run-background-analysis"} {
		t.Run(path, func(t *testing.T) {
			response := doJSONRequest(t, s.Handler(), http.MethodPost, path, "", "", "")
			if response.Code != http.StatusNoContent {
				t.Fatalf("empty synthetic catalog: status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), `"from":"python"`) {
				t.Errorf("route should be handled natively, not proxied: %s", response.Body.String())
			}
		})
	}
}
