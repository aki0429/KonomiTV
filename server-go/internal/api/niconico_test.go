package api

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/niconico"
)

// niconicoFixturePath は Python 版 (server/app/routers/NiconicoRouter.py) から生成した期待値。
// 再生成: internal/niconico/testdata/generate_niconico_fixture.py
const niconicoFixturePath = "../niconico/testdata/niconico_fixture.json"

// testNiconicoClientSecret はテスト用のダミークライアントシークレット
// (フィクスチャ生成スクリプトと同じ値。実値は使わない) 。
const testNiconicoClientSecret = "test-client-secret-for-server-go"

// niconicoFixture は generate_niconico_fixture.py の出力形式。
type niconicoFixture struct {
	AuthURLCases []struct {
		Name           string  `json:"name"`
		Host           string  `json:"host"`
		Origin         *string `json:"origin"`
		Authorization  *string `json:"authorization"`
		ExpectedNetloc string  `json:"expected_netloc"`
		Expected       struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"expected"`
	} `json:"auth_url_cases"`
	CallbackCases            []niconicoCallbackCase `json:"callback_cases"`
	CallbackInvalidTokenCase struct {
		Name               string  `json:"name"`
		Client             string  `json:"client"`
		UserAccessToken    string  `json:"user_access_token"`
		Code               *string `json:"code"`
		Error              *string `json:"error"`
		ExpectedException  string  `json:"expected_exception_type"`
		ExpectedStatusCode int     `json:"expected_status_code"`
	} `json:"callback_invalid_token_case"`
	MissingParamCases []struct {
		Name                string            `json:"name"`
		Query               map[string]string `json:"query"`
		ExpectedStatusCode  int               `json:"expected_status_code"`
		ExpectedContentType string            `json:"expected_content_type"`
		ExpectedBody        map[string]any    `json:"expected_body"`
	} `json:"missing_param_cases"`
	LogoutCases []struct {
		ExpectedUser niconicoFixtureUser `json:"expected_user"`
	} `json:"logout_cases"`
}

// niconicoFixtureUser は Python 版 User モデルのニコニコ連携カラム。
type niconicoFixtureUser struct {
	NiconicoUserID       *int64  `json:"niconico_user_id"`
	NiconicoUserName     *string `json:"niconico_user_name"`
	NiconicoUserPremium  *bool   `json:"niconico_user_premium"`
	NiconicoAccessToken  *string `json:"niconico_access_token"`
	NiconicoRefreshToken *string `json:"niconico_refresh_token"`
	// SaveCalls は Python 版の save() 呼び出し回数 (Go 版では DB の状態で検証する) 。
	SaveCalls int `json:"save_calls"`
}

type niconicoCallbackCase struct {
	Name              string         `json:"name"`
	Client            string         `json:"client"`
	UserAccessToken   string         `json:"user_access_token"`
	Code              *string        `json:"code"`
	Error             *string        `json:"error"`
	TokenStatus       *int           `json:"token_status"`
	TokenBody         map[string]any `json:"token_body"`
	UserStatus        *int           `json:"user_status"`
	UserBody          map[string]any `json:"user_body"`
	TokenNetworkError bool           `json:"token_network_error"`
	UserNetworkError  bool           `json:"user_network_error"`
	Expected          struct {
		StatusCode  int    `json:"status_code"`
		ContentType string `json:"content_type"`
		BodyHTML    string `json:"body_html"`
		BodySHA256  string `json:"body_sha256"`
	} `json:"expected"`
	ExpectedRequests []niconicoExpectedRequest `json:"expected_requests"`
	ExpectedUser     *niconicoFixtureUser      `json:"expected_user"`
}

type niconicoExpectedRequest struct {
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	UserAgent   *string           `json:"user_agent"`
	XFrontendID *string           `json:"x_frontend_id"`
	Form        map[string]string `json:"form"`
}

// niconicoMockRequest はモックサーバーが受け取ったリクエストの記録。
type niconicoMockRequest struct {
	Method      string
	Path        string
	UserAgent   string
	XFrontendID string
	Form        map[string]string
}

func loadNiconicoFixture(t *testing.T) niconicoFixture {
	t.Helper()
	raw, err := os.ReadFile(niconicoFixturePath)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	var value niconicoFixture
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	return value
}

// niconicoAccountRow は users テーブルのニコニコ連携カラムの値。
type niconicoAccountRow struct {
	NiconicoUserID       *int64
	NiconicoUserName     *string
	NiconicoUserPremium  *bool
	NiconicoAccessToken  *string
	NiconicoRefreshToken *string
}

func readNiconicoAccount(t *testing.T, db *sql.DB, userID int64) niconicoAccountRow {
	t.Helper()
	var (
		row                  niconicoAccountRow
		niconicoUserID       sql.NullInt64
		niconicoUserName     sql.NullString
		niconicoUserPremium  sql.NullInt64
		niconicoAccessToken  sql.NullString
		niconicoRefreshToken sql.NullString
	)
	err := db.QueryRow(`
		SELECT niconico_user_id, niconico_user_name, niconico_user_premium,
			niconico_access_token, niconico_refresh_token
		FROM users WHERE id = ?
	`, userID).Scan(
		&niconicoUserID, &niconicoUserName, &niconicoUserPremium,
		&niconicoAccessToken, &niconicoRefreshToken,
	)
	if err != nil {
		t.Fatalf("failed to read niconico account: %v", err)
	}
	if niconicoUserID.Valid {
		row.NiconicoUserID = &niconicoUserID.Int64
	}
	if niconicoUserName.Valid {
		row.NiconicoUserName = &niconicoUserName.String
	}
	if niconicoUserPremium.Valid {
		value := niconicoUserPremium.Int64 != 0
		row.NiconicoUserPremium = &value
	}
	if niconicoAccessToken.Valid {
		row.NiconicoAccessToken = &niconicoAccessToken.String
	}
	if niconicoRefreshToken.Valid {
		row.NiconicoRefreshToken = &niconicoRefreshToken.String
	}
	return row
}

// niconicoMux はニコニコ関連 API のみを登録した mux を返す。
func niconicoMux(server *Server) *http.ServeMux {
	mux := http.NewServeMux()
	server.registerNiconicoRoutes(mux)
	return mux
}

func intValueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// TestNiconicoAuthURLAPI は GET /api/niconico/auth が Python 版と同じ認証 URL を返すことを検証する。
func TestNiconicoAuthURLAPI(t *testing.T) {
	fixture := loadNiconicoFixture(t)
	if len(fixture.AuthURLCases) == 0 {
		t.Fatal("fixture has no auth_url_cases")
	}
	for _, testCase := range fixture.AuthURLCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			userID := createTestUser(t, server.db, "niconico-user", "password", false)
			token := issueTestToken(t, userID)

			request := httptest.NewRequest(http.MethodGet, "/api/niconico/auth", nil)
			// Python 版 request.url.netloc は Host ヘッダーの値になる
			request.Host = testCase.Host
			if testCase.Origin != nil {
				request.Header.Set("Origin", *testCase.Origin)
			}

			// Authorization ヘッダーなしのケースは Go 版では認証エラーになる
			// (Python 版も Depends(GetCurrentUser) で 401 になるため、URL は生成されない) 。
			if testCase.Authorization == nil {
				recorder := httptest.NewRecorder()
				niconicoMux(server).ServeHTTP(recorder, request)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", recorder.Code)
				}
				// 認証なしでも認証 URL 自体は Python 版と同じであることを純粋関数レベルで確認している
				// (internal/niconico/niconico_test.go の TestBuildAuthorizationURLMatchesPython) 。
				return
			}

			// フィクスチャのアクセストークンは Python 版のダミー値なので、Go 版が発行したトークンを使う
			request.Header.Set("Authorization", "Bearer "+token)

			recorder := httptest.NewRecorder()
			niconicoMux(server).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}

			var body struct {
				AuthorizationURL string `json:"authorization_url"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}

			// フィクスチャの認証 URL は Python 版のダミー user_access_token を含むため、
			// デコードした state のトークン部分だけを Go 版が発行したものに差し替えて比較する
			gotStateBase64 := niconicoStateParam(t, body.AuthorizationURL)
			wantStateBase64 := niconicoStateParam(t, testCase.Expected.AuthorizationURL)
			_, fixtureToken := niconico.ParseAuthorizationSchemeParam(*testCase.Authorization)
			if fixtureToken == "" {
				t.Fatalf("fixture authorization header has no token: %q", *testCase.Authorization)
			}
			wantState := strings.Replace(niconicoDecodeState(t, wantStateBase64), fixtureToken, token, 1)
			if gotState := niconicoDecodeState(t, gotStateBase64); gotState != wantState {
				t.Errorf("state mismatch\n got: %s\nwant: %s", gotState, wantState)
			}

			// Python 版は Base64 のパディング (=) を削除する
			if strings.Contains(gotStateBase64, "=") {
				t.Errorf("state should not contain padding: %q", gotStateBase64)
			}

			// state 以外のクエリ部分が完全一致すること
			gotPrefix, _, _ := strings.Cut(body.AuthorizationURL, "state=")
			wantPrefix, _, _ := strings.Cut(testCase.Expected.AuthorizationURL, "state=")
			if gotPrefix != wantPrefix {
				t.Errorf("authorization_url (without state) mismatch\n got: %s\nwant: %s", gotPrefix, wantPrefix)
			}
		})
	}
}

