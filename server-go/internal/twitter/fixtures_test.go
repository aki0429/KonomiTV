package twitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// ゴールデン期待値 (generate_expected.py が生成した twitter_expected.json) の読み込み
// ----------------------------------------------------------------------------

const expectedJSONName = "twitter_expected.json"

// expectedFixture は twitter_expected.json 全体。
type expectedFixture struct {
	QueryScenarios  []queryScenario  `json:"query_scenarios"`
	InvokeScenarios []invokeScenario `json:"invoke_scenarios"`
	TweetScenarios  []tweetScenario  `json:"tweet_scenarios"`
	JSSnippets      []jsSnippet      `json:"js_snippets"`
	Fernet          []fernetFixture  `json:"fernet"`
}

// queryScenario は fetchLoggedViewer / createRetweet / homeLatestTimeline / searchTimeline の 1 シナリオ。
type queryScenario struct {
	Name            string          `json:"name"`
	Method          string          `json:"method"`
	Kwargs          rawObject       `json:"kwargs"`
	Endpoint        string          `json:"endpoint"`
	Variables       rawObject       `json:"variables"`
	AdditionalFlags json.RawMessage `json:"additional_flags"`
	Raw             rawResponse     `json:"raw"`
	Result          json.RawMessage `json:"result"`
}

// invokeScenario は invokeGraphQLAPI レベルのエラー分岐の 1 シナリオ。
type invokeScenario struct {
	Name   string          `json:"name"`
	Method string          `json:"method"`
	Kwargs rawObject       `json:"kwargs"`
	Raw    rawResponse     `json:"raw"`
	Result json.RawMessage `json:"result"`
}

// tweetScenario は createTweet の 1 シナリオ。
type tweetScenario struct {
	Name              string          `json:"name"`
	Tweet             string          `json:"tweet"`
	Images            [][]string      `json:"images"`
	InReplyToStatusID *string         `json:"in_reply_to_status_id"`
	ComposeCall       json.RawMessage `json:"compose_call"`
	Raw               rawResponse     `json:"raw"`
	Result            json.RawMessage `json:"result"`
}

// jsSnippet は TwitterScrapeBrowser.invokeGraphQLAPI が CDP に渡す JS 式。
type jsSnippet struct {
	Endpoint        string          `json:"endpoint"`
	Variables       rawObject       `json:"variables"`
	AdditionalFlags json.RawMessage `json:"additional_flags"`
	JSCode          string          `json:"js_code"`
}

// fernetFixture は Python cryptography.fernet と相互運用するためのフィクスチャ。
type fernetFixture struct {
	Secret    string `json:"secret"`
	FernetKey string `json:"fernet_key"`
	PlainText string `json:"plain_text"`
	Timestamp int64  `json:"timestamp"`
	IVBase64  string `json:"iv_base64"`
	Token     string `json:"token"`
}

// rawObject はキー順を保持したまま生 JSON を読み込むための型。
type rawObject struct {
	Raw json.RawMessage
}

func (object *rawObject) UnmarshalJSON(data []byte) error {
	object.Raw = append([]byte(nil), data...)
	return nil
}

// rawResponse は FakeGraphQLResult 相当 (ブラウザが返す生レスポンス) 。
type rawResponse struct {
	ParsedResponse json.RawMessage   `json:"parsed_response"`
	StatusCode     *int              `json:"status_code"`
	ResponseText   *string           `json:"response_text"`
	Headers        map[string]string `json:"headers"`
	RequestError   string            `json:"request_error"`
}

// loadExpectedFixture は twitter_expected.json を読み込む。
func loadExpectedFixture(t *testing.T) *expectedFixture {
	t.Helper()
	path := filepath.Join("testdata", expectedJSONName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	fixture := &expectedFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatalf("failed to parse %s: %v", path, err)
	}
	if len(fixture.QueryScenarios) == 0 || len(fixture.InvokeScenarios) == 0 || len(fixture.TweetScenarios) == 0 {
		t.Fatalf("fixture is empty: query=%d invoke=%d tweet=%d",
			len(fixture.QueryScenarios), len(fixture.InvokeScenarios), len(fixture.TweetScenarios))
	}
	return fixture
}

// parseOrdered は生 JSON をキー順を保持した any (map は *OrderedMap) へデコードする。
func parseOrdered(t *testing.T, raw []byte) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseJSONValue(decoder)
	if err != nil {
		t.Fatalf("failed to parse JSON: %v (raw=%s)", err, string(raw))
	}
	return value
}

// parseJSONValue は json.Decoder のトークン列から *OrderedMap / []any / スカラーを組み立てる。
func parseJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		ordered := NewOrderedMap()
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := parseJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			ordered.Set(keyToken.(string), value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return ordered, nil
	case '[':
		items := []any{}
		for decoder.More() {
			value, err := parseJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return items, nil
	}
	return nil, errors.New("unexpected delimiter")
}

