package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
)

// ErrorMessages は Twitter API のエラーコードとエラーメッセージの対応表。
// (TwitterGraphQLAPI.ERROR_MESSAGES と一致させること)
var ErrorMessages = map[int]string{
	32:  "Twitter アカウントの認証に失敗しました。もう一度連携し直してください。",
	63:  "Twitter アカウントが凍結またはロックされています。",
	64:  "Twitter アカウントが凍結またはロックされています。",
	88:  "Twitter API エンドポイントのレート制限を超えました。",
	89:  "Twitter アクセストークンの有効期限が切れています。",
	99:  "Twitter OAuth クレデンシャルの認証に失敗しました。",
	131: "Twitter でサーバーエラーが発生しています。",
	135: "Twitter アカウントの認証に失敗しました。もう一度連携し直してください。",
	139: "すでにいいねされています。",
	144: "ツイートが非公開かすでに削除されています。",
	179: "フォローしていない非公開アカウントのツイートは表示できません。",
	185: "ツイート数の上限に達しました。",
	186: "ツイートが長過ぎます。",
	187: "ツイートが重複しています。",
	226: "ツイートが自動化されたスパムと判定されました。",
	261: "Twitter API アプリケーションが凍結されています。",
	326: "Twitter アカウントが一時的にロックされています。",
	327: "すでにリツイートされています。",
	328: "このツイートではリツイートは許可されていません。",
	416: "Twitter API アプリケーションが無効化されています。",
}

const (
	// MinimumTweetInterval はツイートの最小送信間隔 (秒) 。
	MinimumTweetInterval = 20
	// MinimumTweetIntervalJitterMin は最小送信間隔に加える追加待機時間の下限 (秒) 。
	MinimumTweetIntervalJitterMin = 0.1
	// MinimumTweetIntervalJitterMax は最小送信間隔に加える追加待機時間の上限 (秒) 。
	MinimumTweetIntervalJitterMax = 4.0
	// MinimumFetchIntervalSeconds はタイムライン / 検索のカーソル付き取得の最小間隔 (秒) 。
	MinimumFetchIntervalSeconds = 30.0
	// BrowserIdleTimeout はヘッドレスブラウザの自動シャットダウンまでの無操作時間 (秒) 。
	BrowserIdleTimeout = 60
	// MaxTweetImages はツイートに添付できる画像の最大枚数。
	MaxTweetImages = 4
	// UnavailableBrowserMessage はヘッドレスブラウザを起動できない場合のエラーメッセージ。
	UnavailableBrowserMessage = "ヘッドレスブラウザの起動に必要な Chrome または Brave が KonomiTV サーバーにインストールされていません。"
)

// RawResponse はブラウザ経由の GraphQL API レスポンスの生データ
// (TwitterScrapeBrowser.TwitterBrowserGraphQLAPIResult 相当) 。
type RawResponse struct {
	// ParsedResponse は JavaScript 側でパース済みのレスポンス本文。
	ParsedResponse any
	// StatusCode は HTTP ステータスコード。
	StatusCode *int
	// ResponseText はレスポンス本文 (文字列) 。
	ResponseText *string
	// Headers は HTTP レスポンスヘッダー (キーは小文字) 。
	Headers map[string]string
	// RequestError は接続エラーなどのリクエストエラー。
	RequestError string
}

// GraphQLClient は Twitter Web App の GraphQL API ラッパー
// (TwitterGraphQLAPI 相当) 。1 つの Twitter アカウントに 1 インスタンスが対応する。
type GraphQLClient struct {
	account *Account
	backend Backend
	logger  *slog.Logger

	// lastGraphQLAPICallTime は GraphQL API の前回呼び出し時刻 (UNIX 時間) 。
	lastGraphQLAPICallTime float64
	// lastHomeFetchedAt はホームタイムラインを実際に取得しに行った時刻 (UNIX 時間) 。
	lastHomeFetchedAt float64
	// lastSearchFetchedAt は検索タイムラインを実際に取得しに行った時刻 (UNIX 時間) 。
	lastSearchFetchedAt float64
	// consecutiveComposeFailures はツイート送信モーダル操作の連続失敗回数。
	consecutiveComposeFailures int
	// tweetLock はツイート送信の排他制御用ロック (最小送信間隔の制御に使用) 。
	tweetLock sync.Mutex
	// lastTweetTime は最小送信間隔の基準となる時刻 (UNIX 時間) 。
	lastTweetTime float64

	// now は現在時刻の取得関数 (テストで差し替える) 。
	now func() time.Time
	// randomFloat はジッター生成関数 (テストで差し替える) 。
	randomFloat func(minimum float64, maximum float64) float64
	// shutdownScheduler は一定時間後のブラウザ自動停止をスケジュールする (テストで差し替える) 。
	shutdownScheduler func(delay time.Duration, callback func())
}