// niconicoStateParam は認証 URL から state パラメータ (Base64) を取り出す。
func niconicoStateParam(t *testing.T, authorizationURL string) string {
	t.Helper()
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("failed to parse authorization_url: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatalf("state parameter not found in %q", authorizationURL)
	}
	return state
}

// niconicoDecodeState は Base64 (パディングなし) の state をデコードする。
func niconicoDecodeState(t *testing.T, value string) string {
	t.Helper()
	padding := strings.Repeat("=", (4-len(value)%4)%4)
	decoded, err := base64.StdEncoding.DecodeString(value + padding)
	if err != nil {
		t.Fatalf("failed to decode state %q: %v", value, err)
	}
	return string(decoded)
}

// TestNiconicoAuthCallbackAPI は GET /api/niconico/callback の挙動が Python 版と同じであることを検証する。
func TestNiconicoAuthCallbackAPI(t *testing.T) {
	fixture := loadNiconicoFixture(t)
	if len(fixture.CallbackCases) == 0 {
		t.Fatal("fixture has no callback_cases")
	}
	for _, testCase := range fixture.CallbackCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			userID := createTestUser(t, server.db, "niconico-user", "password", false)
			token := issueTestToken(t, userID)

			// クライアントシークレットは難読化データを使わずダミー値にする
			originalLoader := niconicoClientSecretLoader
			niconicoClientSecretLoader = func(string) (string, error) { return testNiconicoClientSecret, nil }
			t.Cleanup(func() { niconicoClientSecretLoader = originalLoader })

			recorded := []niconicoMockRequest{}
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				entry := niconicoMockRequest{
					Method:      r.Method,
					Path:        r.URL.Path,
					UserAgent:   r.Header.Get("User-Agent"),
					XFrontendID: r.Header.Get("X-Frontend-Id"),
				}
				if values, err := url.ParseQuery(string(body)); err == nil && len(values) > 0 {
					form := map[string]string{}
					for key := range values {
						form[key] = values.Get(key)
					}
					entry.Form = form
				}
				recorded = append(recorded, entry)

				if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(intValueOr(testCase.TokenStatus, http.StatusOK))
					if len(testCase.TokenBody) > 0 {
						_ = json.NewEncoder(w).Encode(testCase.TokenBody)
					}
					return
				}

				// ユーザー情報 API
				if testCase.UserNetworkError {
					// ステータス行とヘッダーだけを返してボディを途中で打ち切り、
					// ネットワークエラー (unexpected EOF) を再現する。
					// 接続をそのまま切断すると Go の http.Transport が冪等な GET を自動再送してしまうため、
					// ヘッダーを送信したうえでボディを打ち切る形にしている。
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("{"))
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(intValueOr(testCase.UserStatus, http.StatusOK))
				if len(testCase.UserBody) > 0 {
					_ = json.NewEncoder(w).Encode(testCase.UserBody)
				}
			}))
			t.Cleanup(mock.Close)

			originalTokenURL := niconicoTokenURL
			originalUserPattern := niconicoUserAPIURLPattern
			niconicoTokenURL = mock.URL + "/oauth2/token"
			niconicoUserAPIURLPattern = mock.URL + "/v1/users/%d"
			t.Cleanup(func() {
				niconicoTokenURL = originalTokenURL
				niconicoUserAPIURLPattern = originalUserPattern
			})

			// トークン API への接続エラーはモックサーバーを閉じて再現する
			// (Python 版の MockTransport と違い、ハンドラーに到達する前の接続失敗となるため
			//  リクエストは記録されない) 。
			if testCase.TokenNetworkError {
				mock.Close()
			}

			target := "/api/niconico/callback?client=" + url.QueryEscape(testCase.Client) +
				"&user_access_token=" + url.QueryEscape(token)
			if testCase.Code != nil {
				target += "&code=" + url.QueryEscape(*testCase.Code)
			}
			if testCase.Error != nil {
				target += "&error=" + url.QueryEscape(*testCase.Error)
			}

			recorder := httptest.NewRecorder()
			niconicoMux(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			// レスポンス
			if recorder.Code != testCase.Expected.StatusCode {
				t.Errorf("status = %d, want %d (body: %s)", recorder.Code, testCase.Expected.StatusCode, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != testCase.Expected.ContentType {
				t.Errorf("Content-Type = %q, want %q", contentType, testCase.Expected.ContentType)
			}
			if recorder.Body.String() != testCase.Expected.BodyHTML {
				t.Errorf("body mismatch\n got: %q\nwant: %q", recorder.Body.String(), testCase.Expected.BodyHTML)
			}

			// ニコニコ API へのリクエスト
			if testCase.TokenNetworkError {
				if len(recorded) != 0 {
					t.Errorf("recorded requests = %d, want 0 (接続失敗はモックサーバーに到達しない)", len(recorded))
				}
			} else {
				if len(recorded) != len(testCase.ExpectedRequests) {
					t.Fatalf("recorded requests = %d, want %d (%+v)", len(recorded), len(testCase.ExpectedRequests), recorded)
				}
				for index, expected := range testCase.ExpectedRequests {
					got := recorded[index]
					if got.Method != expected.Method || got.Path != expected.Path {
						t.Errorf("request[%d] = %s %s, want %s %s", index, got.Method, got.Path, expected.Method, expected.Path)
					}
					if expected.UserAgent != nil && got.UserAgent != *expected.UserAgent {
						t.Errorf("request[%d] User-Agent = %q, want %q", index, got.UserAgent, *expected.UserAgent)
					}
					if expected.XFrontendID != nil && got.XFrontendID != *expected.XFrontendID {
						t.Errorf("request[%d] X-Frontend-Id = %q, want %q", index, got.XFrontendID, *expected.XFrontendID)
					}
					if expected.Form != nil {
						form := map[string]string{}
						for key, value := range got.Form {
							form[key] = value
						}
						// クライアントシークレットはフィクスチャ側で置換されているため揃える
						if _, ok := form["client_secret"]; ok {
							form["client_secret"] = "<client_secret>"
						}
						if !reflect.DeepEqual(form, expected.Form) {
							t.Errorf("request[%d] form = %v, want %v", index, form, expected.Form)
						}
					}
				}
			}

			// データベースへの保存結果
			// Python 版の fixture の expected_user は save() 前後の「メモリ上のユーザーオブジェクト」を
			// 記録したものなので、save() が呼ばれていないケース (save_calls == 0) では
			// データベースには何も保存されていないことを検証する。
			row := readNiconicoAccount(t, server.db, userID)
			saved := testCase.ExpectedUser != nil && testCase.ExpectedUser.SaveCalls > 0
			if !saved {
				if row != (niconicoAccountRow{}) {
					t.Errorf("niconico account = %+v, want empty (Python 版の save_calls = 0)", row)
				}
				return
			}

			checkOptional(t, "niconico_user_id", row.NiconicoUserID, testCase.ExpectedUser.NiconicoUserID)
			checkOptional(t, "niconico_user_name", row.NiconicoUserName, testCase.ExpectedUser.NiconicoUserName)
			checkOptional(t, "niconico_user_premium", row.NiconicoUserPremium, testCase.ExpectedUser.NiconicoUserPremium)
			checkOptional(t, "niconico_access_token", row.NiconicoAccessToken, testCase.ExpectedUser.NiconicoAccessToken)
			checkOptional(t, "niconico_refresh_token", row.NiconicoRefreshToken, testCase.ExpectedUser.NiconicoRefreshToken)
		})
	}
}

