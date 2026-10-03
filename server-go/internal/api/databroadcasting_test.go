package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestDataBroadcastingGetProxy はデータ放送の GET プロキシの挙動を検証する。
func TestDataBroadcastingGetProxy(t *testing.T) {
	// 上流サーバー (受け取ったヘッダーを記録し、404 とフィルタ対象外のヘッダーを返す)
	var receivedHeaders http.Header
	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=5")
		w.Header().Set("X-Secret", "should-not-be-forwarded")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("upstream-body"))
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodGet, "/api/data-broadcasting/request/"+upstream.URL+"/data/test.bml", nil)
	request.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("X-Not-Allowed", "should-not-be-forwarded")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	// Python 版と同じく、上流が 404 でも常に 200 を返す
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "upstream-body" {
		t.Errorf("body = %q, want upstream-body", recorder.Body.String())
	}
	// 許可されたレスポンスヘッダーは引き継がれ、それ以外は引き継がれない
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "max-age=5" {
		t.Errorf("Cache-Control = %q", cacheControl)
	}
	if secret := recorder.Header().Get("X-Secret"); secret != "" {
		t.Errorf("X-Secret should not be forwarded: %q", secret)
	}
	// 上流に送信されるヘッダー
	if userAgent := receivedHeaders.Get("User-Agent"); !strings.HasPrefix(userAgent, "KonomiTV/") {
		t.Errorf("User-Agent = %q, want KonomiTV/...", userAgent)
	}
	if acceptLanguage := receivedHeaders.Get("Accept-Language"); acceptLanguage != "ja" {
		t.Errorf("Accept-Language = %q, want ja", acceptLanguage)
	}
	if modifiedSince := receivedHeaders.Get("If-Modified-Since"); modifiedSince == "" {
		t.Error("If-Modified-Since should be forwarded")
	}
	if notAllowed := receivedHeaders.Get("X-Not-Allowed"); notAllowed != "" {
		t.Errorf("X-Not-Allowed should not be forwarded: %q", notAllowed)
	}
	if receivedBody != "" {
		t.Errorf("GET request should not have a body: %q", receivedBody)
	}
}

// TestDataBroadcastingPostProxy はデータ放送の POST プロキシの挙動を検証する。
func TestDataBroadcastingPostProxy(t *testing.T) {
	var receivedBody string
	var receivedContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, "")
	form := url.Values{"Denbun": {"hello-world"}}
	request := httptest.NewRequest(http.MethodPost, "/api/data-broadcasting/request/"+upstream.URL+"/post.bml", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if receivedBody != "Denbun=hello-world" {
		t.Errorf("upstream body = %q, want Denbun=hello-world", receivedBody)
	}
	if receivedContentType != "application/x-www-form-urlencoded" {
		t.Errorf("upstream Content-Type = %q", receivedContentType)
	}
}

// TestDataBroadcastingRejectsNonHTTPURL は http/https 以外の URL が拒否されることを検証する。
func TestDataBroadcastingRejectsNonHTTPURL(t *testing.T) {
	server, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodGet, "/api/data-broadcasting/request/ftp://example.com/file", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Request URL must be http or https URL") {
		t.Errorf("body = %q", recorder.Body.String())
	}
}

// TestDataBroadcastingInternetStatus はインターネット接続状態確認 API の失敗時のレスポンスを検証する。
func TestDataBroadcastingInternetStatus(t *testing.T) {
	server, _ := newTestServer(t, "")
	request := httptest.NewRequest(http.MethodGet, "/api/data-broadcasting/internet-status?destination=konomitv.invalid&timeout_milliseconds=200", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"success\":false,\"ip_address\":null,\"response_time_milliseconds\":null}\n" {
		t.Errorf("body = %q", body)
	}
}