// Account は Twitter 連携アカウント (models/TwitterAccount.py のうち twitter パッケージが扱う範囲) 。
type Account struct {
	ID                int64
	UserID            int64
	Name              string
	ScreenName        string
	IconURL           string
	AccessToken       string
	AccessTokenSecret string
	CookieBrowserInfo string
	// PlainCookies は未保存 (Temporary) アカウント用の平文 Cookie。
	PlainCookies string
	// PersistCookies は Cookie を暗号化して DB に保存するコールバック (internal/api が設定する) 。
	PersistCookies func(ctx context.Context, cookiesTxt string) error
}

// IsNetscapeCookieFile は新しい形式の Cookie 認証レコードかどうかを返す。
func (account *Account) IsNetscapeCookieFile() bool {
	return account.AccessToken == "NETSCAPE_COOKIE_FILE"
}

// LogPrefix はログのプレフィックスを返す。
func (client *GraphQLClient) LogPrefix() string {
	return fmt.Sprintf("[TwitterGraphQLAPI][@%s]", client.account.ScreenName)
}

// Account はこのクライアントが扱うアカウントを返す。
func (client *GraphQLClient) Account() *Account {
	return client.account
}

// SetAccount はアカウント情報を差し替える (Python 版のシングルトン更新相当) 。
func (client *GraphQLClient) SetAccount(account *Account) {
	client.account = account
	client.backend.SetAccount(account)
}

// InvokeGraphQLAPI は GraphQL API へリクエストを送信する (TwitterGraphQLAPI.invokeGraphQLAPI 相当) 。
//
// 戻り値は成功時がレスポンスの data 部分、失敗時は日本語のエラーメッセージ
// (Python 版と同じく文字列で返す) 。
func (client *GraphQLClient) InvokeGraphQLAPI(
	ctx context.Context,
	endpointName string,
	variables *OrderedMap,
	additionalFlags *OrderedMap,
	errorMessagePrefix string,
) (any, error) {
	// ブラウザプロセスが外部から kill されていないかを事前確認する
	if client.backend.IsSetupComplete() && !client.backend.IsBrowserProcessAlive() {
		client.markBrowserForRestart(ctx, "Browser process is no longer alive (may have been killed externally). Marking browser for re-setup...")
	}

	// ヘッドレスブラウザがまだ起動していない場合、セットアップ処理を実行
	if !client.backend.IsSetupComplete() {
		if err := client.backend.Setup(ctx); err != nil {
			// Chrome / Brave 未導入時などは、日本語のエラーメッセージをそのまま返す
			if message, ok := AsBrowserSetupMessage(err); ok {
				return nil, errorMessage(message)
			}
			client.logger.Error("failed to setup browser", "error", err, "prefix", client.LogPrefix())
			return nil, errorMessage(errorMessagePrefix + "ヘッドレスブラウザのセットアップに失敗しました。")
		}
	}

	client.logger.Info(client.LogPrefix() + " Requesting " + endpointName + " GraphQL API ...")
	rawResponse, err := client.backend.InvokeGraphQLAPI(ctx, endpointName, variables, additionalFlags)
	if err != nil {
		client.logger.Error("failed to connect to Twitter GraphQL API", "error", err, "prefix", client.LogPrefix())
		// ブラウザとの接続断またはフック関数の消失を検出した場合、ブラウザを再起動して1回だけリトライする
		isConnectionLost := IsConnectionLostError(err)
		isHookLost := strings.Contains(err.Error(), "__invokeGraphQLAPI is not a function")
		if !isHookLost && !isConnectionLost {
			client.finishRequest(ctx)
			return nil, errorMessage(errorMessagePrefix + "Twitter API に接続できませんでした。")
		}
		if isHookLost {
			client.markBrowserForRestart(ctx,
				"JS hook function lost (page may have navigated away from x.com). Restarting browser and retrying...")
		} else {
			client.markBrowserForRestart(ctx,
				"Browser connection lost (browser process may have been killed or crashed). Restarting browser and retrying...")
		}
		if setupErr := client.backend.Setup(ctx); setupErr != nil {
			if message, ok := AsBrowserSetupMessage(setupErr); ok {
				return nil, errorMessage(message)
			}
			client.logger.Error("browser re-setup failed", "error", setupErr, "prefix", client.LogPrefix())
			return nil, errorMessage(errorMessagePrefix + fmt.Sprintf("ヘッドレスブラウザの再セットアップに失敗しました。(%s)", setupErr))
		}
		client.logger.Info(client.LogPrefix() + " Retrying " + endpointName + " GraphQL API after browser restart...")
		rawResponse, err = client.backend.InvokeGraphQLAPI(ctx, endpointName, variables, additionalFlags)
		if err != nil {
			client.logger.Error("retry also failed", "error", err, "prefix", client.LogPrefix())
			return nil, errorMessage(errorMessagePrefix + "ヘッドレスブラウザを再起動しましたが、Twitter API に接続できませんでした。")
		}
	}

	// 成功・失敗に関わらず、API リクエスト完了後に Cookie の保存とシャットダウン予約を行う
	client.finishRequest(ctx)

	return client.processRawResponse(endpointName, rawResponse, errorMessagePrefix)
}

