package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// pythonInterlacedClientSecretSHA256 は Python 版 Interlaced(3) の出力の SHA-256。
// (クライアントシークレット自体をテストコードに残さないためハッシュで比較する)
const pythonInterlacedClientSecretSHA256 = "af37e5f8da0aad8fb8c3174380b45a947af53a2b3f8a498794356a91ed0bedd0"

// copyJikkyoChannelsJSON は本物の static/jikkyo_channels.json と static/interlaced.dat を
// テスト用 static ディレクトリにコピーする。
func copyJikkyoChannelsJSON(t *testing.T, paths constants.Paths) {
	t.Helper()
	for _, fileName := range []string{"jikkyo_channels.json", "interlaced.dat"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "server", "static", fileName))
		if err != nil {
			t.Skipf("server/static/%s is not available: %v", fileName, err)
		}
		writeFile(t, filepath.Join(paths.StaticDir, fileName), string(raw))
	}
}

// formatOptionalString は *string をエラーメッセージ用に文字列化する。
func formatOptionalString(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *value)
}

// insertTestNiconicoUser はニコニコアカウントを連携済みのテスト用ユーザーを作成する。
func insertTestNiconicoUser(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO users (
			name, password, is_admin, client_settings,
			niconico_user_id, niconico_user_name, niconico_user_premium,
			niconico_access_token, niconico_refresh_token, created_at, updated_at
		) VALUES (?, 'unused', 0, '{}', 12345, 'テストニコニコ', 0, 'old-access-token', 'old-refresh-token', ?, ?)
	`, name, database.NowForDB(), database.NowForDB())
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestInterlacedClientSecretMatchesPython はクライアントシークレットの復号が Python 版と一致することを検証する。
func TestInterlacedClientSecretMatchesPython(t *testing.T) {
	staticDir := filepath.Join("..", "..", "..", "server", "static")
	secret, err := interlacedClientSecret(staticDir)
	if err != nil {
		t.Skipf("server/static/interlaced.dat is not available: %v", err)
	}
	sum := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(sum[:]) != pythonInterlacedClientSecretSHA256 {
		t.Errorf("client secret hash = %s, want %s", hex.EncodeToString(sum[:]), pythonInterlacedClientSecretSHA256)
	}
}

// TestParseNicoLiveProgramID はニコニコチャンネルのページ HTML からの番組 ID 抽出を検証する。
func TestParseNicoLiveProgramID(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		expected string
	}{
		{
			"live_now 内のリンク",
			`<html><body><div id="live_now"><a href="https://live.nicovideo.jp/watch/lv123456">番組</a></div></body></html>`,
			"lv123456",
		},
		{
			"属性の順序が異なる場合",
			`<html><body><div class="live" id="live_now"><a href="https://live.nicovideo.jp/watch/lv999">番組</a></div></body></html>`,
			"lv999",
		},
		{
			"live_now がない場合",
			`<html><body><a href="https://live.nicovideo.jp/watch/lv123456">番組</a></body></html>`,
			"",
		},
		{
			"live_now はあるがリンクがない場合",
			`<html><body><div id="live_now">放送中の番組はありません</div></body></html>`,
			"",
		},
		{
			"別サイトへのリンクしかない場合",
			`<html><body><div id="live_now"><a href="https://example.com/watch/lv123456">番組</a></div></body></html>`,
			"",
		},
	}
	for _, testCase := range cases {
		if actual := parseNicoLiveProgramID([]byte(testCase.body)); actual != testCase.expected {
			t.Errorf("%s: parseNicoLiveProgramID = %q, want %q", testCase.name, actual, testCase.expected)
		}
	}
}

// TestChannelJikkyoWithoutNiconicoChannel は実況チャンネルが存在しない場合の挙動を検証する。
func TestChannelJikkyoWithoutNiconicoChannel(t *testing.T) {
	server, paths := newTestServer(t, "")
	copyJikkyoChannelsJSON(t, paths)
	handler := server.Handler()

	// 実況チャンネルが存在しない CS のチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID6-SID5000", DisplayChannelID: "cs5000", NetworkID: 6, ServiceID: 5000,
		RemoconID: 1, ChannelNumber: "5000", Type: "CS", Name: "テスト CS", IsWatchable: true,
	})

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/cs5000/jikkyo", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response jikkyoWebSocketInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WatchSessionURL != nil || response.CommentSessionURL != nil || response.NicoLiveWatchSessionURL != nil {
		t.Errorf("URLs should be null: %+v", response)
	}
	// 実況チャンネルがない場合は NX-Jikkyo 固有と判定される (Python 版と同じ)
	if response.IsNXJikkyoExclusive != true {
		t.Errorf("is_nxjikkyo_exclusive = %v, want true", response.IsNXJikkyoExclusive)
	}

	// 存在しないチャンネルは 422
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr999/jikkyo", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown channel: status = %d", recorder.Code)
	}

	// IPTV の疑似チャンネルは空の情報 (is_nxjikkyo_exclusive は false)
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/IPTV-cs5000/jikkyo", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WatchSessionURL != nil || response.IsNXJikkyoExclusive != false {
		t.Errorf("IPTV channel: %+v", response)
	}
}

// TestChannelJikkyoNXOnly は NX-Jikkyo にのみ存在する実況チャンネルの挙動を検証する。
func TestChannelJikkyoNXOnly(t *testing.T) {
	server, paths := newTestServer(t, "")
	copyJikkyoChannelsJSON(t, paths)
	handler := server.Handler()

	// jk11 は NX-Jikkyo にのみ存在する (ニコニコチャンネル ID を持たない)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID24632", DisplayChannelID: "gr111", NetworkID: 32736, ServiceID: 24632,
		RemoconID: 1, ChannelNumber: "111", Type: "GR", Name: "テスト (NX のみ)", IsWatchable: true,
	})

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr111/jikkyo", "", "", "")
	var response jikkyoWebSocketInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WatchSessionURL == nil || *response.WatchSessionURL != "https://nx-jikkyo.tsukumijima.net/api/v1/channels/jk11/ws/watch" {
		t.Errorf("watch_session_url = %v", response.WatchSessionURL)
	}
	if response.CommentSessionURL == nil || !strings.HasSuffix(*response.CommentSessionURL, "/channels/jk11/ws/comment") {
		t.Errorf("comment_session_url = %v", response.CommentSessionURL)
	}
	if response.NicoLiveWatchSessionURL != nil || response.NicoLiveWatchSessionError != nil {
		t.Errorf("nicolive fields should be null: %+v", response)
	}
	if response.IsNXJikkyoExclusive != true {
		t.Errorf("is_nxjikkyo_exclusive = %v, want true", response.IsNXJikkyoExclusive)
	}
}

// TestChannelJikkyoNicoLive はニコ生の視聴セッション URL を取得する一連の流れを検証する。
func TestChannelJikkyoNicoLive(t *testing.T) {
	var wsendpointAuthorization string
	wsendpointRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/live"):
			// ニコニコチャンネルのページ (現在放送中の番組のリンクを含む)
			_, _ = w.Write([]byte(`<html><body><div id="live_now"><a href="https://live.nicovideo.jp/watch/lv555">番組</a></div></body></html>`))
		case r.URL.Path == "/wsendpoint":
			wsendpointRequests++
			wsendpointAuthorization = r.Header.Get("Authorization")
			// 1回目はアクセストークンの有効期限切れ (401) 、2回目は成功する
			if wsendpointRequests == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"meta": {"errorCode": "UNAUTHORIZED"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data": {"url": "wss://api.live2.nicovideo.jp/websocket/example"}}`))
		case r.URL.Path == "/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token": "new-access-token", "refresh_token": "new-refresh-token"}`))
		case strings.HasPrefix(r.URL.Path, "/users/"):
			if r.Header.Get("X-Frontend-Id") != "6" {
				t.Errorf("X-Frontend-Id = %q, want 6", r.Header.Get("X-Frontend-Id"))
			}
			_, _ = w.Write([]byte(`{"data": {"user": {"nickname": "新しい名前", "isPremium": true}}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	overrideNiconicoURLs(t, server.URL)

	apiServer, paths := newTestServer(t, "")
	copyJikkyoChannelsJSON(t, paths)
	insertTestChannel(t, apiServer.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	userID := insertTestNiconicoUser(t, apiServer.db, "niconico-user")
	token := issueTestToken(t, userID)

	recorder := doJSONRequest(t, apiServer.Handler(), http.MethodGet, "/api/channels/gr011/jikkyo", "", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response jikkyoWebSocketInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.NicoLiveWatchSessionURL == nil || *response.NicoLiveWatchSessionURL != "wss://api.live2.nicovideo.jp/websocket/example" {
		t.Fatalf("nicolive_watch_session_url = %s (error: %s)", formatOptionalString(response.NicoLiveWatchSessionURL), formatOptionalString(response.NicoLiveWatchSessionError))
	}
	if response.NicoLiveWatchSessionError != nil {
		t.Errorf("nicolive_watch_session_error = %s", *response.NicoLiveWatchSessionError)
	}
	// リフレッシュ後のアクセストークンで再試行されている
	if wsendpointRequests != 2 {
		t.Errorf("wsendpoint requests = %d, want 2", wsendpointRequests)
	}
	if wsendpointAuthorization != "Bearer new-access-token" {
		t.Errorf("Authorization = %q, want the refreshed token", wsendpointAuthorization)
	}

	// リフレッシュ結果が DB に保存されている
	var (
		accessToken  string
		refreshToken string
		userName     string
		isPremium    int
	)
	if err := apiServer.db.QueryRow(
		`SELECT niconico_access_token, niconico_refresh_token, niconico_user_name, niconico_user_premium FROM users WHERE id = ?`,
		userID,
	).Scan(&accessToken, &refreshToken, &userName, &isPremium); err != nil {
		t.Fatal(err)
	}
	if accessToken != "new-access-token" || refreshToken != "new-refresh-token" {
		t.Errorf("tokens = %q / %q", accessToken, refreshToken)
	}
	if userName != "新しい名前" || isPremium != 1 {
		t.Errorf("user info = %q / %d", userName, isPremium)
	}
}

// TestChannelJikkyoNicoLiveErrors はニコ生の視聴セッション URL を取得できなかった場合の挙動を検証する。
func TestChannelJikkyoNicoLiveErrors(t *testing.T) {
	cases := []struct {
		name          string
		handler       http.HandlerFunc
		expectedError string
	}{
		{
			"番組が見つからない場合",
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/live") {
					_, _ = w.Write([]byte(`<html><body><div id="live_now">放送中の番組はありません</div></body></html>`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			},
			"現在放送中のニコニコ実況番組が見つかりませんでした。",
		},
		{
			"wsendpoint がエラーを返した場合",
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/live") {
					_, _ = w.Write([]byte(`<div id="live_now"><a href="https://live.nicovideo.jp/watch/lv555">番組</a></div>`))
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"meta": {"errorCode": "INVALID_PARAMETER"}}`))
			},
			"現在、ニコニコ生放送でエラーが発生しています。(HTTP Error 403 (INVALID_PARAMETER))",
		},
		{
			"アクセストークンの更新に失敗した場合",
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/live") {
					_, _ = w.Write([]byte(`<div id="live_now"><a href="https://live.nicovideo.jp/watch/lv555">番組</a></div>`))
					return
				}
				if r.URL.Path == "/wsendpoint" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
			},
			"アクセストークンの更新に失敗しました。(HTTP Error 400 (invalid_grant))",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fakeServer := httptest.NewServer(testCase.handler)
			defer fakeServer.Close()
			overrideNiconicoURLs(t, fakeServer.URL)

			apiServer, paths := newTestServer(t, "")
			copyJikkyoChannelsJSON(t, paths)
			insertTestChannel(t, apiServer.db, testChannel{
				ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
				RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
			})
			userID := insertTestNiconicoUser(t, apiServer.db, "niconico-user")
			token := issueTestToken(t, userID)

			recorder := doJSONRequest(t, apiServer.Handler(), http.MethodGet, "/api/channels/gr011/jikkyo", "", token, "")
			var response jikkyoWebSocketInfoResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.NicoLiveWatchSessionURL != nil {
				t.Errorf("nicolive_watch_session_url = %s, want nil", *response.NicoLiveWatchSessionURL)
			}
			if response.NicoLiveWatchSessionError == nil || *response.NicoLiveWatchSessionError != testCase.expectedError {
				t.Errorf("nicolive_watch_session_error = %s, want %q", formatOptionalString(response.NicoLiveWatchSessionError), testCase.expectedError)
			}
			// NX-Jikkyo の URL は常に返る
			if response.WatchSessionURL == nil || response.CommentSessionURL == nil {
				t.Errorf("NX-Jikkyo URLs should not be null: %+v", response)
			}
		})
	}
}

// TestChannelJikkyoWithoutLogin は未ログインの場合はニコ生の URL を取得しないことを検証する。
func TestChannelJikkyoWithoutLogin(t *testing.T) {
	server, paths := newTestServer(t, "")
	copyJikkyoChannelsJSON(t, paths)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	// ネットワークアクセスが発生しないよう、不正な URL に差し替えておく
	overrideNiconicoURLs(t, "http://127.0.0.1:1")

	recorder := doJSONRequest(t, server.Handler(), http.MethodGet, "/api/channels/gr011/jikkyo", "", "", "")
	var response jikkyoWebSocketInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WatchSessionURL == nil || !strings.HasSuffix(*response.WatchSessionURL, "/channels/jk1/ws/watch") {
		t.Errorf("watch_session_url = %v", response.WatchSessionURL)
	}
	if response.NicoLiveWatchSessionURL != nil || response.NicoLiveWatchSessionError != nil {
		t.Errorf("nicolive fields should be null: %+v", response)
	}
	if response.IsNXJikkyoExclusive != false {
		t.Errorf("is_nxjikkyo_exclusive = %v, want false", response.IsNXJikkyoExclusive)
	}
}

// TestChannelJikkyoNicoLiveNetworkError はニコ生に接続できない場合の挙動を検証する。
func TestChannelJikkyoNicoLiveNetworkError(t *testing.T) {
	apiServer, paths := newTestServer(t, "")
	copyJikkyoChannelsJSON(t, paths)
	insertTestChannel(t, apiServer.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	userID := insertTestNiconicoUser(t, apiServer.db, "niconico-user")
	token := issueTestToken(t, userID)

	// 接続できないポートに向ける
	overrideNiconicoURLs(t, "http://127.0.0.1:1")

	recorder := doJSONRequest(t, apiServer.Handler(), http.MethodGet, "/api/channels/gr011/jikkyo", "", token, "")
	var response jikkyoWebSocketInfoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	expected := "ニコニコ生放送に接続できませんでした。ニコニコで障害が発生している可能性があります。"
	if response.NicoLiveWatchSessionError == nil || *response.NicoLiveWatchSessionError != expected {
		t.Errorf("nicolive_watch_session_error = %s, want %q", formatOptionalString(response.NicoLiveWatchSessionError), expected)
	}
}

// overrideNiconicoURLs はニコニコ関連の URL をテスト用サーバーに向け、テスト終了時に元に戻す。
func overrideNiconicoURLs(t *testing.T, baseURL string) {
	t.Helper()
	originalChannelPageURLPattern := niconicoChannelPageURLPattern
	originalWSEndpointURL := nicoliveWSEndpointURL
	originalTokenURL := niconicoTokenURL
	originalUserAPIURLPattern := niconicoUserAPIURLPattern
	niconicoChannelPageURLPattern = baseURL + "/%s/live"
	nicoliveWSEndpointURL = baseURL + "/wsendpoint"
	niconicoTokenURL = baseURL + "/oauth2/token"
	niconicoUserAPIURLPattern = baseURL + "/users/%d"
	t.Cleanup(func() {
		niconicoChannelPageURLPattern = originalChannelPageURLPattern
		nicoliveWSEndpointURL = originalWSEndpointURL
		niconicoTokenURL = originalTokenURL
		niconicoUserAPIURLPattern = originalUserAPIURLPattern
	})
}
