package bluesky

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // SQLite ドライバー (テスト用の一時データベース)
)

// clientScenario は testdata/generate_bluesky_fixture.py が記録した 1 シナリオ。
type clientScenario struct {
	Name          string `json:"name"`
	SessionString string `json:"session_string"`
	Image         *struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
	} `json:"image"`
	Operation     map[string]any     `json:"operation"`
	Responses     []scriptedResponse `json:"responses"`
	Result        any                `json:"result"`
	Requests      []scriptedRequest  `json:"requests"`
	SavedSessions []string           `json:"saved_session_strings"`
}

// scriptedResponse はモックする XRPC レスポンス。
type scriptedResponse struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body"`
}

// scriptedRequest は Python 版が実際に送信したリクエストの記録。
type scriptedRequest struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Query         string `json:"query"`
	ContentType   string `json:"content_type"`
	Authorization string `json:"authorization"`
	BodySHA256    string `json:"body_sha256"`
	Body          any    `json:"body"`
}

// clientFixture は client_scenarios を含むフィクスチャ。
type clientFixture struct {
	ClientScenarios []clientScenario `json:"client_scenarios"`
}

func loadClientFixture(t *testing.T) clientFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "bluesky_fixture.json"))
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	var value clientFixture
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	if len(value.ClientScenarios) == 0 {
		t.Fatal("fixture has no client_scenarios")
	}
	return value
}

// recordedRequest はスクリプト輸送が受け取ったリクエスト。
type recordedRequest struct {
	method        string
	path          string
	query         string
	contentType   string
	authorization string
	body          []byte
}

// scriptedTransport はフィクスチャに記録されたレスポンスを順番に返す http.RoundTripper。
// 実際のネットワークへは一切アクセスしない。
type scriptedTransport struct {
	t         *testing.T
	responses []scriptedResponse
	index     int
	recorded  []recordedRequest
}

func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	var err error
	if request.Body != nil {
		body, err = io.ReadAll(request.Body)
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	s.recorded = append(s.recorded, recordedRequest{
		method:        request.Method,
		path:          request.URL.Path,
		query:         request.URL.RawQuery,
		contentType:   request.Header.Get("Content-Type"),
		authorization: request.Header.Get("Authorization"),
		body:          body,
	})

	if s.index >= len(s.responses) {
		return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
	}
	expected := s.responses[s.index]
	s.index++
	if expected.Method != request.Method || expected.Path != request.URL.Path {
		s.t.Errorf("scripted response %d = %s %s, but got %s %s",
			s.index-1, expected.Method, expected.Path, request.Method, request.URL.Path)
	}

	var encoded []byte
	if expected.Body == nil {
		encoded = []byte("null")
	} else {
		encoded, err = json.Marshal(expected.Body)
		if err != nil {
			return nil, err
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
		Request:    request,
	}, nil
}

// newClientScenarioDatabase はセッション保存用の最小データベースを生成する。
func newClientScenarioDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(t.TempDir())+"/test.sqlite")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE bluesky_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
			user_id INT NOT NULL,
			did TEXT NOT NULL,
			handle TEXT NOT NULL,
			name TEXT NOT NULL,
			icon_url TEXT NOT NULL,
			session_string TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	return db
}

// normalizeFixtureValue はフィクスチャ生成側 (_normalize_json) と同じ正規化を行う。
// null のキーは一部を除いて落とされるため、Go 側の出力にも同じ正規化を適用して比較する。
var nullKeyAllowlist = map[string]bool{
	"tweet_id": true, "post_uri": true, "post_cid": true, "movie_url": true,
	"retweeted_tweet": true, "quoted_tweet": true, "newer_cursor_id": true, "image_urls": true,
}

func normalizeFixtureValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := map[string]any{}
		for key, child := range typed {
			if child == nil && nullKeyAllowlist[key] == false {
				continue
			}
			result[key] = normalizeFixtureValue(child)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			result = append(result, normalizeFixtureValue(child))
		}
		return result
	default:
		return value
	}
}