// checkOptional はポインタ同士を値で比較する (どちらも nil なら一致とみなす) 。
func checkOptional[T comparable](t *testing.T, name string, got *T, want *T) {
	t.Helper()
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Errorf("%s = %s, want %s", name, formatOptional(got), formatOptional(want))
		return
	}
	if *got != *want {
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

// formatOptional はポインタの値を表示用の文字列にする (nil は "null") 。
func formatOptional[T comparable](value *T) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%v", *value)
}

// TestNiconicoAuthCallbackAPIInvalidToken は無効な user_access_token での挙動を検証する。
// Python 版は HTTPException の存在しない属性を参照して AttributeError になり 500 を返すため、
// Go 版もステータスコード 500 を返す (レスポンスボディの形式のみ Python 版と異なる) 。
func TestNiconicoAuthCallbackAPIInvalidToken(t *testing.T) {
	fixture := loadNiconicoFixture(t)
	testCase := fixture.CallbackInvalidTokenCase
	if testCase.ExpectedException != "AttributeError" {
		t.Fatalf("fixture assumption broken: expected_exception_type = %q", testCase.ExpectedException)
	}

	server, _ := newTestServer(t, "")
	target := "/api/niconico/callback?client=" + url.QueryEscape(testCase.Client) +
		"&user_access_token=" + url.QueryEscape(testCase.UserAccessToken)
	if testCase.Code != nil {
		target += "&code=" + url.QueryEscape(*testCase.Code)
	}
	if testCase.Error != nil {
		target += "&error=" + url.QueryEscape(*testCase.Error)
	}

	recorder := httptest.NewRecorder()
	niconicoMux(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	if recorder.Code != testCase.ExpectedStatusCode {
		t.Errorf("status = %d, want %d", recorder.Code, testCase.ExpectedStatusCode)
	}
}

// TestNiconicoAuthCallbackAPIMissingParams は必須クエリパラメータ欠落時の 422 を検証する。
func TestNiconicoAuthCallbackAPIMissingParams(t *testing.T) {
	fixture := loadNiconicoFixture(t)
	if len(fixture.MissingParamCases) == 0 {
		t.Fatal("fixture has no missing_param_cases")
	}
	for _, testCase := range fixture.MissingParamCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			server, _ := newTestServer(t, "")

			query := url.Values{}
			for key, value := range testCase.Query {
				query.Set(key, value)
			}
			target := "/api/niconico/callback"
			if len(query) > 0 {
				target += "?" + query.Encode()
			}

			recorder := httptest.NewRecorder()
			niconicoMux(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			if recorder.Code != testCase.ExpectedStatusCode {
				t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, testCase.ExpectedStatusCode, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != testCase.ExpectedContentType {
				t.Errorf("Content-Type = %q, want %q", contentType, testCase.ExpectedContentType)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}
			if !reflect.DeepEqual(body, testCase.ExpectedBody) {
				t.Errorf("body = %v, want %v", body, testCase.ExpectedBody)
			}
		})
	}
}

