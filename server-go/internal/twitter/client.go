package twitter

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// ----------------------------------------------------------------------------
// シングルトンインスタンス管理 (TwitterGraphQLAPI.__instances 相当)
// ----------------------------------------------------------------------------

var (
	instancesMu sync.Mutex
	instances   = map[int64]*GraphQLClient{}
)

// GetInstance は Twitter アカウント ID ごとのシングルトンな GraphQLClient を取得する
// (TwitterGraphQLAPI.__new__ 相当) 。
//
// 既存インスタンスがある場合はアカウント情報を差し替えて返す。
func GetInstance(account *Account, factory BackendFactory, logger *slog.Logger) *GraphQLClient {
	instancesMu.Lock()
	defer instancesMu.Unlock()

	if existing, ok := instances[account.ID]; ok {
		// DB から取得した新鮮なアカウント情報で既存インスタンスを更新する
		existing.SetAccount(account)
		return existing
	}

	backend := UnavailableBackend2(account, factory)
	client := &GraphQLClient{
		account: account,
		backend: backend,
		logger:  logger,
		now:     time.Now,
		randomFloat: func(minimum float64, maximum float64) float64 {
			return minimum + rand.Float64()*(maximum-minimum)
		},
		shutdownScheduler: func(delay time.Duration, callback func()) {
			time.AfterFunc(delay, callback)
		},
	}
	instances[account.ID] = client
	return client
}

// UnavailableBackend2 は factory が nil の場合に UnavailableBackend を返す。
func UnavailableBackend2(account *Account, factory BackendFactory) Backend {
	if factory == nil {
		return NewUnavailableBackend(account)
	}
	return factory(account)
}

// GetInstanceIfExists は登録済みのインスタンスを取得する (未登録なら false) 。
func GetInstanceIfExists(accountID int64) (*GraphQLClient, bool) {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	client, ok := instances[accountID]
	return client, ok
}

// NewDetachedClient はレジストリに登録せずにクライアントを生成する (テスト用) 。
func NewDetachedClient(account *Account, backend Backend, logger *slog.Logger) *GraphQLClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &GraphQLClient{
		account: account,
		backend: backend,
		logger:  logger,
		now:     time.Now,
		randomFloat: func(minimum float64, maximum float64) float64 {
			return minimum + rand.Float64()*(maximum-minimum)
		},
		shutdownScheduler: func(delay time.Duration, callback func()) {
			time.AfterFunc(delay, callback)
		},
	}
}

// RemoveInstance はアカウント ID のインスタンスを破棄する (TwitterGraphQLAPI.removeInstance 相当) 。
func RemoveInstance(accountID int64) {
	instancesMu.Lock()
	client, ok := instances[accountID]
	delete(instances, accountID)
	instancesMu.Unlock()
	if ok {
		_ = client.backend.Shutdown(context.Background())
	}
}

// RebindInstance は連携し直しの前後でインスタンスを引き継ぐ (TwitterGraphQLAPI.rebindInstance 相当) 。
func RebindInstance(previousAccountID *int64, account *Account, factory BackendFactory, logger *slog.Logger) *GraphQLClient {
	if previousAccountID != nil && *previousAccountID != account.ID {
		instancesMu.Lock()
		if existing, ok := instances[*previousAccountID]; ok {
			delete(instances, *previousAccountID)
			existing.SetAccount(account)
			instances[account.ID] = existing
			instancesMu.Unlock()
			return existing
		}
		instancesMu.Unlock()
	}
	return GetInstance(account, factory, logger)
}

// ResetInstances は登録済みインスタンスを全て破棄する (テスト用) 。
func ResetInstances() {
	instancesMu.Lock()
	instances = map[int64]*GraphQLClient{}
	instancesMu.Unlock()
}

// SetNowForTesting はテスト用に現在時刻取得関数を差し替える。
func (client *GraphQLClient) SetNowForTesting(now func() time.Time) {
	client.now = now
}

// SetRandomFloatForTesting はテスト用にジッター生成関数を差し替える。
func (client *GraphQLClient) SetRandomFloatForTesting(randomFloat func(minimum float64, maximum float64) float64) {
	client.randomFloat = randomFloat
}

// SetShutdownSchedulerForTesting はテスト用にシャットダウン予約関数を差し替える。
func (client *GraphQLClient) SetShutdownSchedulerForTesting(scheduler func(delay time.Duration, callback func())) {
	client.shutdownScheduler = scheduler
}

