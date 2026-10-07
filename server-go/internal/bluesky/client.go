package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// 既定の PDS エンドポイント (atproto SDK の _BASE_API_URL と同じ) 。
const defaultBaseURL = "https://bsky.social/xrpc"

// requestTimeout は atproto SDK の REQUEST_TIMEOUT と同じ値 (timeout=30s, connect=10s) 。
const (
	requestTimeout        = 30 * time.Second
	requestConnectTimeout = 10 * time.Second
)

// APIError は Bluesky API 呼び出しのエラーを表す。
type APIError struct {
	StatusCode int
	// Kind は atproto SDK の例外クラスに対応する区分。
	Kind APIErrorKind
	// Message は API が返したエラーメッセージ。
	Message string
}

// APIErrorKind は atproto SDK の例外の種類。
type APIErrorKind string

const (
	// APIErrorUnauthorized は 401 / 403 (atproto の UnauthorizedError) 。
	APIErrorUnauthorized APIErrorKind = "unauthorized"
	// APIErrorBadRequest は 400 (atproto の BadRequestError) 。
	APIErrorBadRequest APIErrorKind = "bad_request"
	// APIErrorNetwork は 409 / 413 / 502 (atproto の NetworkError) 。
	APIErrorNetwork APIErrorKind = "network"
	// APIErrorRequest はその他の HTTP エラー (atproto の RequestException) 。
	APIErrorRequest APIErrorKind = "request"
	// APIErrorTimeout は接続タイムアウト (atproto の InvokeTimeoutError) 。
	APIErrorTimeout APIErrorKind = "timeout"
	// APIErrorTransport はレスポンスを受け取れなかった通信エラー (atproto の NetworkError) 。
	APIErrorTransport APIErrorKind = "transport"
)

// Error は error インターフェースを実装する。
func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("bluesky api error: status=%d kind=%s message=%s", e.StatusCode, e.Kind, e.Message)
	}
	return fmt.Sprintf("bluesky api error: kind=%s message=%s", e.Kind, e.Message)
}

// isRetriable は一時的な通信失敗として再試行してよいエラーかどうかを返す。
// Python 版 BlueskyAPI._isRetriableBlueskyError() と同じ判定。
func (e *APIError) isRetriable() bool {
	if e.Kind == APIErrorTimeout || e.Kind == APIErrorTransport {
		return true
	}
	// SDK は 413 も NetworkError として扱うが、blob サイズ超過は再試行しても解消しない
	return e.StatusCode == 502
}

// isUnauthorized は再連携で解消すべき認証エラーかどうかを返す。
func (e *APIError) isUnauthorized() bool {
	return e.Kind == APIErrorUnauthorized
}

// xrpcResponse は XRPC のレスポンス。
type xrpcResponse struct {
	StatusCode int
	// Content はレスポンスボディ (JSON の場合はデコード済み) 。
	Content []byte
	// Headers はレスポンスヘッダー。
	Headers http.Header
}

// decodeJSON はレスポンスボディを target へデコードする。
// Python 版は SDK のレスポンスモデル化直前に未知 Union を正規化しているため、
// 同じ処理をデコード前に行う (未知 embed / reason で落ちないようにする) 。
func (r *xrpcResponse) decodeJSON(target any) error {
	if len(r.Content) == 0 || target == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(r.Content))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		// JSON として読めないレスポンスはそのままデコードを試みる
		return json.Unmarshal(r.Content, target)
	}
	NormalizeUnknownSchemaForSDK(generic)
	encoded, err := marshalJSONWithoutEscaping(generic)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

// errNoResponse はモックテストで未定義のリクエストに使うエラー。
var errNoResponse = errors.New("no scripted response")