// pythonJSONOf は raw JSON を Python の json.dumps(value, ensure_ascii=False) 相当へ変換する
// (キー順は元の JSON のまま保持される) 。
func pythonJSONOf(t *testing.T, raw []byte) string {
	t.Helper()
	value := parseOrdered(t, raw)
	if value == nil {
		return "null"
	}
	encoded, err := PythonJSON(value)
	if err != nil {
		t.Fatalf("failed to encode ordered JSON: %v", err)
	}
	return string(encoded)
}

// jsonEqual は Go の値と期待値 JSON が (オブジェクトのキー順を無視して) 一致するか比較する。
func jsonEqual(t *testing.T, expectedRaw []byte, actual any) {
	t.Helper()
	actualBytes, err := json.Marshal(actual)
	if err != nil {
		t.Fatalf("failed to marshal actual value: %v", err)
	}
	expectedValue := decodeWithNumbers(t, expectedRaw)
	actualValue := decodeWithNumbers(t, actualBytes)
	if !deepEqualJSON(expectedValue, actualValue) {
		t.Fatalf("response mismatch\n expected: %s\n   actual: %s", string(expectedRaw), string(actualBytes))
	}
}

// decodeWithNumbers は JSON を json.Number 付きで any へデコードする。
func decodeWithNumbers(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("failed to decode JSON: %v (data=%s)", err, string(data))
	}
	return value
}

// deepEqualJSON は json.Number を含むツリーを再帰的に比較する。
func deepEqualJSON(a any, b any) bool {
	switch aTyped := a.(type) {
	case map[string]any:
		bTyped, ok := b.(map[string]any)
		if !ok || len(aTyped) != len(bTyped) {
			return false
		}
		for key, aValue := range aTyped {
			bValue, exists := bTyped[key]
			if !exists || !deepEqualJSON(aValue, bValue) {
				return false
			}
		}
		return true
	case []any:
		bTyped, ok := b.([]any)
		if !ok || len(aTyped) != len(bTyped) {
			return false
		}
		for index := range aTyped {
			if !deepEqualJSON(aTyped[index], bTyped[index]) {
				return false
			}
		}
		return true
	case json.Number:
		bTyped, ok := b.(json.Number)
		if !ok {
			return false
		}
		aFloat, aErr := aTyped.Float64()
		bFloat, bErr := bTyped.Float64()
		if aErr == nil && bErr == nil {
			return aFloat == bFloat
		}
		return aTyped.String() == bTyped.String()
	default:
		return a == b
	}
}

// ----------------------------------------------------------------------------
// フェイク Backend
// ----------------------------------------------------------------------------

// fakeBackend は Backend のフェイク実装。事前に積んだレスポンスを順に返す。
type fakeBackend struct {
	account    *Account
	responses  []*RawResponse
	setupError error

	isSetupComplete bool
	processAlive    bool
	setupCalls      int
	shutdownCalls   int
	markForRestart  int
	cookiesTxt      string
	composeResults  []*ComposeResult
	calls           []fakeCall
	composeCalls    []ComposeRequest
	screenshots     []string
}

// fakeCall は invokeGraphQLAPI の 1 回分の呼び出し。
type fakeCall struct {
	endpoint        string
	variables       *OrderedMap
	additionalFlags *OrderedMap
}

func newFakeBackend(responses ...*RawResponse) *fakeBackend {
	return &fakeBackend{
		isSetupComplete: true,
		processAlive:    true,
		responses:       responses,
	}
}

func (backend *fakeBackend) SetAccount(account *Account) { backend.account = account }

func (backend *fakeBackend) IsSetupComplete() bool { return backend.isSetupComplete }

func (backend *fakeBackend) IsBrowserProcessAlive() bool { return backend.processAlive }

func (backend *fakeBackend) Setup(ctx context.Context) error {
	backend.setupCalls++
	if backend.setupError != nil {
		return backend.setupError
	}
	backend.isSetupComplete = true
	return nil
}

func (backend *fakeBackend) Shutdown(ctx context.Context) error {
	backend.shutdownCalls++
	backend.isSetupComplete = false
	return nil
}

func (backend *fakeBackend) MarkForRestart(ctx context.Context) error {
	backend.markForRestart++
	backend.isSetupComplete = false
	return nil
}

func (backend *fakeBackend) InvokeGraphQLAPI(ctx context.Context, endpointName string, variables *OrderedMap, additionalFlags *OrderedMap) (*RawResponse, error) {
	backend.calls = append(backend.calls, fakeCall{endpoint: endpointName, variables: variables, additionalFlags: additionalFlags})
	if backend.setupError != nil {
		return nil, backend.setupError
	}
	if len(backend.responses) == 0 {
		return nil, errors.New("no fake response queued")
	}
	response := backend.responses[0]
	backend.responses = backend.responses[1:]
	if response == nil {
		return nil, errors.New("fake response is nil")
	}
	return response, nil
}

