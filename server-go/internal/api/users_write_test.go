package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createTestTwitterAccountDB はテスト用の Twitter アカウントを作成する。
func createTestTwitterAccountDB(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO twitter_accounts (user_id, name, screen_name, icon_url, access_token, access_token_secret)
		VALUES (?, 'テスト Twitter', 'test_twitter', 'https://example.com/icon.png', 'token', 'secret')
	`, userID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// createTestBlueskyAccountDB はテスト用の Bluesky アカウントを作成する。
func createTestBlueskyAccountDB(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO bluesky_accounts (user_id, did, handle, name, icon_url, session_string)
		VALUES (?, 'did:plc:test', 'test.bsky.social', 'テスト Bluesky', 'https://example.com/icon.png', 'session')
	`, userID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestUserCreate はアカウント作成 API の挙動を検証する。
func TestUserCreate(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	// ***** 最初のユーザーは管理者権限で作成される *****
	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/users", `{"username":"first","password":"hunter2"}`, "", "application/json")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var first userResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Name != "first" || !first.IsAdmin {
		t.Errorf("first user = %+v, want admin named first", first)
	}

	// ***** 2人目は管理者権限なし *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users", `{"username":"second","password":"hunter2"}`, "", "application/json")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var second userResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.IsAdmin {
		t.Errorf("second user should not be an admin: %+v", second)
	}

	// ***** 重複したユーザー名は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users", `{"username":"first","password":"hunter2"}`, "", "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified username is duplicated") {
		t.Errorf("duplicate: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 利用不可のユーザー名は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users", `{"username":"ME","password":"hunter2"}`, "", "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified username is not permitted") {
		t.Errorf("reserved: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 必須フィールドがない場合は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users", `{"username":"nopassword"}`, "", "application/json")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing field: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 作成したユーザーでログインできる *****
	login := doJSONRequest(t, handler, http.MethodPost, "/api/users/token", "username=first&password=hunter2", "", "application/x-www-form-urlencoded")
	if login.Code != http.StatusOK {
		t.Errorf("login after create: status = %d, body = %s", login.Code, login.Body.String())
	}
}