// Client は Bluesky の XRPC クライアント。
type Client struct {
	httpClient *http.Client
	// baseURL は PDS の XRPC エンドポイント (末尾は /xrpc) 。
	baseURL string
	// session は復元済みのセッション (nil の場合は未ログイン) 。
	session *Session
	// onSessionChange はセッション作成・更新時に呼ばれるコールバック。
	onSessionChange func(event SessionEvent, session *Session) error
	// logger はログ出力に使う関数。
	logf func(format string, values ...any)
	// now は現在時刻を返す関数 (テストで差し替え可能) 。
	now func() time.Time
	// sleep は再試行の待機に使う関数 (テストで差し替え可能) 。
	sleep func(duration time.Duration)
	// maxAttempts は再試行回数。
	maxAttempts int
	// retryBaseDelay は再試行の基本待機時間。
	retryBaseDelay time.Duration
}

// SessionEvent はセッション変更イベント (atproto SDK の SessionEvent 相当) 。
type SessionEvent string

const (
	// SessionEventCreate は新規ログイン。
	SessionEventCreate SessionEvent = "create"
	// SessionEventRefresh はトークン更新。
	SessionEventRefresh SessionEvent = "refresh"
)

// ClientOptions は Client の生成に必要な依存関係。
type ClientOptions struct {
	// HTTPClient は XRPC に使う HTTP クライアント (nil の場合は既定のクライアント) 。
	HTTPClient *http.Client
	// BaseURL は PDS の XRPC エンドポイント (nil の場合は https://bsky.social/xrpc) 。
	BaseURL string
	// OnSessionChange はセッション作成・更新時に呼ばれるコールバック。
	OnSessionChange func(event SessionEvent, session *Session) error
	// Logf はログ出力に使う関数。
	Logf func(format string, values ...any)
}

// NewClient は Client を生成する。
func NewClient(options ClientOptions) *Client {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	baseURL := options.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{
		httpClient:      httpClient,
		baseURL:         normalizeBaseURL(baseURL),
		onSessionChange: options.OnSessionChange,
		logf:            logf,
		now:             time.Now,
		sleep:           time.Sleep,
		maxAttempts:     3,
		retryBaseDelay:  800 * time.Millisecond,
	}
}

// NewHTTPClient は XRPC 用の HTTP クライアントを生成する。
// atproto SDK の Timeout(timeout=30s, connect=10s) に相当する。
func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: requestConnectTimeout}).DialContext,
		ResponseHeaderTimeout: requestTimeout,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport}
}

// normalizeBaseURL は PDS のベース URL を XRPC エンドポイント形式に揃える。
func normalizeBaseURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/xrpc") {
		return baseURL
	}
	return strings.TrimRight(baseURL, "/") + "/xrpc"
}

// Session は現在のセッションを返す。
func (c *Client) Session() *Session {
	return c.session
}

// SetBaseURL は PDS のベース URL を差し替える (テスト用) 。
func (c *Client) SetBaseURL(baseURL string) {
	c.baseURL = normalizeBaseURL(baseURL)
}

// SetOnSessionChange はセッション作成・更新時に呼ばれるコールバックを登録する。
func (c *Client) SetOnSessionChange(callback func(event SessionEvent, session *Session) error) {
	c.onSessionChange = callback
}

// getProfile は app.bsky.actor.getProfile を呼び出す。
func (c *Client) getProfile(ctx context.Context, actor string) (*Profile, error) {
	params := url.Values{}
	params.Set("actor", actor)
	var profile Profile
	if err := c.invokeQueryRaw(ctx, "app.bsky.actor.getProfile", params, &profile); err != nil {
		return nil, err
	}
	return &profile, nil
}

// uploadBlob は com.atproto.repo.uploadBlob を呼び出す。
// atproto SDK と同じく Content-Type: */* でバイナリを送信する。
func (c *Client) uploadBlob(ctx context.Context, data []byte, target any) error {
	response, err := c.invokeWithRetry(ctx, http.MethodPost, "com.atproto.repo.uploadBlob", nil, data, "*/*")
	if err != nil {
		return err
	}
	return response.decodeJSON(target)
}

// SetNowForTest は現在時刻を返す関数を差し替える (テスト用) 。
func (c *Client) SetNowForTest(now func() time.Time) {
	c.now = now
}