// errorMessage は Python 版が「文字列を返す」エラー経路を表現するエラーを生成する。
func errorMessage(message string) error {
	return &graphqlErrorMessage{message: message}
}

// graphqlErrorMessage は日本語のエラーメッセージのみを運ぶエラー。
type graphqlErrorMessage struct {
	message string
}

func (err *graphqlErrorMessage) Error() string {
	return err.message
}

// AsGraphQLErrorMessage はエラーから日本語のエラーメッセージを取り出す。
func AsGraphQLErrorMessage(err error) (string, bool) {
	var target *graphqlErrorMessage
	if errors.As(err, &target) {
		return target.message, true
	}
	return "", false
}

// markBrowserForRestart はブラウザを再起動が必要な状態にマークする。
func (client *GraphQLClient) markBrowserForRestart(ctx context.Context, reason string) {
	client.logger.Warn(client.LogPrefix() + " " + reason)
	if err := client.backend.MarkForRestart(ctx); err != nil {
		client.logger.Warn("error during browser shutdown", "error", err, "prefix", client.LogPrefix())
	}
}

// finishRequest は API リクエスト完了後の後処理 (Cookie 保存・時刻更新・シャットダウン予約) を行う。
func (client *GraphQLClient) finishRequest(ctx context.Context) {
	if client.backend.IsSetupComplete() {
		if err := client.saveCookies(ctx); err != nil {
			client.logger.Warn("failed to save cookies after API call", "error", err, "prefix", client.LogPrefix())
		}
	}
	client.lastGraphQLAPICallTime = client.currentTime()
	client.scheduleShutdown()
}

// saveCookies はブラウザの Cookie を取得してアカウントへ保存する。
func (client *GraphQLClient) saveCookies(ctx context.Context) error {
	cookies, err := client.backend.SaveCookiesToNetscapeFormat(ctx)
	if err != nil {
		return err
	}
	return PersistCookies(ctx, client.account, cookies)
}

// scheduleShutdown は一定時間後にヘッドレスブラウザを自動停止させるタスクを再スケジュールする。
func (client *GraphQLClient) scheduleShutdown() {
	if !client.backend.IsSetupComplete() || client.shutdownScheduler == nil {
		return
	}
	client.shutdownScheduler(BrowserIdleTimeout*time.Second, func() {
		if client.currentTime()-client.lastGraphQLAPICallTime >= BrowserIdleTimeout {
			client.logger.Info(fmt.Sprintf("%s Shutting down browser after %d seconds of inactivity.",
				client.LogPrefix(), BrowserIdleTimeout))
			_ = client.backend.Shutdown(context.Background())
		}
	})
}