func (backend *fakeBackend) PostTweetViaComposeUI(ctx context.Context, request ComposeRequest) (*ComposeResult, error) {
	backend.composeCalls = append(backend.composeCalls, request)
	if len(backend.composeResults) == 0 {
		return nil, errors.New("no fake compose result queued")
	}
	result := backend.composeResults[0]
	backend.composeResults = backend.composeResults[1:]
	return result, nil
}

func (backend *fakeBackend) SaveCookiesToNetscapeFormat(ctx context.Context) (string, error) {
	return backend.cookiesTxt, nil
}

func (backend *fakeBackend) CaptureDebugScreenshot(ctx context.Context, reason string) (string, error) {
	backend.screenshots = append(backend.screenshots, reason)
	return "", nil
}

// newTestClient はフェイク Backend を持つテスト用クライアントを生成する。
func newTestClient(t *testing.T, backend *fakeBackend, account *Account) *GraphQLClient {
	t.Helper()
	if account == nil {
		account = &Account{ScreenName: "dummy_user"}
	}
	if backend.account == nil {
		backend.account = account
	}
	return NewDetachedClient(account, backend, nil)
}

// rawResponseFromFixture はフィクスチャの raw から RawResponse を組み立てる。
func rawResponseFromFixture(t *testing.T, raw rawResponse) *RawResponse {
	t.Helper()
	response := &RawResponse{
		StatusCode:   raw.StatusCode,
		ResponseText: raw.ResponseText,
		Headers:      raw.Headers,
		RequestError: raw.RequestError,
	}
	if len(raw.ParsedResponse) > 0 && string(raw.ParsedResponse) != "null" {
		response.ParsedResponse = decodeWithNumbers(t, raw.ParsedResponse)
	}
	return response
}

// pyJSONVariables は OrderedMap を Python の json.dumps 相当の文字列にする。
func pyJSONVariables(t *testing.T, object *OrderedMap) string {
	t.Helper()
	if object == nil {
		return "null"
	}
	encoded, err := PythonJSON(object)
	if err != nil {
		t.Fatalf("failed to encode variables: %v", err)
	}
	return string(encoded)
}

// mustContain は文字列に部分文字列が含まれることを確認する。
func mustContain(t *testing.T, haystack string, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}

// sha256Sum は Fernet の鍵導出 (Python 版の hashlib.sha256) と同じ計算を行う。
func sha256Sum(secret string) []byte {
	digest := sha256.Sum256([]byte(secret))
	return digest[:]
}

// stringOr は rawObject から文字列フィールドを取り出す (存在しない場合は既定値) 。
func stringOr(t *testing.T, object rawObject, key string, fallback string) string {
	t.Helper()
	value := decodeWithNumbers(t, object.Raw)
	objectMap, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("kwargs is not an object: %s", string(object.Raw))
	}
	rawItem, exists := objectMap[key]
	if !exists || rawItem == nil {
		return fallback
	}
	text, ok := rawItem.(string)
	if !ok {
		t.Fatalf("kwargs[%q] is not a string: %s", key, string(object.Raw))
	}
	return text
}

// stringValue は rawObject (JSON オブジェクト) から文字列フィールドを取り出す。
func stringValue(t *testing.T, object rawObject, key string) string {
	t.Helper()
	value := decodeWithNumbers(t, object.Raw)
	objectMap, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("kwargs is not an object: %s", string(object.Raw))
	}
	text, ok := objectMap[key].(string)
	if !ok {
		t.Fatalf("kwargs[%q] is not a string: %s", key, string(object.Raw))
	}
	return text
}

// stringSliceValue は rawObject から文字列配列フィールドを取り出す (存在しない場合は nil) 。
func stringSliceValue(t *testing.T, object rawObject, key string) []string {
	t.Helper()
	value := decodeWithNumbers(t, object.Raw)
	objectMap, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("kwargs is not an object: %s", string(object.Raw))
	}
	rawItems, exists := objectMap[key]
	if !exists || rawItems == nil {
		return nil
	}
	items, ok := rawItems.([]any)
	if !ok {
		t.Fatalf("kwargs[%q] is not an array: %s", key, string(object.Raw))
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("kwargs[%q] contains a non-string item: %s", key, string(object.Raw))
		}
		result = append(result, text)
	}
	return result
}

// optionalStringValue は rawObject から nullable な文字列フィールドを取り出す。
func optionalStringValue(t *testing.T, object rawObject, key string) *string {
	t.Helper()
	value := decodeWithNumbers(t, object.Raw)
	objectMap, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("kwargs is not an object: %s", string(object.Raw))
	}
	rawItem, exists := objectMap[key]
	if !exists || rawItem == nil {
		return nil
	}
	text, ok := rawItem.(string)
	if !ok {
		t.Fatalf("kwargs[%q] is not a string: %s", key, string(object.Raw))
	}
	return &text
}