// TestNiconicoAccountLogoutAPI は DELETE /api/niconico/logout がニコニコ連携を解除することを検証する。
func TestNiconicoAccountLogoutAPI(t *testing.T) {
	fixture := loadNiconicoFixture(t)
	if len(fixture.LogoutCases) == 0 {
		t.Fatal("fixture has no logout_cases")
	}
	expected := fixture.LogoutCases[0].ExpectedUser

	server, _ := newTestServer(t, "")
	userID := createTestUser(t, server.db, "niconico-user", "password", false)
	token := issueTestToken(t, userID)

	// ニコニコ連携済みの状態にする (Python 版のフィクスチャと同じ値)
	userName := "テストユーザー"
	premium := true
	accessToken := "at"
	refreshToken := "rt"
	niconicoUserID := int64(12345)
	if err := niconico.SaveAccount(t.Context(), server.writeDB, userID, niconico.Account{
		NiconicoUserID:   &niconicoUserID,
		NiconicoUserName: &userName,
		UserPremium:      &premium,
		AccessToken:      &accessToken,
		RefreshToken:     &refreshToken,
	}); err != nil {
		t.Fatalf("failed to seed niconico account: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/niconico/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	niconicoMux(server).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", recorder.Body.String())
	}

	row := readNiconicoAccount(t, server.db, userID)
	if row != (niconicoAccountRow{}) {
		t.Errorf("niconico account = %+v, want all null", row)
	}
	if expected.NiconicoUserID != nil || expected.NiconicoUserName != nil ||
		expected.NiconicoUserPremium != nil || expected.NiconicoAccessToken != nil ||
		expected.NiconicoRefreshToken != nil {
		t.Errorf("fixture assumption broken: logout_cases expected_user should be all null")
	}
}