// KeepAlive はヘッドレスブラウザの自動シャットダウンを抑制する。
func (client *GraphQLClient) KeepAlive() {
	if !client.backend.IsSetupComplete() {
		return
	}
	client.lastGraphQLAPICallTime = client.currentTime()
	client.scheduleShutdown()
}

// processRawResponse はブラウザから返された生レスポンスを Python 版と同じ手順で解釈する。
func (client *GraphQLClient) processRawResponse(endpointName string, rawResponse *RawResponse, errorMessagePrefix string) (any, error) {
	parsedResponse := rawResponse.ParsedResponse
	statusCode := rawResponse.StatusCode
	responseText := rawResponse.ResponseText
	headers := rawResponse.Headers

	// リクエストエラーが発生した場合 (接続エラー)
	if rawResponse.RequestError != "" {
		client.logger.Error(client.LogPrefix() + " Request error: " + rawResponse.RequestError)
		return nil, errorMessage(errorMessagePrefix + "Twitter API に接続できませんでした。")
	}

	// JSON でないレスポンスが返ってきた場合
	if headers != nil {
		contentType := headers["content-type"]
		if contentType != "" && !strings.Contains(contentType, "application/json") {
			client.logger.Error(client.LogPrefix() + " Response is not JSON. (Content-Type: " + contentType + ")")
			client.logResponseDiagnostics("GraphQL API returned non-JSON response", statusCode, headers, responseText)
			return nil, errorMessage(errorMessagePrefix + fmt.Sprintf("Twitter API から JSON 以外のレスポンスが返されました。(Content-Type: %s)", contentType))
		}
	}

	// レスポンスを JSON として解釈する
	var responseJSON any
	if parsedResponse != nil {
		// JavaScript 側で既にパース済み
		responseJSON = parsedResponse
	} else if responseText != nil && *responseText != "" {
		decoded, err := DecodeJSON(*responseText)
		if err != nil {
			// HTTP ステータスコードが 200 系以外で返ってきている場合
			if statusCode != nil && !(*statusCode >= 200 && *statusCode < 300) {
				client.logger.Error(fmt.Sprintf("%s Failed to invoke GraphQL API. (HTTP Error %d)", client.LogPrefix(), *statusCode))
				client.logResponseDiagnostics("Failed to parse GraphQL API response as JSON after HTTP error", statusCode, headers, responseText)
				return nil, errorMessage(errorMessagePrefix + fmt.Sprintf("Twitter API から HTTP %d エラーが返されました。", *statusCode))
			}
			client.logger.Error(client.LogPrefix()+" Failed to parse response as JSON", "error", err)
			client.logResponseDiagnostics("Failed to parse GraphQL API response as JSON", statusCode, headers, responseText)
			return nil, errorMessage(errorMessagePrefix + "Twitter API のレスポンスを JSON としてパースできませんでした。")
		}
		responseJSON = decoded
	}

	if responseJSON == nil {
		client.logger.Error(client.LogPrefix() + " Failed to parse response as JSON.")
		client.logResponseDiagnostics("GraphQL API response JSON is empty", statusCode, headers, responseText)
		return nil, errorMessage(errorMessagePrefix + "Twitter API のレスポンスを JSON としてパースできませんでした。")
	}

	responseObject, isObject := responseJSON.(map[string]any)
	if !isObject {
		// Python 版では dict 以外 (list など) は "data" キー判定に落ちる
		client.logger.Error(client.LogPrefix() + " Response does not have \"data\" key.")
		return nil, errorMessage(errorMessagePrefix + "Twitter API のレスポンスに \"data\" キーが存在しません。開発者に修正を依頼してください。")
	}

	// API レスポンスにエラーが含まれていて、かつ data キーが存在しない場合
	if errorsValue, hasErrors := responseObject["errors"]; hasErrors {
		if _, hasData := responseObject["data"]; !hasData {
			errorCode, errorMessageValue, ok := extractFirstError(errorsValue)
			if !ok {
				return nil, errorMessage(errorMessagePrefix + "Twitter API のレスポンスのエラー形式が不正です。開発者に修正を依頼してください。")
			}
			alternativeErrorMessage := fmt.Sprintf("Code: %s / Message: %s", pyValueString(errorCode), pyValueString(errorMessageValue))
			client.logger.Error(client.LogPrefix() + " Failed to invoke GraphQL API (" + alternativeErrorMessage + ")")
			if mapped, exists := ErrorMessages[intValue(errorCode)]; exists {
				return nil, errorMessage(errorMessagePrefix + mapped)
			}
			return nil, errorMessage(errorMessagePrefix + alternativeErrorMessage)
		}
	}

	// API レスポンスにエラーが含まれていないが、'data' キーが存在しない場合
	if _, hasData := responseObject["data"]; !hasData {
		client.logger.Error(client.LogPrefix() + " Response does not have \"data\" key.")
		return nil, errorMessage(errorMessagePrefix + "Twitter API のレスポンスに \"data\" キーが存在しません。開発者に修正を依頼してください。")
	}

	// ここまで来たら GraphQL API レスポンスの取得には成功しているはず
	client.logger.Info(fmt.Sprintf("%s %s GraphQL API request completed.", client.LogPrefix(), endpointName))
	return responseObject["data"], nil
}