// TestClientScenariosMatchPython は 9 EP の API 操作が Python 版と同じリクエストを送り、
// 同じ結果を返すことを検証する (HTTP はフィクスチャの録画済みレスポンスで置き換える) 。
func TestClientScenariosMatchPython(t *testing.T) {
	fixture := loadClientFixture(t)
	for _, scenario := range fixture.ClientScenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			transport := &scriptedTransport{t: t, responses: scenario.Responses}
			manager := NewManager(ManagerOptions{
				DB:         newClientScenarioDatabase(t),
				HTTPClient: &http.Client{Transport: transport},
			})
			// Handle は Python 版フィクスチャの BlueskyAccount スタブに合わせて空にしている
			// (フィクスチャの tweet_url が "https://bsky.app/profile//post/..." になっているため) 。
			// 本番ではデータベースに保存された handle が入る。
			account := &Account{
				ID:            1,
				UserID:        1,
				DID:           "did:plc:test",
				Handle:        "",
				SessionString: scenario.SessionString,
			}
			service := manager.Service(account)

			// 投稿レコードの createdAt と、アクセストークンの有効期限判定は実行時刻に依存するため、
			// フィクスチャ生成時と同じ時刻に固定する (固定しないと Go 版がトークンを期限切れと見なし、
			// Python 版には無いセッション更新リクエストを送ってしまう) 。
			restoreNow := setNowForTestFromScenario(t, scenario)
			defer restoreNow()
			service.client.SetNowForTest(scenarioClockFor(t, scenario))

			result := runClientScenario(t, service, scenario)

			// 結果の比較 (フィクスチャ側の正規化に合わせる)
			got := normalizeFixtureValue(toJSONValue(t, result))
			want := normalizeFixtureValue(scenario.Result)
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Errorf("result mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}

			// リクエストの比較
			if len(transport.recorded) != len(scenario.Requests) {
				t.Fatalf("requests = %d, want %d (%+v)",
					len(transport.recorded), len(scenario.Requests), transport.recorded)
			}
			for index, expected := range scenario.Requests {
				compareScriptedRequest(t, index, expected, transport.recorded[index])
			}

			// セッションの再保存は発生しない (フィクスチャも 0 件)
			if len(scenario.SavedSessions) != 0 {
				t.Errorf("fixture expects %d saved session strings, but the test does not verify them",
					len(scenario.SavedSessions))
			}
		})
	}
}

// setNowForTestFromScenario はフィクスチャが記録した createdAt へ現在時刻を固定する。
func setNowForTestFromScenario(t *testing.T, scenario clientScenario) func() {
	t.Helper()
	var recorded time.Time
	for _, request := range scenario.Requests {
		body, ok := request.Body.(map[string]any)
		if !ok {
			continue
		}
		record, ok := body["record"].(map[string]any)
		if !ok {
			continue
		}
		createdAt, ok := record["createdAt"].(string)
		if !ok {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			t.Fatalf("failed to parse recorded createdAt %q: %v", createdAt, err)
		}
		recorded = parsed
	}
	if recorded.IsZero() {
		return func() {}
	}
	previous := nowForTest
	nowForTest = func() time.Time { return recorded }
	return func() { nowForTest = previous }
}

