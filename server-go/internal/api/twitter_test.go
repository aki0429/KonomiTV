package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/twitter"
)

// ----------------------------------------------------------------------------
// フェイク Backend
// ----------------------------------------------------------------------------

// fakeTwitterBackend は twitter.Backend のフェイク。事前に積んだレスポンスを順に返す。
type fakeTwitterBackend struct {
	account    *twitter.Account
	responses  []*twitter.RawResponse
	compose    []*twitter.ComposeResult
	invokes    []fakeTwitterCall
	composes   []twitter.ComposeRequest
	cookiesTxt string
	setupDone  bool
}

type fakeTwitterCall struct {
	endpoint  string
	variables *twitter.OrderedMap
}

func newFakeTwitterBackend(responses ...*twitter.RawResponse) *fakeTwitterBackend {
	return &fakeTwitterBackend{responses: responses, setupDone: true}
}

func (backend *fakeTwitterBackend) SetAccount(account *twitter.Account) { backend.account = account }
func (backend *fakeTwitterBackend) IsSetupComplete() bool               { return backend.setupDone }
func (backend *fakeTwitterBackend) IsBrowserProcessAlive() bool         { return true }
func (backend *fakeTwitterBackend) Setup(ctx context.Context) error {
	backend.setupDone = true
	return nil
}
func (backend *fakeTwitterBackend) Shutdown(ctx context.Context) error { return nil }
func (backend *fakeTwitterBackend) MarkForRestart(ctx context.Context) error {
	backend.setupDone = false
	return nil
}
func (backend *fakeTwitterBackend) InvokeGraphQLAPI(
	ctx context.Context, endpointName string, variables *twitter.OrderedMap, additionalFlags *twitter.OrderedMap,
) (*twitter.RawResponse, error) {
	backend.invokes = append(backend.invokes, fakeTwitterCall{endpoint: endpointName, variables: variables})
	if len(backend.responses) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	response := backend.responses[0]
	backend.responses = backend.responses[1:]
	return response, nil
}
func (backend *fakeTwitterBackend) PostTweetViaComposeUI(ctx context.Context, request twitter.ComposeRequest) (*twitter.ComposeResult, error) {
	backend.composes = append(backend.composes, request)
	if len(backend.compose) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	result := backend.compose[0]
	backend.compose = backend.compose[1:]
	return result, nil
}
func (backend *fakeTwitterBackend) SaveCookiesToNetscapeFormat(ctx context.Context) (string, error) {
	return backend.cookiesTxt, nil
}
func (backend *fakeTwitterBackend) CaptureDebugScreenshot(ctx context.Context, reason string) (string, error) {
	return "", nil
}

// rawResponseJSON は parsed_response / status_code を指定して RawResponse を生成する。
func rawResponseJSON(parsedResponse string, statusCode int) *twitter.RawResponse {
	var value any
	decoder := json.NewDecoder(strings.NewReader(parsedResponse))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	return &twitter.RawResponse{
		ParsedResponse: value,
		StatusCode:     &statusCode,
		Headers:        map[string]string{"content-type": "application/json"},
	}
}

// ----------------------------------------------------------------------------
// テスト用ヘルパー
// ----------------------------------------------------------------------------

// newTwitterTestServer は Twitter ルートを登録したハンドラーとトークンを返す。
func newTwitterTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	server, _ := newTestServer(t, "")
	userID := createTestUser(t, server.db, "twitter-user", "hunter2", true)
	token := issueTestToken(t, userID)

	mux := http.NewServeMux()
	server.registerTwitterRoutes(mux)

	t.Cleanup(func() {
		twitter.ResetInstances()
		newTwitterBackendFactory = twitter.NewUnavailableBackend
	})
	return server, mux, token
}