// LastTweetTime は最後にツイートした時刻 (UNIX 秒) を返す (テスト用) 。
func (client *GraphQLClient) LastTweetTime() float64 {
	return client.lastTweetTime
}

// SetLastTweetTimeForTesting はテスト用に最後のツイート時刻を設定する。
func (client *GraphQLClient) SetLastTweetTimeForTesting(value float64) {
	client.lastTweetTime = value
}

// ----------------------------------------------------------------------------
// 各エンドポイント
// ----------------------------------------------------------------------------

// FetchLoggedViewer は現在ログイン中の Twitter アカウントの情報を取得する
// (TwitterGraphQLAPI.fetchLoggedViewer 相当) 。
//
// 戻り値は *TweetUser か *TwitterAPIResult のいずれか。
func (client *GraphQLClient) FetchLoggedViewer(ctx context.Context) any {
	response := client.invokeOrResult(ctx, "Viewer", buildViewerVariables(), buildViewerAdditionalFlags(), "ユーザー情報の取得に失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	responseObject, ok := response.(map[string]any)
	if !ok {
		return &TwitterAPIResult{IsSuccess: false, Detail: "ユーザー情報の取得に失敗しました。レスポンスにユーザー情報が含まれていません。開発者に修正を依頼してください。"}
	}

	viewer, _ := getObject(responseObject, "viewer")
	userResults, _ := getObject(viewer, "user_results")
	result, _ := getObject(userResults, "result")
	if len(result) == 0 {
		client.logger.Error(client.LogPrefix() + " Failed to fetch logged viewer: user_results.result not found")
		return &TwitterAPIResult{IsSuccess: false, Detail: "ユーザー情報の取得に失敗しました。レスポンスにユーザー情報が含まれていません。開発者に修正を依頼してください。"}
	}

	userID, ok := pyStr(result["rest_id"])
	if !ok || userID == "" {
		client.logger.Error(client.LogPrefix() + " Failed to fetch logged viewer: rest_id not found")
		return &TwitterAPIResult{IsSuccess: false, Detail: "ユーザー情報の取得に失敗しました。ユーザー ID を取得できませんでした。開発者に修正を依頼してください。"}
	}

	core, _ := getObject(result, "core")
	name, _ := core["name"].(string)
	screenName, _ := core["screen_name"].(string)
	if name == "" || screenName == "" {
		client.logger.Error(client.LogPrefix() + " Failed to fetch logged viewer: name or screen_name not found")
		return &TwitterAPIResult{IsSuccess: false, Detail: "ユーザー情報の取得に失敗しました。ユーザー名またはスクリーンネームを取得できませんでした。開発者に修正を依頼してください。"}
	}

	avatar, _ := getObject(result, "avatar")
	iconURL, _ := avatar["image_url"].(string)
	if iconURL == "" {
		client.logger.Error(client.LogPrefix() + " Failed to fetch logged viewer: image_url not found")
		return &TwitterAPIResult{IsSuccess: false, Detail: "ユーザー情報の取得に失敗しました。アイコン URL を取得できませんでした。開発者に修正を依頼してください。"}
	}

	return &TweetUser{
		Source:     "Twitter",
		ID:         userID,
		Name:       name,
		ScreenName: screenName,
		IconURL:    replaceAll(iconURL, "_normal", ""),
	}
}

// CreateTweet はツイート送信モーダル経由でツイートを送信する (TwitterGraphQLAPI.createTweet 相当) 。
//
// 戻り値は *PostTweetResult か *TwitterAPIResult のいずれか。
func (client *GraphQLClient) CreateTweet(ctx context.Context, tweet string, images []UploadedImage, inReplyToStatusID *string) any {
	client.tweetLock.Lock()
	defer client.tweetLock.Unlock()

	current := client.currentTime()
	minimumTweetIntervalEnd := client.lastTweetTime + MinimumTweetInterval
	throttleDeadline := current
	if current < minimumTweetIntervalEnd {
		additionalWait := roundToThreeDecimals(client.randomFloat(MinimumTweetIntervalJitterMin, MinimumTweetIntervalJitterMax))
		throttleDeadline = minimumTweetIntervalEnd + additionalWait
	}

	// ブラウザプロセスが外部から kill されていないかを事前確認する
	if client.backend.IsSetupComplete() && !client.backend.IsBrowserProcessAlive() {
		client.markBrowserForRestart(ctx, "Browser process is no longer alive (may have been killed externally). Marking browser for re-setup...")
		client.consecutiveComposeFailures = 0
	}

	// ツイート送信モーダルの操作が連続で失敗している場合、ブラウザを再起動して状態をリセットする
	if client.consecutiveComposeFailures >= 2 && client.backend.IsSetupComplete() {
		client.markBrowserForRestart(ctx, fmt.Sprintf(
			"%d consecutive tweet posting failures detected. Restarting browser to reset state...",
			client.consecutiveComposeFailures,
		))
		client.consecutiveComposeFailures = 0
	}

	// ヘッドレスブラウザがまだ起動していない場合、セットアップ処理を実行
	if !client.backend.IsSetupComplete() {
		if err := client.backend.Setup(ctx); err != nil {
			if message, ok := AsBrowserSetupMessage(err); ok {
				return &TwitterAPIResult{IsSuccess: false, Detail: message}
			}
			client.logger.Error("failed to setup browser", "error", err, "prefix", client.LogPrefix())
			return &TwitterAPIResult{IsSuccess: false, Detail: errorMessagePrefixGeneric + fmt.Sprintf("%s", err)}
		}
	}

	throttleRemainingSeconds := maxFloat(0.0, throttleDeadline-client.currentTime())
	client.logger.Info(fmt.Sprintf("%s Posting tweet via tweet posting modal...", client.LogPrefix()))
	composeUIResult, err := client.backend.PostTweetViaComposeUI(ctx, ComposeRequest{
		TweetText:            tweet,
		Images:               images,
		ThrottleRemainingSec: throttleRemainingSeconds,
		InReplyToStatusID:    inReplyToStatusID,
	})
	if err != nil {
		client.logger.Error("failed to post tweet via tweet posting modal", "error", err, "prefix", client.LogPrefix())
		client.consecutiveComposeFailures++
		if IsConnectionLostError(err) {
			client.markBrowserForRestart(ctx,
				"Browser connection lost during tweet posting. Shutting down browser for re-setup on next call...")
		}
		client.finishTweetRequest(ctx)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。Twitter API に接続できませんでした。"}
	}
	client.finishTweetRequest(ctx)

	if composeUIResult.ComposeSubmittedAt != nil {
		client.lastTweetTime = *composeUIResult.ComposeSubmittedAt
	}

	if !composeUIResult.IsSuccess {
		client.consecutiveComposeFailures++
		errorText := "不明なエラー"
		if composeUIResult.ErrorMessage != nil {
			errorText = *composeUIResult.ErrorMessage
		}
		client.logger.Error(fmt.Sprintf("%s Failed to create tweet via tweet posting modal: %s (consecutive failures: %d)",
			client.LogPrefix(), errorText, client.consecutiveComposeFailures))
		if !client.backend.IsBrowserProcessAlive() {
			client.markBrowserForRestart(ctx,
				"Browser process died during tweet posting modal operation. Shutting down browser for re-setup on next call...")
		}
		return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。" + errorText}
	}

	graphqlAPIResult := composeUIResult.GraphQLAPIResult
	if graphqlAPIResult == nil {
		client.consecutiveComposeFailures++
		client.logger.Error(client.LogPrefix() + " Failed to get CreateTweet response: graphql_api_result is None")
		return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。CreateTweet のレスポンスが取得できませんでした。"}
	}

	parsedResponse := graphqlAPIResult.ParsedResponse
	statusCode := graphqlAPIResult.StatusCode

	// HTTP ステータスコードが 200 系以外の場合
	if statusCode != nil && !(*statusCode >= 200 && *statusCode < 300) {
		client.consecutiveComposeFailures++
		if responseObject, ok := parsedResponse.(map[string]any); ok {
			if _, hasErrors := responseObject["errors"]; hasErrors {
				errorCode, errorMessageValue, ok := extractFirstError(responseObject["errors"])
				if ok {
					message, exists := ErrorMessages[intValue(errorCode)]
					if !exists {
						message = fmt.Sprintf("Code: %s / Message: %s", pyValueString(errorCode), pyValueString(errorMessageValue))
					}
					return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。" + message}
				}
			}
		}
		return &TwitterAPIResult{IsSuccess: false, Detail: fmt.Sprintf("Twitter へのツイートに失敗しました。Twitter API から HTTP %d エラーが返されました。", *statusCode)}
	}

	if _, ok := parsedResponse.(map[string]any); !ok {
		client.consecutiveComposeFailures++
		client.logger.Error(client.LogPrefix() + " Failed to extract tweet ID from response: parsed_response is not a dict")
		return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。レスポンスの形式が不正です。"}
	}

	parsedObject := parsedResponse.(map[string]any)
	if _, hasErrors := parsedObject["errors"]; hasErrors {
		if _, hasData := parsedObject["data"]; !hasData {
			client.consecutiveComposeFailures++
			errorCode, errorMessageValue, ok := extractFirstError(parsedObject["errors"])
			message := "Unknown error"
			if ok {
				if mapped, exists := ErrorMessages[intValue(errorCode)]; exists {
					message = mapped
				} else {
					message = fmt.Sprintf("Code: %s / Message: %s", pyValueString(errorCode), pyValueString(errorMessageValue))
				}
			}
			client.logger.Error(client.LogPrefix() + " CreateTweet API error: " + message)
			return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。" + message}
		}
	}

	responseData := parsedObject
	if data, hasData := parsedObject["data"]; hasData {
		if dataObject, ok := data.(map[string]any); ok {
			responseData = dataObject
		}
	}

	createTweet, _ := getObject(responseData, "create_tweet")
	tweetResults, hasTweetResults := getObject(createTweet, "tweet_results")
	if hasTweetResults && len(tweetResults) == 0 {
		client.consecutiveComposeFailures++
		client.logger.Warn(client.LogPrefix() + " CreateTweet returned empty tweet_results (HTTP 200 but no tweet data). " +
			"This may indicate Twitter rate limiting or spam detection. Subsequent image uploads may fail. " +
			fmt.Sprintf("(consecutive failures: %d)", client.consecutiveComposeFailures))
		_, _ = client.backend.CaptureDebugScreenshot(ctx, "empty_tweet_results")
		return &TwitterAPIResult{IsSuccess: false, Detail: "Twitter へのツイートに失敗しました。Twitter からの応答が空でした。レートリミットまたはスパム制限の可能性があります。"}
	}

	result, _ := getObject(tweetResults, "result")
	tweetID, ok := pyStr(result["rest_id"])
	if !ok {
		// HTTP 200 + errors なし + data キーありだが中身が想定外 → ツイート自体は送信済みと判断
		client.consecutiveComposeFailures = 0
		return &PostTweetResult{
			IsSuccess: true,
			Detail:    "Twitter にツイートしましたが、ツイート ID を取得できませんでした。",
			TweetURL:  "https://x.com/i/status/__error__",
			TweetID:   nil,
		}
	}

	client.consecutiveComposeFailures = 0
	client.logger.Info(fmt.Sprintf("%s Tweet posted successfully. tweet_id: %s", client.LogPrefix(), tweetID))
	selected := tweetID
	return &PostTweetResult{
		IsSuccess: true,
		Detail:    "Twitter にツイートしました。",
		TweetURL:  "https://x.com/i/status/" + tweetID,
		TweetID:   &selected,
	}
}

// errorMessagePrefixGeneric はブラウザセットアップ失敗時に使う汎用プレフィックス。
const errorMessagePrefixGeneric = "Twitter へのツイートに失敗しました。"

// finishTweetRequest はツイート送信後の Cookie 保存・シャットダウン再スケジュールを行う。
func (client *GraphQLClient) finishTweetRequest(ctx context.Context) {
	if client.backend.IsSetupComplete() {
		if err := client.saveCookies(ctx); err != nil {
			client.logger.Warn("failed to save cookies after tweet posting", "error", err, "prefix", client.LogPrefix())
		}
	}
	client.lastGraphQLAPICallTime = client.currentTime()
	client.scheduleShutdown()
}

// CreateRetweet はツイートをリツイートする。
func (client *GraphQLClient) CreateRetweet(ctx context.Context, tweetID string) *TwitterAPIResult {
	variables, _ := buildRetweetVariables(tweetID)
	response := client.invokeOrResult(ctx, "CreateRetweet", variables, nil, "リツイートに失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "リツイートしました。"}
}

// DeleteRetweet はツイートのリツイートを取り消す。
func (client *GraphQLClient) DeleteRetweet(ctx context.Context, tweetID string) *TwitterAPIResult {
	variables, _ := buildDeleteRetweetVariables(tweetID)
	response := client.invokeOrResult(ctx, "DeleteRetweet", variables, nil, "リツイートの取り消しに失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "リツイートを取り消ししました。"}
}

// FavoriteTweet はツイートをいいねする。
func (client *GraphQLClient) FavoriteTweet(ctx context.Context, tweetID string) *TwitterAPIResult {
	variables, _ := buildTweetIDVariables(tweetID)
	response := client.invokeOrResult(ctx, "FavoriteTweet", variables, nil, "いいねに失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "いいねしました。"}
}

// UnfavoriteTweet はツイートのいいねを取り消す。
func (client *GraphQLClient) UnfavoriteTweet(ctx context.Context, tweetID string) *TwitterAPIResult {
	variables, _ := buildTweetIDVariables(tweetID)
	response := client.invokeOrResult(ctx, "UnfavoriteTweet", variables, nil, "いいねの取り消しに失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "いいねを取り消しました。"}
}

// HomeLatestTimeline はタイムラインの最新ツイートを取得する (TwitterGraphQLAPI.homeLatestTimeline 相当) 。
//
// 戻り値は *TimelineTweetsResult か *TwitterAPIResult のいずれか。
func (client *GraphQLClient) HomeLatestTimeline(ctx context.Context, cursorID *string, cursorType string, seenTweetIDs []string) any {
	if cursorID != nil && cursorType == "Top" {
		elapsedSeconds := client.currentTime() - client.lastHomeFetchedAt
		if elapsedSeconds < MinimumFetchIntervalSeconds {
			return &TimelineTweetsResult{
				IsSuccess:        true,
				Detail:           fmt.Sprintf("取得制限により抑制されました (経過: %.1f 秒)", elapsedSeconds),
				Tweets:           []Tweet{},
				NewerCursorID:    nil,
				LoadMoreCursors:  []TimelineLoadMoreCursor{},
				IsCursorConsumed: false,
			}
		}
	}

	variables := NewOrderedMap()
	additionalFlags := (*OrderedMap)(nil)
	if cursorID == nil {
		variables.Set("count", 20)
	} else if cursorType == "Top" {
		variables.Set("count", 40)
	} else {
		variables.Set("count", 20)
	}
	if cursorID != nil {
		variables.Set("cursor", *cursorID)
	}
	variables.Set("enableRanking", false)
	variables.Set("includePromotedContent", true)
	if cursorID == nil {
		variables.Set("requestContext", "launch")
	}
	if len(seenTweetIDs) > 0 {
		variables.Set("seenTweetIds", stringSliceToAny(seenTweetIDs))
		additionalFlags = NewOrderedMap()
		additionalFlags.Set("forcePost", true)
	}

	response := client.invokeOrResult(ctx, "HomeLatestTimeline", variables, additionalFlags, "タイムラインの取得に失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return client.buildTimelineResult(ctx, response, cursorID, cursorType, true)
}

// SearchTimeline はツイートを検索する (TwitterGraphQLAPI.searchTimeline 相当) 。
func (client *GraphQLClient) SearchTimeline(ctx context.Context, searchType string, query string, cursorID *string, cursorType string) any {
	if cursorID != nil && cursorType == "Top" {
		elapsedSeconds := client.currentTime() - client.lastSearchFetchedAt
		if elapsedSeconds < MinimumFetchIntervalSeconds {
			return &TimelineTweetsResult{
				IsSuccess:        true,
				Detail:           fmt.Sprintf("取得制限により抑制されました (経過: %.1f 秒)", elapsedSeconds),
				Tweets:           []Tweet{},
				NewerCursorID:    nil,
				LoadMoreCursors:  []TimelineLoadMoreCursor{},
				IsCursorConsumed: false,
			}
		}
	}

	variables := NewOrderedMap()
	variables.Set("rawQuery", strings.TrimSpace(query)+" lang:ja -filter:replies")
	if cursorID == nil {
		variables.Set("count", 20)
	} else if cursorType == "Top" {
		variables.Set("count", 40)
	} else {
		variables.Set("count", 20)
	}
	if cursorID != nil {
		variables.Set("cursor", *cursorID)
	}
	variables.Set("querySource", "typed_query")
	variables.Set("product", searchType)
	variables.Set("withGrokTranslatedBio", false)

	response := client.invokeOrResult(ctx, "SearchTimeline", variables, nil, "ツイートの検索に失敗しました。")
	if result, ok := response.(*TwitterAPIResult); ok {
		return result
	}
	return client.buildTimelineResult(ctx, response, cursorID, cursorType, false)
}

// buildTimelineResult はタイムライン系レスポンスから TimelineTweetsResult を組み立てる。
func (client *GraphQLClient) buildTimelineResult(ctx context.Context, response any, cursorID *string, cursorType string, isHome bool) any {
	responseObject, ok := response.(map[string]any)
	if !ok {
		return &TwitterAPIResult{IsSuccess: false, Detail: "タイムラインの取得に失敗しました。レスポンスの形式が不正です。"}
	}

	newerCursorID, loadMoreCursors, err := GetCursorsFromTimelineAPIResponse(responseObject)
	if err != nil {
		client.logger.Error("failed to get cursors from timeline response", "error", err, "prefix", client.LogPrefix())
		return &TwitterAPIResult{IsSuccess: false, Detail: "タイムラインの取得に失敗しました。レスポンスの形式が不正です。"}
	}
	tweets, err := GetTweetsFromTimelineAPIResponse(responseObject)
	if err != nil {
		client.logger.Error("failed to get tweets from timeline response", "error", err, "prefix", client.LogPrefix())
		return &TwitterAPIResult{IsSuccess: false, Detail: "タイムラインの取得に失敗しました。レスポンスの形式が不正です。"}
	}

	if cursorID == nil || cursorType == "Top" {
		if isHome {
			client.lastHomeFetchedAt = client.currentTime()
		} else {
			client.lastSearchFetchedAt = client.currentTime()
		}
	}

	detail := "Twitter のタイムラインを取得しました。"
	if !isHome {
		detail = "Twitter のツイートを検索しました。"
	}
	return &TimelineTweetsResult{
		IsSuccess:        true,
		Detail:           detail,
		Tweets:           tweets,
		NewerCursorID:    newerCursorID,
		LoadMoreCursors:  loadMoreCursors,
		IsCursorConsumed: true,
	}
}

// invokeOrResult は InvokeGraphQLAPI を呼び、失敗 (日本語メッセージ) の場合は *TwitterAPIResult に変換して返す。
//
// 戻り値は data (any) か *TwitterAPIResult のいずれか。
func (client *GraphQLClient) invokeOrResult(ctx context.Context, endpointName string, variables *OrderedMap, additionalFlags *OrderedMap, errorMessagePrefix string) any {
	data, err := client.InvokeGraphQLAPI(ctx, endpointName, variables, additionalFlags, errorMessagePrefix)
	if err != nil {
		message, ok := AsGraphQLErrorMessage(err)
		if !ok {
			client.logger.Error("unexpected error while invoking GraphQL API", "error", err, "prefix", client.LogPrefix())
			message = errorMessagePrefix + "予期しないエラーが発生しました。"
		}
		client.logger.Error(fmt.Sprintf("%s Failed to invoke %s: %s", client.LogPrefix(), endpointName, message))
		return &TwitterAPIResult{IsSuccess: false, Detail: message}
	}
	return data
}

// ----------------------------------------------------------------------------
// variables / additional_flags ビルダー (挿入順を Twitter Web App に厳密に合わせる)
// ----------------------------------------------------------------------------

func buildViewerVariables() *OrderedMap {
	variables := NewOrderedMap()
	variables.Set("withCommunitiesMemberships", true)
	return variables
}

func buildViewerAdditionalFlags() *OrderedMap {
	flags := NewOrderedMap()
	fieldToggles := NewOrderedMap()
	fieldToggles.Set("isDelegate", false)
	fieldToggles.Set("withAuxiliaryUserLabels", true)
	flags.Set("fieldToggles", fieldToggles)
	return flags
}

func buildRetweetVariables(tweetID string) (*OrderedMap, error) {
	variables := NewOrderedMap()
	variables.Set("tweet_id", tweetID)
	variables.Set("dark_request", false)
	return variables, nil
}

func buildDeleteRetweetVariables(tweetID string) (*OrderedMap, error) {
	variables := NewOrderedMap()
	variables.Set("source_tweet_id", tweetID)
	variables.Set("dark_request", false)
	return variables, nil
}

func buildTweetIDVariables(tweetID string) (*OrderedMap, error) {
	variables := NewOrderedMap()
	variables.Set("tweet_id", tweetID)
	return variables, nil
}

// stringSliceToAny は []string を Python の list 相当の []any に変換する。
func stringSliceToAny(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

// maxFloat は 2 つの float64 の大きい方を返す。
func maxFloat(a float64, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
