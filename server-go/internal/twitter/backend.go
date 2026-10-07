package twitter

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrBrowserConnectionLost は CDP の WebSocket 接続が切断されたことを示す番兵エラー
// (TwitterGraphQLAPI.__isConnectionLostError の ConnectionClosed 系に相当) 。
var ErrBrowserConnectionLost = errors.New("browser connection lost (no close frame)")

// BrowserSetupError は Chrome / Brave 未導入・起動失敗・セットアップタイムアウトを表すエラー。
// Error() がそのまま日本語のユーザー向けメッセージになる (Python 版の
// BrowserBinaryNotFoundError / BrowserConnectionFailedError / BrowserSetupTimeoutError 相当) 。
type BrowserSetupError struct {
	message string
}

// NewBrowserSetupError はセットアップエラーを生成する。
func NewBrowserSetupError(message string) *BrowserSetupError {
	return &BrowserSetupError{message: message}
}

func (err *BrowserSetupError) Error() string {
	return err.message
}

// AsBrowserSetupMessage はエラーから日本語のセットアップエラーメッセージを取り出す。
func AsBrowserSetupMessage(err error) (string, bool) {
	var target *BrowserSetupError
	if errors.As(err, &target) {
		return target.message, true
	}
	return "", false
}

// ComposeRequest はツイート送信モーダル経由の投稿リクエスト。
type ComposeRequest struct {
	TweetText            string
	Images               []UploadedImage
	ThrottleRemainingSec float64
	InReplyToStatusID    *string
}

// UploadedImage は投稿するツイートに添付する画像。
type UploadedImage struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ComposeResult はツイート送信モーダルの自動操作の結果
// (TwitterScrapeBrowser.PostTweetViaComposeUIResult 相当) 。
type ComposeResult struct {
	IsSuccess          bool
	ErrorMessage       *string
	ComposeSubmittedAt *float64
	GraphQLAPIResult   *RawResponse
}

// Backend は Twitter Web App を操作するヘッドレスブラウザの抽象化
// (TwitterScrapeBrowser のうち GraphQL クライアントが必要とする範囲) 。
//
// Python 版は Zendriver で Chromium を制御するが、Go 版ではこのインターフェースの実装として
// 差し替え可能にしている。実際のブラウザ駆動実装は別途用意する必要があり、
// 未実装の場合は UnavailableBackend が日本語のエラーメッセージを返す。
//
// GraphQL の query_id / operationName を含む HTTP ヘッダーと X-Client-Transaction-ID は
// Twitter Web App の main.js が実行時に内部生成して付与する値であり、Python 版
// (TwitterGraphQLAPI / TwitterScrapeBrowser) にも静的な定義が存在しない。したがって Go 版では
// これらを再現せず、Backend 実装 (ブラウザ上で window.__invokeGraphQLAPI を呼び出す側) の
// 責務とする。Go 側が生成・検証するのは endpoint 名 / variables JSON / additional_flags JSON /
// 実行する JavaScript スニペットまで (いずれも twitter_expected.json の js_code とバイト一致で検証) 。
type Backend interface {
	// SetAccount は操作対象の Twitter アカウントを差し替える。
	SetAccount(account *Account)
	// IsSetupComplete はセットアップ完了済みかどうかを返す。
	IsSetupComplete() bool
	// IsBrowserProcessAlive はブラウザプロセスが生存しているかを返す。
	IsBrowserProcessAlive() bool
	// Setup はブラウザを起動して Cookie を設定する。
	Setup(ctx context.Context) error
	// Shutdown はブラウザを停止する。
	Shutdown(ctx context.Context) error
	// MarkForRestart はブラウザを再起動が必要な状態にマークする (シャットダウンを含む) 。
	MarkForRestart(ctx context.Context) error
	// InvokeGraphQLAPI は Twitter Web App 内部の GraphQL クライアントに HTTP リクエストを実行させる。
	InvokeGraphQLAPI(ctx context.Context, endpointName string, variables *OrderedMap, additionalFlags *OrderedMap) (*RawResponse, error)
	// PostTweetViaComposeUI はツイート送信モーダルを自動操作してツイートを送信する。
	PostTweetViaComposeUI(ctx context.Context, request ComposeRequest) (*ComposeResult, error)
	// SaveCookiesToNetscapeFormat はブラウザの Cookie を Netscape 形式の文字列として保存する。
	SaveCookiesToNetscapeFormat(ctx context.Context) (string, error)
	// CaptureDebugScreenshot はデバッグ用のスクリーンショットを保存する。
	CaptureDebugScreenshot(ctx context.Context, reason string) (string, error)
}

// BackendFactory は Twitter アカウントに対する Backend を生成する。
type BackendFactory func(account *Account) Backend