// insertTwitterAccount は Twitter アカウントのテストレコードを挿入する。
func insertTwitterAccount(t *testing.T, db *sql.DB, userID int64, screenName string, accessToken string, secret string) int64 {
	t.Helper()
	result, err := db.Exec(
		`INSERT INTO twitter_accounts (user_id, name, screen_name, icon_url, access_token, access_token_secret) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, "テスト", screenName, "https://example.com/icon.png", accessToken, secret,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// currentUserID はテストユーザーの ID を取得する。
func currentUserID(t *testing.T, server *Server, name string) int64 {
	t.Helper()
	var id int64
	if err := server.db.QueryRow(`SELECT id FROM users WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// ----------------------------------------------------------------------------
// バリデーション (FastAPI/Pydantic 互換の 422 ボディ)
// ----------------------------------------------------------------------------

// validationExpected は generate_validation_expected.py が生成した 422 期待値。
type validationExpected struct {
	Name         string          `json:"name"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Body         json.RawMessage `json:"body"`
	StatusCode   int             `json:"status_code"`
	ResponseJSON json.RawMessage `json:"response_json"`
}

// TestTwitterValidationErrors は FastAPI 互換の 422 ボディを再現できているか検証する。
func TestTwitterValidationErrors(t *testing.T) {
	path := filepath.Join("..", "twitter", "testdata", "twitter_validation_expected.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	var cases []validationExpected
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse %s: %v", path, err)
	}
	if len(cases) == 0 {
		t.Fatal("no validation fixtures")
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			server, handler, token := newTwitterTestServer(t)
			// Python の期待値ジェネレーター (generate_validation_expected.py) は
			// GetCurrentTwitterAccount 依存を必ず成功するフェイクに差し替えて検証エラーを採取している。
			// Go では認証済み + 有効な TwitterAccount が存在する状態がこれに相当するため、
			// screen_name "dummy" の NETSCAPE_COOKIE_FILE レコードを用意して依存を通過させる。
			userID := currentUserID(t, server, "twitter-user")
			insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")
			body := ""
			if len(testCase.Body) > 0 && string(testCase.Body) != "null" {
				body = string(testCase.Body)
			}
			recorder := doJSONRequest(t, handler, testCase.Method, testCase.Path, body, token, "application/json")
			if recorder.Code != testCase.StatusCode {
				t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, testCase.StatusCode, recorder.Body.String())
			}
			var actual any
			if err := json.Unmarshal(recorder.Body.Bytes(), &actual); err != nil {
				t.Fatalf("failed to parse response: %v (body: %s)", err, recorder.Body.String())
			}
			var expected any
			if err := json.Unmarshal(testCase.ResponseJSON, &expected); err != nil {
				t.Fatal(err)
			}
			if !deepEqualJSONAny(expected, actual) {
				expectedText, _ := json.Marshal(expected)
				actualText, _ := json.Marshal(actual)
				t.Fatalf("response mismatch\n expected: %s\n   actual: %s", expectedText, actualText)
			}
		})
	}
}

// deepEqualJSONAny は JSON ツリーを再帰的に比較する (数値は文字列表現で比較) 。
func deepEqualJSONAny(expected any, actual any) bool {
	switch expectedTyped := expected.(type) {
	case map[string]any:
		actualTyped, ok := actual.(map[string]any)
		if !ok || len(expectedTyped) != len(actualTyped) {
			return false
		}
		for key, value := range expectedTyped {
			other, exists := actualTyped[key]
			if !exists || !deepEqualJSONAny(value, other) {
				return false
			}
		}
		return true
	case []any:
		actualTyped, ok := actual.([]any)
		if !ok || len(expectedTyped) != len(actualTyped) {
			return false
		}
		for index := range expectedTyped {
			if !deepEqualJSONAny(expectedTyped[index], actualTyped[index]) {
				return false
			}
		}
		return true
	default:
		return expected == actual
	}
}

// ----------------------------------------------------------------------------
// エンドポイントの正常系 / アカウント取得
// ----------------------------------------------------------------------------

// TestTwitterRetweetEndpoints はリツイート / いいね系エンドポイントが GraphQL を呼ぶことを検証する。
func TestTwitterRetweetEndpoints(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	backend := newFakeTwitterBackend(
		rawResponseJSON(`{"data": {"create_retweet": {"retweet_results": {}}}}`, 200),
		rawResponseJSON(`{"data": {"unretweet": {"source_tweet_results": {}}}}`, 200),
		rawResponseJSON(`{"data": {"favorite_tweet": "Done"}}`, 200),
		rawResponseJSON(`{"data": {"unfavorite_tweet": "Done"}}`, 200),
	)
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	cases := []struct {
		method   string
		path     string
		detail   string
		endpoint string
	}{
		{http.MethodPut, "/api/twitter/accounts/dummy/tweets/123/retweet", "リツイートしました。", "CreateRetweet"},
		{http.MethodDelete, "/api/twitter/accounts/dummy/tweets/123/retweet", "リツイートを取り消ししました。", "DeleteRetweet"},
		{http.MethodPut, "/api/twitter/accounts/dummy/tweets/123/favorite", "いいねしました。", "FavoriteTweet"},
		{http.MethodDelete, "/api/twitter/accounts/dummy/tweets/123/favorite", "いいねを取り消しました。", "UnfavoriteTweet"},
	}
	for index, testCase := range cases {
		recorder := doJSONRequest(t, handler, testCase.method, testCase.path, "", token, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body: %s)", testCase.path, recorder.Code, recorder.Body.String())
		}
		var body twitter.TwitterAPIResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.IsSuccess || body.Detail != testCase.detail {
			t.Errorf("%s body = %+v", testCase.path, body)
		}
		if backend.invokes[index].endpoint != testCase.endpoint {
			t.Errorf("%s endpoint = %q, want %q", testCase.path, backend.invokes[index].endpoint, testCase.endpoint)
		}
	}
}

// TestTwitterTimelineEndpoint はタイムライン取得がレスポンスをそのまま返すことを検証する。
func TestTwitterTimelineEndpoint(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	timelineResponse := map[string]any{
		"data": map[string]any{
			"home": map[string]any{
				"home_timeline_urt": map[string]any{
					"instructions": []any{
						map[string]any{
							"type": "TimelineAddEntries",
							"entries": []any{
								map[string]any{
									"entryId": "tweet-1",
									"content": map[string]any{
										"entryType": "TimelineTimelineItem",
										"itemContent": map[string]any{
											"itemType": "TimelineTweet",
											"tweet_results": map[string]any{
												"result": map[string]any{
													"__typename": "Tweet",
													"rest_id":    "1001",
													"source":     "Twitter Web App",
													"legacy": map[string]any{
														"id_str": "1001", "created_at": "Wed Oct 07 12:00:00 +0000 2026",
														"full_text": "こんにちは", "lang": "ja",
														"retweet_count": 1, "favorite_count": 2, "retweeted": false, "favorited": true,
													},
													"core": map[string]any{
														"user_results": map[string]any{
															"result": map[string]any{
																"rest_id": "111",
																"core":    map[string]any{"name": "テスト", "screen_name": "test"},
																"avatar":  map[string]any{"image_url": "https://example.com/a_normal.jpg"},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	timelineResponseJSON, err := json.Marshal(timelineResponse)
	if err != nil {
		t.Fatal(err)
	}
	backend := newFakeTwitterBackend(rawResponseJSON(string(timelineResponseJSON), 200))
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/accounts/dummy/timeline", "", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		IsSuccess bool `json:"is_success"`
		Tweets    []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"tweets"`
		IsCursorConsumed bool `json:"is_cursor_consumed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.IsSuccess || len(body.Tweets) != 1 || body.Tweets[0].ID != "1001" || body.Tweets[0].Text != "こんにちは" {
		t.Errorf("unexpected timeline body: %s", recorder.Body.String())
	}
	if !body.IsCursorConsumed {
		t.Error("is_cursor_consumed should be true")
	}
}

// TestTwitterTweetEndpoint はツイート送信 (multipart) が動作することを検証する。
func TestTwitterTweetEndpoint(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	backend := newFakeTwitterBackend()
	backend.compose = []*twitter.ComposeResult{{
		IsSuccess:        true,
		GraphQLAPIResult: rawResponseJSON(`{"data": {"create_tweet": {"tweet_results": {"result": {"rest_id": "999"}}}}}`, 200),
	}}
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	_ = writer.WriteField("tweet", "こんにちは")
	part, err := writer.CreateFormFile("images", "test.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("dummy-image-bytes"))
	_ = writer.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/twitter/accounts/dummy/tweets", &buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body twitter.PostTweetResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.IsSuccess || body.TweetID == nil || *body.TweetID != "999" {
		t.Errorf("unexpected tweet body: %s", recorder.Body.String())
	}
	if len(backend.composes) != 1 || backend.composes[0].TweetText != "こんにちは" || len(backend.composes[0].Images) != 1 {
		t.Errorf("unexpected compose call: %+v", backend.composes)
	}
}

// TestTwitterTweetTooManyImages は画像 5 枚で 422 になることを検証する。
func TestTwitterTweetTooManyImages(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	backend := newFakeTwitterBackend()
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for index := 0; index < 5; index++ {
		part, err := writer.CreateFormFile("images", "test.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("x"))
	}
	_ = writer.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/twitter/accounts/dummy/tweets", &buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
}

// TestTwitterAccountNotFound は存在しないスクリーンネームで 422 になることを検証する。
func TestTwitterAccountNotFound(t *testing.T) {
	_, handler, token := newTwitterTestServer(t)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/accounts/unknown/timeline", "", token, "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Detail != "TwitterAccount associated with screen_name does not exist" {
		t.Errorf("detail = %q", body.Detail)
	}
}

// TestTwitterOldFormatAccountRemoved は古い形式のレコードが削除され 422 になることを検証する。
func TestTwitterOldFormatAccountRemoved(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "old", "OAUTH_TOKEN", "secret")

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/accounts/old/timeline", "", token, "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body errorResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if body.Detail != "Old cookie format or OAuth session is no longer available" {
		t.Errorf("detail = %q", body.Detail)
	}
	var count int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM twitter_accounts WHERE access_token = 'OAUTH_TOKEN'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("old format account should be deleted, count = %d", count)
	}
}

// TestTwitterAccountDelete はアカウント連携解除で 204 とレコード削除を検証する。
func TestTwitterAccountDelete(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return newFakeTwitterBackend() }

	recorder := doJSONRequest(t, handler, http.MethodDelete, "/api/twitter/accounts/dummy", "", token, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM twitter_accounts WHERE screen_name = 'dummy'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("account should be deleted, count = %d", count)
	}
}

// TestTwitterCookiePersistedAfterCall は API 呼び出し後に Cookie が暗号化されて DB に保存されることを検証する。
func TestTwitterCookiePersistedAfterCall(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	accountID := insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	backend := newFakeTwitterBackend(rawResponseJSON(`{"data": {"favorite_tweet": "Done"}}`, 200))
	backend.cookiesTxt = "# Netscape HTTP Cookie File\n.x.com\tTRUE\t/\tTRUE\t0\tauth_token\tdummy"
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	recorder := doJSONRequest(t, handler, http.MethodPut, "/api/twitter/accounts/dummy/tweets/1/favorite", "", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}

	var secret string
	if err := server.db.QueryRow(`SELECT access_token_secret FROM twitter_accounts WHERE id = ?`, accountID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "enc:") {
		t.Fatalf("cookie should be encrypted with enc: prefix, got %q", secret)
	}
	// 復号すると元の Cookie に戻ることを確認する
	decrypted, err := server.decryptTwitterCookieForTest(secret)
	if err != nil {
		t.Fatalf("failed to decrypt cookie: %v", err)
	}
	if decrypted != backend.cookiesTxt {
		t.Errorf("decrypted cookie mismatch\n got: %q\nwant: %q", decrypted, backend.cookiesTxt)
	}
}

// TestTwitterVideoProxyValidation は動画プロキシの URL バリデーションを検証する。
func TestTwitterVideoProxyValidation(t *testing.T) {
	_, handler, token := newTwitterTestServer(t)

	cases := []struct {
		path   string
		status int
		detail string
	}{
		{"/api/twitter/video-proxy?url=http://video.twimg.com/a.mp4", http.StatusUnprocessableEntity, "URL scheme must be https."},
		{"/api/twitter/video-proxy?url=https://evil.example.com/a.mp4", http.StatusUnprocessableEntity, "URL domain is not allowed. Only video.twimg.com, pbs.twimg.com are allowed."},
	}
	for _, testCase := range cases {
		recorder := doJSONRequest(t, handler, http.MethodGet, testCase.path, "", token, "")
		if recorder.Code != testCase.status {
			t.Fatalf("%s status = %d, want %d (body: %s)", testCase.path, recorder.Code, testCase.status, recorder.Body.String())
		}
		var body errorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Detail != testCase.detail {
			t.Errorf("%s detail = %q, want %q", testCase.path, body.Detail, testCase.detail)
		}
	}
}

// TestTwitterKeepAlive は keep-alive が 204 を返すことを検証する。
func TestTwitterKeepAlive(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return newFakeTwitterBackend() }

	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/twitter/accounts/dummy/keep-alive", "", token, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", recorder.Code, recorder.Body.String())
	}
}

// TestTwitterSearchEndpoint は検索が SearchTimeline を呼ぶことを検証する。
func TestTwitterSearchEndpoint(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	backend := newFakeTwitterBackend(rawResponseJSON(`{"data": {"search_by_raw_query": {"search_timeline": {"timeline": {"instructions": []}}}}}`, 200))
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { return backend }

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/accounts/dummy/search?query=%E6%96%B0%E5%9E%8B%E8%BB%8A", "", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if len(backend.invokes) != 1 || backend.invokes[0].endpoint != "SearchTimeline" {
		t.Fatalf("unexpected invokes: %+v", backend.invokes)
	}
	rawQuery, _ := backend.invokes[0].variables.Get("rawQuery")
	if rawQuery != "新型車 lang:ja -filter:replies" {
		t.Errorf("rawQuery = %v", rawQuery)
	}
}

// decryptTwitterCookieForTest はテスト用に Cookie を復号する。
func (s *Server) decryptTwitterCookieForTest(encrypted string) (string, error) {
	secret, err := auth.LoadOrCreateSecret(filepath.Join(s.paths.DataDir, "jwt_secret.dat"))
	if err != nil {
		return "", err
	}
	instance := twitter.NewFernetFromSecret(secret)
	plain, err := instance.Decrypt(strings.TrimPrefix(encrypted, twitterCookieEncryptionPrefix))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ----------------------------------------------------------------------------
// 認証依存とクエリ検証の評価順序 (FastAPI 互換)
// ----------------------------------------------------------------------------

// TestTwitterAuthCheckedBeforeQueryValidation は未認証時にクエリ検証エラー (422 配列) より
// 先に 401 "Not authenticated" が返ることを検証する。
//
// 実機 Python の実測:
//
//	GET /api/twitter/accounts/sample/search            → 401 {"detail": "Not authenticated"}
//	GET /api/twitter/accounts/sample/search?search_type=__invalid__ → 401 {"detail": "Not authenticated"}
//	GET /api/twitter/accounts/sample/timeline?cursor_type=__invalid__ → 401 {"detail": "Not authenticated"}
//
// FastAPI はエンドポイント自身のクエリ検証より先に依存 (GetCurrentTwitterAccount → GetCurrentUser) を
// 解決するため、未認証なら依存の 401 が確定しクエリ検証エラーは出力されない。
func TestTwitterAuthCheckedBeforeQueryValidation(t *testing.T) {
	_, handler, _ := newTwitterTestServer(t)
	paths := []string{
		"/api/twitter/accounts/sample/search",
		"/api/twitter/accounts/sample/search?search_type=__invalid__",
		"/api/twitter/accounts/sample/timeline?cursor_type=__invalid__",
	}
	for _, path := range paths {
		recorder := doJSONRequest(t, handler, http.MethodGet, path, "", "", "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401 (body: %s)", path, recorder.Code, recorder.Body.String())
		}
		var body errorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s failed to parse body: %v", path, err)
		}
		if body.Detail != "Not authenticated" {
			t.Errorf("%s detail = %q, want %q", path, body.Detail, "Not authenticated")
		}
	}
}

// TestTwitterTimelineValidationAfterAuth は認証済み (かつ有効なアカウントがある) 場合に
// クエリ検証エラー配列が返ることを検証する (認証を先に評価しても検証自体は生きていることの確認) 。
func TestTwitterTimelineValidationAfterAuth(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	userID := currentUserID(t, server, "twitter-user")
	insertTwitterAccount(t, server.db, userID, "dummy", "NETSCAPE_COOKIE_FILE", "enc:dummy")

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/accounts/dummy/timeline?cursor_type=Bad", "", token, "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	expected := map[string]any{
		"detail": []any{
			map[string]any{
				"type":  "literal_error",
				"loc":   []any{"query", "cursor_type"},
				"msg":   "Input should be 'Top', 'Bottom', 'Gap' or 'ShowMore'",
				"input": "Bad",
				"ctx":   map[string]any{"expected": "'Top', 'Bottom', 'Gap' or 'ShowMore'"},
			},
		},
	}
	var actual any
	if err := json.Unmarshal(recorder.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if !deepEqualJSONAny(expected, actual) {
		t.Errorf("body mismatch: %s", recorder.Body.String())
	}
}

// ----------------------------------------------------------------------------
// video-proxy の url パラメータ
// ----------------------------------------------------------------------------

// TestTwitterVideoProxyEmptyURL は url が空文字でも「指定あり」として受理され、
// ハンドラの scheme 検証で 422 "URL scheme must be https." になることを検証する。
//
// 実機 Python の実測: GET /api/twitter/video-proxy?url= → 422 {"detail": "URL scheme must be https."}
// (url は str 必須パラメータで、空文字も有効な値として受理される)
func TestTwitterVideoProxyEmptyURL(t *testing.T) {
	_, handler, _ := newTwitterTestServer(t)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/video-proxy?url=", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Detail != "URL scheme must be https." {
		t.Errorf("detail = %q, want %q", body.Detail, "URL scheme must be https.")
	}
}

// TestTwitterVideoProxyMissingURL は url 未指定時に missing の検証エラー配列を返す (負対照) 。
func TestTwitterVideoProxyMissingURL(t *testing.T) {
	_, handler, _ := newTwitterTestServer(t)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/twitter/video-proxy", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	expected := map[string]any{
		"detail": []any{
			map[string]any{
				"type":  "missing",
				"loc":   []any{"query", "url"},
				"msg":   "Field required",
				"input": nil,
			},
		},
	}
	var actual any
	if err := json.Unmarshal(recorder.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if !deepEqualJSONAny(expected, actual) {
		t.Errorf("body mismatch: %s", recorder.Body.String())
	}
}