// TestUserUpdateAndDelete はログイン中ユーザーの情報更新・削除を検証する。
func TestUserUpdateAndDelete(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	userID := createTestUser(t, server.db, "alice", "hunter2", false)
	token := issueTestToken(t, userID)

	// ***** ユーザー名を更新できる *****
	recorder := doJSONRequest(t, handler, http.MethodPut, "/api/users/me", `{"username":"alice2"}`, token, "application/json")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("update name: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me", "", token, "")
	if !strings.Contains(recorder.Body.String(), `"name":"alice2"`) {
		t.Errorf("name should be updated: %s", recorder.Body.String())
	}

	// ***** 既存のユーザー名への変更は 422 *****
	createTestUser(t, server.db, "bob", "hunter2", false)
	recorder = doJSONRequest(t, handler, http.MethodPut, "/api/users/me", `{"username":"bob"}`, token, "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified username is duplicated") {
		t.Errorf("duplicate update: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** パスワードを更新できる *****
	recorder = doJSONRequest(t, handler, http.MethodPut, "/api/users/me", `{"password":"newpassword"}`, token, "application/json")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("update password: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if login := doJSONRequest(t, handler, http.MethodPost, "/api/users/token", "username=alice2&password=newpassword", "", "application/x-www-form-urlencoded"); login.Code != http.StatusOK {
		t.Errorf("login with the new password: status = %d", login.Code)
	}
	if login := doJSONRequest(t, handler, http.MethodPost, "/api/users/token", "username=alice2&password=hunter2", "", "application/x-www-form-urlencoded"); login.Code != http.StatusUnauthorized {
		t.Errorf("login with the old password should fail: status = %d", login.Code)
	}

	// ***** 自分自身を削除できる *****
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/users/me", "", token, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete me: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	// 削除後は同じトークンでアクセスできない
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me", "", token, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("deleted user should not be authenticated: status = %d", recorder.Code)
	}
}

// TestAdminUserManagement は管理者によるユーザー管理 API を検証する。
func TestAdminUserManagement(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	adminID := createTestUser(t, server.db, "admin", "hunter2", true)
	normalID := createTestUser(t, server.db, "normal", "hunter2", false)
	adminToken := issueTestToken(t, adminID)

	// ***** 非管理者は指定ユーザー API にアクセスできない *****
	normalToken := issueTestToken(t, normalID)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/users/normal", "", normalToken, "")
	if recorder.Code != http.StatusForbidden {
		t.Errorf("non-admin access: status = %d", recorder.Code)
	}

	// ***** 管理者は指定ユーザーに管理者権限を付与できる *****
	recorder = doJSONRequest(t, handler, http.MethodPut, "/api/users/normal", `{"is_admin":true}`, adminToken, "application/json")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("grant admin: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var isAdmin int
	if err := server.db.QueryRow(`SELECT is_admin FROM users WHERE id = ?`, normalID).Scan(&isAdmin); err != nil {
		t.Fatal(err)
	}
	if isAdmin != 1 {
		t.Error("normal user should be an admin now")
	}

	// ***** 最後の管理者の権限は剥奪できない *****
	// admin と normal の2人が管理者なので、normal の権限は剥奪できる
	recorder = doJSONRequest(t, handler, http.MethodPut, "/api/users/normal", `{"is_admin":false}`, adminToken, "application/json")
	if recorder.Code != http.StatusNoContent {
		t.Errorf("revoke normal's admin: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	// ここで管理者は admin だけなので、admin 自身の権限は剥奪できない
	recorder = doJSONRequest(t, handler, http.MethodPut, "/api/users/admin", `{"is_admin":false}`, adminToken, "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Cannot revoke admin permission") {
		t.Errorf("revoke last admin: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 管理者はユーザーを削除できる *****
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/users/normal", "", adminToken, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete specified user: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 管理者が自分自身を削除しても、残ったユーザーに管理者権限が再付与される *****
	remainingID := createTestUser(t, server.db, "remaining", "hunter2", false)
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/users/me", "", adminToken, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete admin me: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if err := server.db.QueryRow(`SELECT is_admin FROM users WHERE id = ?`, remainingID).Scan(&isAdmin); err != nil {
		t.Fatal(err)
	}
	if isAdmin != 1 {
		t.Error("remaining user should be promoted to admin")
	}
}

// TestAccountLinkCreateAndDelete は Twitter / Bluesky アカウント紐付けの作成・削除を検証する。
func TestAccountLinkCreateAndDelete(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	userID := createTestUser(t, server.db, "linker", "hunter2", false)
	token := issueTestToken(t, userID)

	// テスト用の Twitter / Bluesky アカウントを作成する
	twitterID := createTestTwitterAccountDB(t, server.db, userID)
	blueskyID := createTestBlueskyAccountDB(t, server.db, userID)

	// ***** 紐付けを作成できる *****
	body := fmt.Sprintf(`{"twitter_account_id":%d,"bluesky_account_id":%d}`, twitterID, blueskyID)
	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/users/me/account-links", body, token, "application/json")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create link: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var link accountLinkResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	if link.TwitterAccount.ID != twitterID || link.BlueskyAccount.ID != blueskyID {
		t.Errorf("unexpected link: %+v", link)
	}

	// ***** 同じ Twitter アカウントを再度紐付けようとすると 422 *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users/me/account-links", body, token, "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "already linked") {
		t.Errorf("duplicate link: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 他人のアカウントは紐付けできない (422) *****
	otherID := createTestUser(t, server.db, "other", "hunter2", false)
	otherTwitterID := createTestTwitterAccountDB(t, server.db, otherID)
	body = fmt.Sprintf(`{"twitter_account_id":%d,"bluesky_account_id":%d}`, otherTwitterID, blueskyID)
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/users/me/account-links", body, token, "application/json")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified Twitter account does not exist") {
		t.Errorf("other's twitter account: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 紐付けを削除できる *****
	recorder = doJSONRequest(t, handler, http.MethodDelete, fmt.Sprintf("/api/users/me/account-links/%d", link.ID), "", token, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete link: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 存在しない紐付けの削除は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodDelete, fmt.Sprintf("/api/users/me/account-links/%d", link.ID), "", token, "")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified account link does not exist") {
		t.Errorf("delete missing link: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestUserIconUpload はアイコン画像のアップロードを検証する。
func TestUserIconUpload(t *testing.T) {
	server, paths := newTestServer(t, "")
	handler := server.Handler()
	userID := createTestUser(t, server.db, "iconer", "hunter2", false)
	token := issueTestToken(t, userID)

	// ***** JPEG/PNG 以外は 422 *****
	request := newMultipartRequest(t, http.MethodPut, "/api/users/me/icon", "image", "test.txt", "text/plain", []byte("not an image"), token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Please upload JPEG or PNG image") {
		t.Fatalf("non-image upload: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// ***** 横長 PNG をアップロードすると 512x512 の PNG に変換されて保存される *****
	request = newMultipartRequest(t, http.MethodPut, "/api/users/me/icon", "image", "test.png", "image/png", makeTestPNG(t, 100, 60), token)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("icon upload: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	iconPath := filepath.Join(paths.DataDir, "account-icons", fmt.Sprintf("%02d.png", userID))
	file, err := os.Open(iconPath)
	if err != nil {
		t.Fatalf("icon file should be saved: %v", err)
	}
	defer func() { _ = file.Close() }()
	savedImage, err := png.Decode(file)
	if err != nil {
		t.Fatalf("saved icon should be a PNG: %v", err)
	}
	if savedImage.Bounds().Dx() != 512 || savedImage.Bounds().Dy() != 512 {
		t.Errorf("saved icon size = %dx%d, want 512x512", savedImage.Bounds().Dx(), savedImage.Bounds().Dy())
	}

	// ***** アイコン取得 API がアップロードした画像を返す *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/users/me/icon", "", token, "")
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "default-icon") {
		t.Errorf("icon should be the uploaded one: status = %d", recorder.Code)
	}
}

// newMultipartRequest はマルチパートフォームのリクエストを生成する。
func newMultipartRequest(t *testing.T, method string, path string, fieldName string, fileName string, contentType string, content []byte, token string) *http.Request {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, fileName))
	partHeader.Set("Content-Type", contentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, &buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

// makeTestPNG はテスト用の PNG 画像を生成する。
func makeTestPNG(t *testing.T, width int, height int) []byte {
	t.Helper()
	imageData := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			imageData.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, imageData); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