// SetSleepForTest は待機関数を差し替える (テスト用) 。
func (c *Client) SetSleepForTest(sleep func(duration time.Duration)) {
	c.sleep = sleep
}

// Login は App Password またはセッション文字列でログインし、プロフィールを返す。
// atproto SDK の AsyncClient.login() と同じく、セッション復元後にプロフィールを取得する。
func (c *Client) Login(ctx context.Context, identifier string, password string) (*Profile, error) {
	if _, err := c.createSession(ctx, identifier, password); err != nil {
		return nil, err
	}
	return c.getProfile(ctx, c.session.Handle)
}

// ImportSessionString は保存済みのセッション文字列からセッションを復元する。
// atproto SDK の AsyncClient.login(session_string=...) 相当。
func (c *Client) ImportSessionString(ctx context.Context, sessionString string) (*Profile, error) {
	session, err := DecodeSessionString(sessionString)
	if err != nil {
		return nil, err
	}
	c.session = session
	c.baseURL = normalizeBaseURL(session.PDSEndpoint)
	return c.getProfile(ctx, session.Handle)
}

// ExportSessionString は現在のセッションを文字列へ変換する。
func (c *Client) ExportSessionString() (string, error) {
	if c.session == nil {
		return "", errors.New("session does not exist")
	}
	return c.session.Encode(), nil
}

// Close はアイドル接続を閉じる。
func (c *Client) Close() {
	c.httpClient.CloseIdleConnections()
}

// createSession は com.atproto.server.createSession を呼び出す。
func (c *Client) createSession(ctx context.Context, identifier string, password string) (*Session, error) {
	body := map[string]any{"identifier": identifier, "password": password}
	var parsed struct {
		AccessJWT  string          `json:"accessJwt"`
		RefreshJWT string          `json:"refreshJwt"`
		Handle     string          `json:"handle"`
		DID        string          `json:"did"`
		DidDoc     json.RawMessage `json:"didDoc"`
	}
	if err := c.invokeProcedureRaw(ctx, "com.atproto.server.createSession", body, &parsed); err != nil {
		return nil, err
	}

	// didDoc から atproto_pds サービスのエンドポイントを取り出す
	// (自前 PDS では取れない場合があるため、その場合は現在の baseURL を使う)
	pdsEndpoint := c.baseURL
	if endpoint := pdsEndpointFromDidDoc(parsed.DidDoc); endpoint != "" {
		pdsEndpoint = endpoint
	}

	session := &Session{
		Handle:      parsed.Handle,
		DID:         parsed.DID,
		AccessJWT:   parsed.AccessJWT,
		RefreshJWT:  parsed.RefreshJWT,
		PDSEndpoint: pdsEndpoint,
	}
	c.session = session
	c.baseURL = normalizeBaseURL(pdsEndpoint)
	if err := c.notifySessionChange(SessionEventCreate); err != nil {
		return nil, err
	}
	return session, nil
}

// refreshSession は com.atproto.server.refreshSession でトークンを更新する。
func (c *Client) refreshSession(ctx context.Context) error {
	if c.session == nil || c.session.RefreshJWT == "" {
		return errors.New("login required")
	}
	var parsed struct {
		AccessJWT  string          `json:"accessJwt"`
		RefreshJWT string          `json:"refreshJwt"`
		Handle     string          `json:"handle"`
		DID        string          `json:"did"`
		DidDoc     json.RawMessage `json:"didDoc"`
	}
	if err := c.invokeProcedureWithToken(ctx, "com.atproto.server.refreshSession", nil, c.session.RefreshJWT, &parsed); err != nil {
		return err
	}

	pdsEndpoint := c.baseURL
	if endpoint := pdsEndpointFromDidDoc(parsed.DidDoc); endpoint != "" {
		pdsEndpoint = endpoint
	}
	c.session.AccessJWT = parsed.AccessJWT
	c.session.RefreshJWT = parsed.RefreshJWT
	if parsed.Handle != "" {
		c.session.Handle = parsed.Handle
	}
	if parsed.DID != "" {
		c.session.DID = parsed.DID
	}
	c.session.PDSEndpoint = pdsEndpoint
	c.baseURL = normalizeBaseURL(pdsEndpoint)
	return c.notifySessionChange(SessionEventRefresh)
}

