package api

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/logging"
)

// このファイルはプロキシ実行時エラーログのプライバシー回帰テスト (PG-1) を提供する。
//
// 背景: リバースプロキシの ErrorHandler は、以前は接続先の生 URL
// (userinfo / path / query / fragment を含む) をそのままログに出していた。
// ログには user / password / path / query / fragment の 5 成分が漏えいしてはならない。
// 一方、レスポンスボディは従来どおり FastAPI 互換の {"detail":"Bad Gateway"} のまま清浄であること。

// pg1FailingTransport は常に失敗する合成トランスポート。実バックエンドには一切接続しない。
type pg1FailingTransport struct {
	err error
}

func (t pg1FailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

// servePG1ProxyError は生成したプロキシに合成トランスポートを差し込み、
// 代表的な未移行リクエストを 1 本通してログ出力とレスポンスを返す。
func servePG1ProxyError(t *testing.T, backendURL string, transport http.RoundTripper) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(logging.NewPythonHandler([]io.Writer{&buf}, slog.LevelInfo))
	handler, err := newProxy(backendURL, logger)
	if err != nil {
		t.Fatalf("newProxy failed: %v", err)
	}
	proxy, ok := handler.(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("newProxy returned %T, want *httputil.ReverseProxy", handler)
	}
	proxy.Transport = transport
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/unimplemented", nil))
	logOutput := buf.String()
	t.Logf("proxy error log: %s", strings.TrimSpace(logOutput))
	return logOutput, recorder
}

// TestPG1ProxyErrorLogHidesBackendCredentials は、接続失敗時のログに
// 生 backendURL の 5 成分 (user / password / path / query / fragment) が
// 出力されないことを検証する (レスポンスボディは清浄のまま) 。
func TestPG1ProxyErrorLogHidesBackendCredentials(t *testing.T) {
	backendURL := "http://pg1-leak-user:pg1-leak-password@pg1-leak-host.invalid:7010/pg1-leak-path?token=pg1-leak-query#pg1-leak-fragment"
	logOutput, recorder := servePG1ProxyError(t, backendURL, pg1FailingTransport{err: errors.New("synthetic pg1 transport failure")})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}

	// 5 成分はいずれもログ・レスポンスのどちらにも現れてはならない。
	for _, component := range []string{
		"pg1-leak-user",
		"pg1-leak-password",
		"pg1-leak-path",
		"pg1-leak-query",
		"pg1-leak-fragment",
	} {
		if strings.Contains(logOutput, component) {
			t.Errorf("proxy error log exposes synthetic component %q: %s", component, logOutput)
		}
		if strings.Contains(recorder.Body.String(), component) {
			t.Errorf("proxy error response exposes synthetic component %q: %s", component, recorder.Body.String())
		}
	}
	// 生 backendURL 全体もログに出てはならない。
	if strings.Contains(logOutput, backendURL) {
		t.Errorf("proxy error log exposes the raw backend URL: %s", logOutput)
	}
	// path 属性にはクライアント本来のリクエストパスだけを出力する
	// (したがって backendURL の path 成分は現れない) 。
	if !strings.Contains(logOutput, "path=/api/unimplemented") {
		t.Errorf("proxy error log should report the original client path: %s", logOutput)
	}
	// バックエンドの識別情報は host:port 表記として引き続きログに残る。
	if !strings.Contains(logOutput, "pg1-leak-host.invalid:7010") {
		t.Errorf("proxy error log lost the backend host:port identity: %s", logOutput)
	}
	// レスポンスボディは FastAPI 互換の清浄な 502 のまま。
	if got, want := recorder.Body.String(), "{\"detail\":\"Bad Gateway\"}\n"; got != want {
		t.Errorf("response body = %q, want %q", got, want)
	}
}

// TestPG1ProxyErrorLogUnwrapsURLError は、トランスポートが返した *url.Error を
// アンラップしてログ出力することを検証する。*url.Error の Error() には接続先 URL が
// 埋め込まれ、userinfo / path / query 成分がログへ漏えいし得るため、
// 原因となった内側のエラーだけを記録しなければならない。
func TestPG1ProxyErrorLogUnwrapsURLError(t *testing.T) {
	transportErr := &url.Error{
		Op: "Get",
		// 代表的な漏えい経路: URL 付きでラップされたトランスポートエラー
		URL: "http://pg1-url-secret-user:pg1-url-secret-password@pg1-url-host.invalid:7010/pg1-url-secret-path?token=pg1-url-secret-query",
		Err: errors.New("synthetic pg1 dial failure"),
	}
	backendURL := "http://pg1-url-host.invalid:7010/base"
	logOutput, recorder := servePG1ProxyError(t, backendURL, pg1FailingTransport{err: transportErr})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	for _, component := range []string{
		"pg1-url-secret-user",
		"pg1-url-secret-password",
		"pg1-url-secret-path",
		"pg1-url-secret-query",
	} {
		if strings.Contains(logOutput, component) {
			t.Errorf("unwrapped url.Error log exposes synthetic component %q: %s", component, logOutput)
		}
		if strings.Contains(recorder.Body.String(), component) {
			t.Errorf("proxy error response exposes synthetic component %q: %s", component, recorder.Body.String())
		}
	}
	// アンラップ後も原因となったエラー自体はログに残る (エラーを握りつぶさない) 。
	if !strings.Contains(logOutput, "synthetic pg1 dial failure") {
		t.Errorf("proxy error log lost the underlying transport error: %s", logOutput)
	}
	// レスポンスボディは FastAPI 互換の清浄な 502 のまま。
	if got, want := recorder.Body.String(), "{\"detail\":\"Bad Gateway\"}\n"; got != want {
		t.Errorf("response body = %q, want %q", got, want)
	}
}
