package twitter

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestTweetScenarios は createTweet の出力と compose UI へのリクエストを期待値と比較する。
func TestTweetScenarios(t *testing.T) {
	fixture := loadExpectedFixture(t)

	for _, scenario := range fixture.TweetScenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			graphqlAPIResult := rawResponseFromFixture(t, scenario.Raw)
			composeResult := &ComposeResult{
				IsSuccess:          true,
				ComposeSubmittedAt: floatPointer(1759800000.0),
				GraphQLAPIResult:   graphqlAPIResult,
			}
			backend := newFakeBackend()
			backend.composeResults = []*ComposeResult{composeResult}
			client := newTestClient(t, backend, nil)

			images := make([]UploadedImage, 0, len(scenario.Images))
			for _, image := range scenario.Images {
				images = append(images, UploadedImage{Filename: image[0], ContentType: image[1]})
			}

			actual := client.CreateTweet(context.Background(), scenario.Tweet, images, scenario.InReplyToStatusID)

			// compose UI へのリクエストの検証
			if len(backend.composeCalls) != 1 {
				t.Fatalf("expected 1 compose call, got %d", len(backend.composeCalls))
			}
			composeCall := backend.composeCalls[0]
			actualCompose := map[string]any{
				"tweet_text":                 composeCall.TweetText,
				"images":                     imagesForJSON(uploadedImagesToPair(composeCall.Images)),
				"throttle_remaining_seconds": composeCall.ThrottleRemainingSec,
				"in_reply_to_status_id":      composeCall.InReplyToStatusID,
			}
			jsonEqual(t, scenario.ComposeCall, actualCompose)

			// 出力の検証
			jsonEqual(t, scenario.Result, actual)
		})
	}
}

// TestCreateTweetConsecutiveFailuresRestart は連続失敗が閾値に達したらブラウザを再起動することを確認する。
func TestCreateTweetConsecutiveFailuresRestart(t *testing.T) {
	backend := newFakeBackend()
	backend.composeResults = []*ComposeResult{
		{IsSuccess: false, ErrorMessage: stringPointer("1回目の失敗")},
		{IsSuccess: false, ErrorMessage: stringPointer("2回目の失敗")},
	}
	client := newTestClient(t, backend, nil)
	client.consecutiveComposeFailures = 1 // 既に 1 回失敗している状態

	first := client.CreateTweet(context.Background(), "test", nil, nil)
	result, ok := first.(*TwitterAPIResult)
	if !ok || result.IsSuccess {
		t.Fatalf("expected failure result, got %#v", first)
	}
	mustContain(t, result.Detail, "Twitter へのツイートに失敗しました。")

	second := client.CreateTweet(context.Background(), "test", nil, nil)
	result, ok = second.(*TwitterAPIResult)
	if !ok || result.IsSuccess {
		t.Fatalf("expected failure result, got %#v", second)
	}
	if backend.markForRestart == 0 {
		t.Error("expected browser to be marked for restart after consecutive failures")
	}
}

// TestCreateTweetThrottle は最小送信間隔の残り時間が compose UI へ渡ることを確認する。
func TestCreateTweetThrottle(t *testing.T) {
	backend := newFakeBackend()
	backend.composeResults = []*ComposeResult{{
		IsSuccess:          true,
		ComposeSubmittedAt: floatPointer(1759800020.0),
		GraphQLAPIResult: rawResponseFromFixture(t, rawResponse{
			ParsedResponse: json.RawMessage(`{"data": {"create_tweet": {"tweet_results": {"result": {"rest_id": "99"}}}}}`),
			StatusCode:     intPointer(200),
		}),
	}}
	client := newTestClient(t, backend, nil)
	now := float64(1759800000)
	client.SetNowForTesting(func() time.Time { return time.Unix(int64(now), 0) })
	client.SetRandomFloatForTesting(func(minimum float64, maximum float64) float64 { return 2.0 })
	// 直前のツイートから 10 秒しか経っていない → 20 秒間隔に達していない
	client.SetLastTweetTimeForTesting(now - 10)

	client.CreateTweet(context.Background(), "test", nil, nil)

	if len(backend.composeCalls) != 1 {
		t.Fatalf("expected 1 compose call, got %d", len(backend.composeCalls))
	}
	// minimum_interval_end = last + 20 = now + 10, jitter = 2.0 → deadline = now + 12 → remaining = 12.0
	remaining := backend.composeCalls[0].ThrottleRemainingSec
	if remaining < 11.99 || remaining > 12.01 {
		t.Errorf("expected throttle_remaining_seconds ≈ 12.0, got %v", remaining)
	}
}

// helpers -------------------------------------------------------------------

// imagesForJSON は [[filename, content_type], ...] を JSON 比較用の any へ変換する。
func imagesForJSON(pairs [][]string) any {
	items := make([]any, 0, len(pairs))
	for _, pair := range pairs {
		items = append(items, []any{pair[0], pair[1]})
	}
	return items
}

// uploadedImagesToPair は UploadedImage のスライスを [filename, content_type] の配列へ変換する。
func uploadedImagesToPair(images []UploadedImage) [][]string {
	pairs := make([][]string, 0, len(images))
	for _, image := range images {
		pairs = append(pairs, []string{image.Filename, image.ContentType})
	}
	return pairs
}

func floatPointer(value float64) *float64 { return &value }
func stringPointer(value string) *string  { return &value }
func intPointer(value int) *int           { return &value }