// scenarioClockFor はフィクスチャ生成時の時刻 (アクセストークンの iat) を返す関数を返す。
// アクセストークンの有効期限判定だけに使う (createdAt は setNowForTestFromScenario が担当) 。
func scenarioClockFor(t *testing.T, scenario clientScenario) func() time.Time {
	t.Helper()
	fields := strings.Split(scenario.SessionString, ":::")
	if len(fields) < 3 {
		t.Fatalf("invalid session string in fixture: %q", scenario.SessionString)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		t.Fatalf("session access token is not a JWT: %q", fields[2])
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		t.Fatalf("failed to decode session access token: %v", err)
	}
	var claims struct {
		IssuedAt int64 `json:"iat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("failed to parse session access token: %v", err)
	}
	if claims.IssuedAt == 0 {
		t.Fatalf("session access token does not have iat: %q", fields[2])
	}
	clock := time.Unix(claims.IssuedAt, 0).UTC()
	return func() time.Time { return clock }
}

// runClientScenario はシナリオの操作を Service 経由で実行する。
func runClientScenario(t *testing.T, service *Service, scenario clientScenario) any {
	t.Helper()
	ctx := context.Background()
	operation := scenario.Operation
	operationType, _ := operation["type"].(string)
	cursorID := optionalScenarioString(operation["cursor_id"])

	var (
		result any
		err    error
	)
	switch operationType {
	case "home_latest_timeline":
		result, err = service.HomeLatestTimeline(ctx, cursorID)
	case "search_timeline":
		query, _ := operation["query"].(string)
		result, err = service.SearchTimeline(ctx, query, cursorID)
	case "create_post":
		text, _ := operation["text"].(string)
		images := []ImageInput{}
		if scenario.Image != nil && operation["with_image"] == true {
			data, decodeErr := base64.StdEncoding.DecodeString(scenario.Image.Data)
			if decodeErr != nil {
				t.Fatalf("failed to decode scenario image: %v", decodeErr)
			}
			images = append(images, ImageInput{Data: data, ContentType: scenario.Image.ContentType})
		}
		if count, ok := operation["with_image_count"].(float64); ok {
			for index := 0; index < int(count); index++ {
				images = append(images, ImageInput{Data: []byte("dummy"), ContentType: "image/png"})
			}
		}
		result, err = service.CreatePost(ctx, text, images, parseScenarioReplyReference(operation["reply_to"]))
	case "create_repost":
		postID, _ := operation["post_id"].(string)
		result, err = service.CreateRepost(ctx, postID)
	case "delete_repost":
		postID, _ := operation["post_id"].(string)
		result, err = service.DeleteRepost(ctx, postID)
	case "favorite_post":
		postID, _ := operation["post_id"].(string)
		result, err = service.FavoritePost(ctx, postID)
	case "unfavorite_post":
		postID, _ := operation["post_id"].(string)
		result, err = service.UnfavoritePost(ctx, postID)
	default:
		t.Fatalf("unsupported operation type: %q", operationType)
	}
	if err != nil {
		t.Fatalf("operation %s returned error: %v", operationType, err)
	}
	return result
}

func optionalScenarioString(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

func parseScenarioReplyReference(value any) *ReplyReference {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	reference := &ReplyReference{}
	if text, ok := raw["root_uri"].(string); ok {
		reference.RootURI = text
	}
	if text, ok := raw["root_cid"].(string); ok {
		reference.RootCID = text
	}
	if text, ok := raw["parent_uri"].(string); ok {
		reference.ParentURI = text
	}
	if text, ok := raw["parent_cid"].(string); ok {
		reference.ParentCID = text
	}
	return reference
}

// compareScriptedRequest は 1 件のリクエストをフィクスチャの記録と比較する。
func compareScriptedRequest(t *testing.T, index int, expected scriptedRequest, recorded recordedRequest) {
	t.Helper()
	if recorded.method != expected.Method || recorded.path != expected.Path {
		t.Errorf("request[%d] = %s %s, want %s %s",
			index, recorded.method, recorded.path, expected.Method, expected.Path)
	}

	// クエリパラメータは順序の違いを無視して比較する
	// (Python 版はパラメータ辞書の順序、Go 版は url.Values のソート順で送出する)
	gotQuery, err := url.ParseQuery(recorded.query)
	if err != nil {
		t.Fatalf("request[%d] failed to parse query: %v", index, err)
	}
	wantQuery, err := url.ParseQuery(expected.Query)
	if err != nil {
		t.Fatalf("request[%d] failed to parse expected query: %v", index, err)
	}
	if !reflect.DeepEqual(gotQuery, wantQuery) {
		t.Errorf("request[%d] query = %v, want %v", index, gotQuery, wantQuery)
	}

	if recorded.contentType != expected.ContentType {
		t.Errorf("request[%d] Content-Type = %q, want %q", index, recorded.contentType, expected.ContentType)
	}

	if recorded.authorization != expected.Authorization {
		t.Errorf("request[%d] Authorization = %q, want %q", index, recorded.authorization, expected.Authorization)
	}

	if expected.Body == nil {
		if len(recorded.body) != 0 {
			t.Errorf("request[%d] body = %q, want empty", index, string(recorded.body))
		}
		return
	}

	// blob アップロードはバイナリなので、フィクスチャは要約 (content_type/sha256/length) を記録している
	if summary, ok := expected.Body.(map[string]any); ok {
		if _, isBlob := summary["sha256"]; isBlob {
			digest := fmt.Sprintf("%x", sha256.Sum256(recorded.body))
			if digest != summary["sha256"] {
				t.Errorf("request[%d] blob sha256 = %s, want %v", index, digest, summary["sha256"])
			}
			if float64(len(recorded.body)) != summary["length"] {
				t.Errorf("request[%d] blob length = %d, want %v", index, len(recorded.body), summary["length"])
			}
			if summaryContentType, ok := summary["content_type"].(string); ok && recorded.contentType != summaryContentType {
				t.Errorf("request[%d] Content-Type = %q, want %q", index, recorded.contentType, summaryContentType)
			}
			return
		}
	}

	var got any
	if err := json.Unmarshal(recorded.body, &got); err != nil {
		t.Fatalf("request[%d] failed to parse body: %v (%q)", index, err, string(recorded.body))
	}
	if !reflect.DeepEqual(got, expected.Body) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(expected.Body)
		t.Errorf("request[%d] body mismatch\n got: %s\nwant: %s", index, gotJSON, wantJSON)
	}
}