// notifySessionChange は登録されたコールバックを呼び出す。
// atproto SDK と同じく、コールバックにはセッションのコピーを渡す。
func (c *Client) notifySessionChange(event SessionEvent) error {
	if c.onSessionChange == nil {
		return nil
	}
	sessionCopy := *c.session
	return c.onSessionChange(event, &sessionCopy)
}

// shouldRefreshSession はアクセストークンの期限が近いかどうかを返す。
// atproto SDK と同じく 15 分前から更新する。
func (c *Client) shouldRefreshSession() bool {
	if c.session == nil || c.session.AccessJWT == "" {
		return false
	}
	expiredAt, err := accessTokenExpiresAt(c.session.AccessJWT)
	if err != nil {
		return false
	}
	return c.now().After(expiredAt.Add(-15 * time.Minute))
}

// refreshIfNeeded は必要であればアクセストークンを更新する。
func (c *Client) refreshIfNeeded(ctx context.Context) error {
	if !c.shouldRefreshSession() {
		return nil
	}
	return c.refreshSession(ctx)
}

// invokeQueryRaw は XRPC の query を呼び出し、レスポンスを target へデコードする。
func (c *Client) invokeQueryRaw(ctx context.Context, nsid string, params url.Values, target any) error {
	response, err := c.invokeWithRetry(ctx, http.MethodGet, nsid, params, nil)
	if err != nil {
		return err
	}
	return response.decodeJSON(target)
}

// invokeProcedureRaw は XRPC の procedure を呼び出し、レスポンスを target へデコードする。
func (c *Client) invokeProcedureRaw(ctx context.Context, nsid string, body any, target any) error {
	payload, err := marshalJSONWithoutEscaping(body)
	if err != nil {
		return err
	}
	response, err := c.invokeWithRetry(ctx, http.MethodPost, nsid, nil, payload)
	if err != nil {
		return err
	}
	return response.decodeJSON(target)
}

// invokeWithRetry は必要であればトークンを更新した上で XRPC を呼び出し、
// 一時的な通信失敗に限って再試行する。
func (c *Client) invokeWithRetry(ctx context.Context, method string, nsid string, params url.Values, body []byte, contentTypes ...string) (*xrpcResponse, error) {
	contentType := ""
	if len(contentTypes) > 0 {
		contentType = contentTypes[0]
	}
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.refreshIfNeeded(ctx); err != nil {
			return nil, err
		}
		token := ""
		if c.session != nil {
			token = c.session.AccessJWT
		}

		response, err := c.sendRequest(ctx, method, nsid, params, body, token, contentType)
		if err == nil {
			return response, nil
		}

		var apiError *APIError
		if !errors.As(err, &apiError) {
			return nil, err
		}
		if !apiError.isRetriable() || attempt >= c.maxAttempts {
			return nil, err
		}

		delay := c.retryBaseDelay * time.Duration(attempt)
		c.logf("Retrying Bluesky API operation after transient error. [operation: %s, attempt: %d/%d, retry_delay: %.1fs, error: %v]",
			nsid, attempt, c.maxAttempts, delay.Seconds(), err)
		c.sleep(delay)
	}
	return nil, fmt.Errorf("bluesky api retry loop unexpectedly finished. [operation: %s]", nsid)
}

// invokeProcedureWithToken は指定されたトークンで procedure を呼び出す (再試行あり) 。
func (c *Client) invokeProcedureWithToken(ctx context.Context, nsid string, body any, token string, target any) error {
	var payload []byte
	if body != nil {
		encoded, err := marshalJSONWithoutEscaping(body)
		if err != nil {
			return err
		}
		payload = encoded
	}
	// トークン更新は呼び出し側のセッション状態を変化させるため、ここでは再試行しない
	response, err := c.sendRequest(ctx, http.MethodPost, nsid, nil, payload, token)
	if err != nil {
		return err
	}
	return response.decodeJSON(target)
}

