package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// doJSONRequest はテスト用に JSON リクエストを実行してレスポンスを返す。
func doJSONRequest(t *testing.T, handler http.Handler, method string, path string, body string, token string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUserTokenAndMe はアクセストークン発行から /api/users/me までの一連の流れを検証する。
func TestUserTokenAndMe(t *testing.T) {
	server, _ := newTestServer(t, "")
	createTestUser(t, server.db, "alice", "hunter2", false)
	handler := server.Handler()

	// ***** 正しいユーザー名とパスワードでアクセストークンを発行できる *****
	form := url.Values{"username": {"alice"}, "password": {"hunter2"}}
	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/users/token", form.Encode(), "", "application/x-www-form-urlencoded")
	if recorder.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var tokenResponse userAccessTokenResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &tokenResponse); err != nil {
		t.Fatal(err)
	}
	if tokenResponse.AccessToken == "" || tokenResponse.TokenType != "bearer" {
		t.Fatalf("unexpected token response: %+v", tokenResponse)
	}

	// ***** 発行したトークンで /api/users/me にアクセスできる *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me", "", tokenResponse.AccessToken, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var me userResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Name != "alice" || me.IsAdmin {
		t.Errorf("unexpected user: %+v", me)
	}
	// 空の関連アカウントは [] としてシリアライズされる (null ではない)
	for _, fragment := range []string{`"twitter_accounts":[]`, `"bluesky_accounts":[]`, `"account_links":[]`} {
		if !strings.Contains(recorder.Body.String(), fragment) {
			t.Errorf("response body should contain %s (body: %s)", fragment, recorder.Body.String())
		}
	}

	// ***** 未認証の場合は 401 Not authenticated *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me", "", "", "")
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Not authenticated") {
		t.Errorf("unauthenticated me: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 不正なトークンの場合は 401 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me", "", "invalid-token", "")
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Access token is invalid") {
		t.Errorf("invalid token: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 正しくないパスワードの場合は 401 Incorrect password *****
	form = url.Values{"username": {"alice"}, "password": {"wrong"}}
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users/token", form.Encode(), "", "application/x-www-form-urlencoded")
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Incorrect password") {
		t.Errorf("wrong password: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 存在しないユーザー名の場合は 401 Incorrect username *****
	form = url.Values{"username": {"nobody"}, "password": {"hunter2"}}
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users/token", form.Encode(), "", "application/x-www-form-urlencoded")
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "Incorrect username") {
		t.Errorf("unknown user: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 非管理者は /api/users にアクセスできない (403) *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users", "", tokenResponse.AccessToken, "")
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "Don't have permission to access this resource") {
		t.Errorf("non-admin list: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 管理者ユーザーの場合は /api/users にアクセスできる *****
	if _, err := server.db.Exec(`UPDATE users SET is_admin = 1 WHERE name = 'alice'`); err != nil {
		t.Fatal(err)
	}
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users", "", tokenResponse.AccessToken, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin list: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var users []userResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "alice" {
		t.Errorf("unexpected users: %+v", users)
	}

	// ***** 管理者は指定ユーザーを取得できる *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/alice", "", tokenResponse.AccessToken, "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"alice"`) {
		t.Errorf("specified user: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 存在しないユーザーの場合は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/unknown", "", tokenResponse.AccessToken, "")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified user was not found") {
		t.Errorf("unknown specified user: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestPythonJoseTokenCompatibility は python-jose が発行したトークンを Go 側で検証できることを確かめる。
func TestPythonJoseTokenCompatibility(t *testing.T) {
	server, _ := newTestServer(t, "")
	createTestUser(t, server.db, "bob", "password", false)
	// フィクスチャトークンの sub は 42 なので、ユーザー ID を 42 に合わせる
	setTestUserID(t, server.db, "bob", 42)

	recorder := doJSONRequest(t, server.Handler(), http.MethodGet, "/api/users/me", "", pythonJoseAccessToken, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var me userResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.ID != 42 || me.Name != "bob" {
		t.Errorf("unexpected user: %+v", me)
	}
}

// TestUserIconDefault はアイコン未設定時にデフォルトアイコンが返ることを検証する。
func TestUserIconDefault(t *testing.T) {
	server, _ := newTestServer(t, "")
	userID := createTestUser(t, server.db, "carol", "password", false)
	token := issueTestToken(t, userID)

	recorder := doJSONRequest(t, server.Handler(), http.MethodGet, "/api/users/me/icon", "", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cacheControl)
	}
	if !strings.Contains(recorder.Body.String(), "default-icon") {
		t.Errorf("body = %q, want default icon content", recorder.Body.String())
	}
}