// UnavailableBackend はブラウザ駆動が未実装の環境で使う Backend。
// Setup が必ず BrowserSetupError を返し、ハンドラーは Python 版と同じ
// 日本語メッセージをクライアントへ返す。
type UnavailableBackend struct {
	account *Account
}

// NewUnavailableBackend は UnavailableBackend を生成する。
func NewUnavailableBackend(account *Account) Backend {
	return &UnavailableBackend{account: account}
}

// SetAccount は操作対象のアカウントを差し替える。
func (backend *UnavailableBackend) SetAccount(account *Account) {
	backend.account = account
}

// IsSetupComplete は常に false を返す。
func (backend *UnavailableBackend) IsSetupComplete() bool {
	return false
}

// IsBrowserProcessAlive は常に true を返す (死活不明のため安全側) 。
func (backend *UnavailableBackend) IsBrowserProcessAlive() bool {
	return true
}

// Setup は常にセットアップエラーを返す。
func (backend *UnavailableBackend) Setup(ctx context.Context) error {
	return NewBrowserSetupError(UnavailableBrowserMessage)
}

// Shutdown は何もしない。
func (backend *UnavailableBackend) Shutdown(ctx context.Context) error {
	return nil
}

// MarkForRestart は何もしない。
func (backend *UnavailableBackend) MarkForRestart(ctx context.Context) error {
	return nil
}

// InvokeGraphQLAPI は常に接続エラーを返す。
func (backend *UnavailableBackend) InvokeGraphQLAPI(ctx context.Context, endpointName string, variables *OrderedMap, additionalFlags *OrderedMap) (*RawResponse, error) {
	return nil, errors.New("browser backend is not available")
}

// PostTweetViaComposeUI は常に失敗を返す。
func (backend *UnavailableBackend) PostTweetViaComposeUI(ctx context.Context, request ComposeRequest) (*ComposeResult, error) {
	return nil, errors.New("browser backend is not available")
}

// SaveCookiesToNetscapeFormat は常に空文字列を返す。
func (backend *UnavailableBackend) SaveCookiesToNetscapeFormat(ctx context.Context) (string, error) {
	return "", nil
}

// CaptureDebugScreenshot は何もしない。
func (backend *UnavailableBackend) CaptureDebugScreenshot(ctx context.Context, reason string) (string, error) {
	return "", nil
}

// BuildInvokeGraphQLScript は TwitterScrapeBrowser.invokeGraphQLAPI が CDP に渡す
// JavaScript 式を、Python 版とバイト単位で同じ内容として生成する。
//
// この式は Python 版 TwitterScrapeBrowser の
//
//	js_code = f"""
//	(async () => {{
//	    const requestPayload = {json.dumps(variables, ensure_ascii=False)};
//	    ...
//
// と同一の出力になる (internal/twitter/testdata/twitter_expected.json の js_code で検証) 。
func BuildInvokeGraphQLScript(endpointName string, variables *OrderedMap, additionalFlags *OrderedMap) (string, error) {
	variablesJSON, err := PythonJSON(variables)
	if err != nil {
		return "", err
	}
	additionalFlagsJSON := "null"
	if additionalFlags != nil {
		additionalFlagsJSON = string(mustPythonJSON(additionalFlags))
	}
	// Python 版の f-string と同一 (インデントと空白も含めて一致させる)
	return fmt.Sprintf(`
        (async () => {
            const requestPayload = %s;
            const additionalFlags = %s;
            const result = await window.__invokeGraphQLAPI('%s', requestPayload, additionalFlags);
            console.log('window.__invokeGraphQLAPI() result:', result);
            return result;
        })()
        `, string(variablesJSON), additionalFlagsJSON, endpointName), nil
}

// mustPythonJSON は PythonJSON の失敗を握りつぶす (additionalFlags は必ずシリアライズ可能) 。
func mustPythonJSON(value any) []byte {
	encoded, err := PythonJSON(value)
	if err != nil {
		// シリアライズできない値が渡された場合は何かがバグっているため、呼び出し側で気付けるよう panic させる
		panic("twitter: failed to serialize additional flags: " + err.Error())
	}
	return encoded
}

// ParseAcceptLanguageHeader は Accept-Language ヘッダーを CDP に渡しやすい言語タグ配列へ変換する
// (TwitterRouter.ParseAcceptLanguageHeader 相当) 。
func ParseAcceptLanguageHeader(acceptLanguage *string) []string {
	if acceptLanguage == nil {
		return []string{}
	}
	languageTags := []string{}
	for _, languagePart := range strings.Split(*acceptLanguage, ",") {
		// `ja-JP;q=0.9` のような重み部分は先頭の言語タグだけを使う
		languageTag := strings.TrimSpace(strings.SplitN(strings.TrimSpace(languagePart), ";", 2)[0])
		if languageTag != "" {
			languageTags = append(languageTags, languageTag)
		}
	}
	return languageTags
}