// logResponseDiagnostics は GraphQL API レスポンスの診断情報をログに出力する。
func (client *GraphQLClient) logResponseDiagnostics(reason string, statusCode *int, headers map[string]string, responseText *string) {
	status := "<None>"
	if statusCode != nil {
		status = fmt.Sprintf("%d", *statusCode)
	}
	client.logger.Error(fmt.Sprintf("%s %s. status_code: %s", client.LogPrefix(), reason, status))
	if headers != nil {
		if encoded, err := PythonJSON(headers); err == nil {
			client.logger.Error(client.LogPrefix() + " Response headers: " + string(encoded))
		}
	}
	if responseText == nil {
		client.logger.Error(client.LogPrefix() + " Response text: <None>")
		return
	}
	const responseTextLimit = 4000
	if len(*responseText) > responseTextLimit {
		client.logger.Error(fmt.Sprintf("%s Response text: %s... (truncated, total_length: %d)",
			client.LogPrefix(), (*responseText)[:responseTextLimit], len(*responseText)))
	} else {
		client.logger.Error(client.LogPrefix() + " Response text: " + *responseText)
	}
}

// extractFirstError は errors 配列の先頭要素から code / message を取り出す。
func extractFirstError(value any) (code any, message any, ok bool) {
	items, isList := value.([]any)
	if !isList || len(items) == 0 {
		return nil, nil, false
	}
	first, isObject := items[0].(map[string]any)
	if !isObject {
		return nil, nil, false
	}
	code, hasCode := first["code"]
	if !hasCode {
		return nil, nil, false
	}
	return code, first["message"], true
}

// intValue は JSON 由来の数値を int に変換する (変換できない場合は -1) 。
func intValue(value any) int {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return -1
		}
		return int(parsed)
	case float64:
		return int(typed)
	case int:
		return typed
	}
	return -1
}

// pyValueString は f-string 内の {value} 相当の文字列化を行う。
func pyValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%v", typed)
	case bool:
		if typed {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", value)
}

// IsConnectionLostError はブラウザとの接続断を示すエラーかどうかを判定する
// (TwitterGraphQLAPI.__isConnectionLostError 相当) 。
func IsConnectionLostError(err error) bool {
	if err == nil {
		return false
	}
	// WebSocket 接続が切断された場合のメッセージパターン
	if strings.Contains(err.Error(), "no close frame") {
		return true
	}
	// ブラウザバックエンドが接続断として返す番兵エラー
	if errors.Is(err, ErrBrowserConnectionLost) {
		return true
	}
	return false
}

// DecodeJSON は JSON 文字列を any にデコードする (数値は json.Number で保持) 。
func DecodeJSON(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// roundToThreeDecimals は Python の round(value, 3) 相当 (銀行家丸め) で小数第3位に丸める。
func roundToThreeDecimals(value float64) float64 {
	scaled := value * 1000
	floor := math.Floor(scaled)
	diff := scaled - floor
	switch {
	case diff > 0.5:
		return (floor + 1) / 1000
	case diff < 0.5:
		return floor / 1000
	default:
		if math.Mod(floor, 2) == 0 {
			return floor / 1000
		}
		return (floor + 1) / 1000
	}
}

// currentTime は現在時刻 (UNIX 秒, 小数あり) を返す。
func (client *GraphQLClient) currentTime() float64 {
	return float64(client.now().UnixNano()) / 1e9
}
