package auth

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// pythonPasslibBcryptHash は passlib (bcrypt, rounds=12) で生成した "hunter2" のハッシュ。
// Go 側の bcrypt 実装が Python 版のハッシュを検証できることを確認するために使う。
const pythonPasslibBcryptHash = "$2b$12$auqAxYzQWGIqDvEioTt0hu4pIOhylT9lgEtQTrNxJHBywuMhgzAtu"

// createAuthTestDatabase は users テーブルだけを持つテスト用データベースと、ID=42 のユーザーを作成する。
func createAuthTestDatabase(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth-test.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
			name TEXT NOT NULL,
			password TEXT NOT NULL,
			is_admin INT NOT NULL,
			client_settings JSON NOT NULL,
			niconico_user_id INT,
			niconico_user_name TEXT,
			niconico_user_premium INT,
			niconico_access_token TEXT,
			niconico_refresh_token TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format("2006-01-02 15:04:05.999999-07:00")
	result, err := db.Exec(
		`INSERT INTO users (id, name, password, is_admin, client_settings, created_at, updated_at) VALUES (42, 'auth-test', ?, 0, '{}', ?, ?)`,
		pythonPasslibBcryptHash, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return db, id
}

// TestVerifyPasswordWithPythonHash は passlib が生成した bcrypt ハッシュを検証できることを確かめる。
func TestVerifyPasswordWithPythonHash(t *testing.T) {
	if !VerifyPassword("hunter2", pythonPasslibBcryptHash) {
		t.Error("VerifyPassword() should accept the correct password for a passlib hash")
	}
	if VerifyPassword("wrong-password", pythonPasslibBcryptHash) {
		t.Error("VerifyPassword() should reject an incorrect password")
	}
}

// TestGenerateAndAuthenticateToken はトークンの生成と検証のラウンドトリップを検証する。
func TestGenerateAndAuthenticateToken(t *testing.T) {
	db, userID := createAuthTestDatabase(t)
	manager := NewWithSecret("test-secret", db, false, slog.New(slog.DiscardHandler))

	token, err := manager.GenerateAccessToken(userID)
	if err != nil {
		t.Fatalf("GenerateAccessToken() returned an error: %v", err)
	}

	user, err := manager.AuthenticateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("AuthenticateToken() returned an error: %v", err)
	}
	if user.ID != userID || user.Name != "auth-test" {
		t.Errorf("unexpected user: %+v", user)
	}

	// 改ざんされたトークンは拒否される
	if _, err := manager.AuthenticateToken(context.Background(), token+"tampered"); err == nil {
		t.Error("AuthenticateToken() should reject a tampered token")
	}

	// 存在しないユーザーのトークンは「User associated with access token does not exist」
	missingToken, err := manager.GenerateAccessToken(999)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AuthenticateToken(context.Background(), missingToken)
	authError, ok := err.(*AuthError)
	if !ok || authError.Detail != "User associated with access token does not exist" {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestResolveUserKey は匿名 ID の Cookie の発行と再利用を検証する。
func TestResolveUserKey(t *testing.T) {
	db, _ := createAuthTestDatabase(t)
	manager := NewWithSecret("test-secret", db, true, slog.New(slog.DiscardHandler))
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	// Cookie がない場合は新しい匿名 ID が発行される
	request := httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	recorder := httptest.NewRecorder()
	key := manager.ResolveUserKey(recorder, request)
	if len(key) <= len("anon:") || key[:5] != "anon:" || !uuidPattern.MatchString(key[5:]) {
		t.Fatalf("unexpected user key: %q", key)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("anonymous cookie should be set: %+v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != anonymousIDCookieName || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteNoneMode {
		t.Errorf("unexpected cookie attributes: %+v", cookie)
	}

	// 既存の匿名 ID があればそれが再利用される
	if key[5:] != cookie.Value {
		t.Errorf("anonymous ID in user key (%q) and cookie (%q) should match", key[5:], cookie.Value)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	request.AddCookie(&http.Cookie{Name: anonymousIDCookieName, Value: cookie.Value})
	recorder = httptest.NewRecorder()
	key = manager.ResolveUserKey(recorder, request)
	if key != "anon:"+cookie.Value {
		t.Errorf("anonymous ID should be reused: %q", key)
	}

	// 不正な匿名 ID は新しいものに置き換えられる
	request = httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	request.AddCookie(&http.Cookie{Name: anonymousIDCookieName, Value: "../../etc/passwd"})
	recorder = httptest.NewRecorder()
	key = manager.ResolveUserKey(recorder, request)
	if key == "anon:../../etc/passwd" {
		t.Error("invalid anonymous ID should not be accepted")
	}

	// ログイン中のユーザーの場合は user:{ID} になる
	token, err := manager.GenerateAccessToken(42)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	key = manager.ResolveUserKey(recorder, request)
	if key != "user:42" {
		t.Errorf("user key = %q, want user:42", key)
	}
}