// sendRequest は 1 回の HTTP リクエストを送信し、atproto SDK 互換のエラー分類を行う。
func (c *Client) sendRequest(ctx context.Context, method string, nsid string, params url.Values, body []byte, token string, contentTypes ...string) (*xrpcResponse, error) {
	endpoint := c.baseURL + "/" + nsid
	if len(params) > 0 {
		endpoint += "?" + encodeQuery(params)
	}

	var reader io.Reader
	contentType := ""
	if len(contentTypes) > 0 && contentTypes[0] != "" {
		contentType = contentTypes[0]
	} else if body != nil {
		// JSON ボディを送る procedure は atproto SDK と同じく application/json を付ける
		// (呼び出し側が空文字の Content-Type を明示した場合もここで既定値に倒す)
		contentType = "application/json"
	}
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	httpResponse, err := c.httpClient.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// リクエストのキャンセルは再試行しても意味がないため、そのまま返す
			return nil, err
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, &APIError{Kind: APIErrorTimeout, Message: err.Error()}
		}
		// レスポンス情報がない NetworkError は接続断などの通信失敗
		return nil, &APIError{Kind: APIErrorTransport, Message: err.Error()}
	}
	defer func() { _ = httpResponse.Body.Close() }()

	content, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, &APIError{Kind: APIErrorTransport, Message: err.Error()}
	}

	response := &xrpcResponse{StatusCode: httpResponse.StatusCode, Content: content, Headers: httpResponse.Header}
	if httpResponse.StatusCode >= 200 && httpResponse.StatusCode <= 299 {
		return response, nil
	}

	apiError := &APIError{StatusCode: httpResponse.StatusCode, Message: extractErrorMessage(content)}
	switch httpResponse.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		apiError.Kind = APIErrorUnauthorized
	case http.StatusBadRequest:
		apiError.Kind = APIErrorBadRequest
	case http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusBadGateway:
		apiError.Kind = APIErrorNetwork
	default:
		apiError.Kind = APIErrorRequest
	}
	return nil, apiError
}

// extractErrorMessage は XRPC のエラーレスポンスから message を取り出す。
func extractErrorMessage(content []byte) string {
	var parsed struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return string(content)
	}
	if parsed.Message != "" {
		return parsed.Message
	}
	return parsed.Error
}

// encodeQuery は url.Values を atproto SDK (httpx) と同じ形式のクエリ文字列にする。
func encodeQuery(params url.Values) string {
	return params.Encode()
}

// marshalJSONWithoutEscaping は FastAPI / atproto SDK と同じく非 ASCII 文字と HTML 文字をエスケープせずに JSON 化する。
func marshalJSONWithoutEscaping(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// Encode は末尾に改行を付与するため取り除く
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// pdsEndpointFromDidDoc は DID ドキュメントから atproto_pds のエンドポイントを取り出す。
// atproto SDK の DidDocument.get_pds_endpoint() と同じ判定。
func pdsEndpointFromDidDoc(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var didDoc struct {
		ID      string `json:"id"`
		Service []struct {
			ID              string `json:"id"`
			Type            string `json:"type"`
			ServiceEndpoint any    `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := json.Unmarshal(raw, &didDoc); err != nil {
		return ""
	}
	if didDoc.ID == "" {
		return ""
	}
	for _, service := range didDoc.Service {
		if service.ID != "#atproto_pds" && service.ID != didDoc.ID+"#atproto_pds" {
			continue
		}
		if service.Type != "AtprotoPersonalDataServer" {
			continue
		}
		endpoint, ok := service.ServiceEndpoint.(string)
		if !ok {
			return ""
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return ""
		}
		return endpoint
	}
	return ""
}
