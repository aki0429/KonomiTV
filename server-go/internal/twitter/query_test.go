package twitter

import (
	"context"
	"testing"
	"time"
)

// TestQueryScenarios は各エンドポイントメソッドの
// リクエスト組み立て (endpoint / variables / additional_flags) と出力を期待値と比較する。
func TestQueryScenarios(t *testing.T) {
	fixture := loadExpectedFixture(t)

	for _, scenario := range fixture.QueryScenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			backend := newFakeBackend(rawResponseFromFixture(t, scenario.Raw))
			client := newTestClient(t, backend, nil)
			ctx := context.Background()

			var actual any
			switch scenario.Method {
			case "fetchLoggedViewer":
				actual = client.FetchLoggedViewer(ctx)
			case "createRetweet":
				actual = client.CreateRetweet(ctx, stringValue(t, scenario.Kwargs, "tweet_id"))
			case "deleteRetweet":
				actual = client.DeleteRetweet(ctx, stringValue(t, scenario.Kwargs, "tweet_id"))
			case "favoriteTweet":
				actual = client.FavoriteTweet(ctx, stringValue(t, scenario.Kwargs, "tweet_id"))
			case "unfavoriteTweet":
				actual = client.UnfavoriteTweet(ctx, stringValue(t, scenario.Kwargs, "tweet_id"))
			case "homeLatestTimeline":
				actual = client.HomeLatestTimeline(
					ctx,
					optionalStringValue(t, scenario.Kwargs, "cursor_id"),
					stringOr(t, scenario.Kwargs, "cursor_type", "Top"),
					stringSliceValue(t, scenario.Kwargs, "seen_tweet_ids"),
				)
			case "searchTimeline":
				actual = client.SearchTimeline(
					ctx,
					stringValue(t, scenario.Kwargs, "search_type"),
					stringValue(t, scenario.Kwargs, "query"),
					optionalStringValue(t, scenario.Kwargs, "cursor_id"),
					stringOr(t, scenario.Kwargs, "cursor_type", "Top"),
				)
			default:
				t.Fatalf("unknown method: %s", scenario.Method)
			}

			// リクエストの検証
			if len(backend.calls) != 1 {
				t.Fatalf("expected 1 GraphQL call, got %d", len(backend.calls))
			}
			call := backend.calls[0]
			if call.endpoint != scenario.Endpoint {
				t.Errorf("endpoint mismatch: got %q, want %q", call.endpoint, scenario.Endpoint)
			}
			expectedVariables := pythonJSONOf(t, scenario.Variables.Raw)
			actualVariables := pyJSONVariables(t, call.variables)
			if actualVariables != expectedVariables {
				t.Errorf("variables mismatch (順序を含む)\n got: %s\nwant: %s", actualVariables, expectedVariables)
			}
			expectedFlags := "null"
			if len(scenario.AdditionalFlags) > 0 && string(scenario.AdditionalFlags) != "null" {
				expectedFlags = pythonJSONOf(t, scenario.AdditionalFlags)
			}
			actualFlags := pyJSONVariables(t, call.additionalFlags)
			if actualFlags != expectedFlags {
				t.Errorf("additional_flags mismatch\n got: %s\nwant: %s", actualFlags, expectedFlags)
			}

			// 出力の検証
			jsonEqual(t, scenario.Result, actual)
		})
	}
}

// TestHomeTimelineThrottle は新着方向 (cursor_type=Top) の取得が最小間隔で抑制されるか確認する。
func TestHomeTimelineThrottle(t *testing.T) {
	backend := newFakeBackend()
	client := newTestClient(t, backend, nil)
	now := float64(1759800000)
	client.SetNowForTesting(func() time.Time { return time.Unix(int64(now), 0) })

	// カーソルなし初回は取得できる (抑制なし) … ただしレスポンスが無いので接続エラーになる
	// ここでは抑制の判定のみを確認するため、直前に取得時刻をセットする
	client.lastHomeFetchedAt = now - 10 // 10 秒前

	cursorID := "CURSOR"
	result := client.HomeLatestTimeline(context.Background(), &cursorID, "Top", nil)
	timelineResult, ok := result.(*TimelineTweetsResult)
	if !ok {
		t.Fatalf("expected *TimelineTweetsResult, got %T", result)
	}
	if timelineResult.IsCursorConsumed {
		t.Error("expected is_cursor_consumed=false when throttled")
	}
	if len(backend.calls) != 0 {
		t.Errorf("expected no GraphQL call when throttled, got %d", len(backend.calls))
	}
	mustContain(t, timelineResult.Detail, "取得制限により抑制されました")
}

// TestSearchTimelineThrottle は検索の新着方向が最小間隔で抑制されるか確認する。
func TestSearchTimelineThrottle(t *testing.T) {
	backend := newFakeBackend()
	client := newTestClient(t, backend, nil)
	now := float64(1759800000)
	client.SetNowForTesting(func() time.Time { return time.Unix(int64(now), 0) })
	client.lastSearchFetchedAt = now - 5

	cursorID := "SEARCH_CURSOR"
	result := client.SearchTimeline(context.Background(), "Latest", "新型車", &cursorID, "Top")
	timelineResult, ok := result.(*TimelineTweetsResult)
	if !ok {
		t.Fatalf("expected *TimelineTweetsResult, got %T", result)
	}
	if timelineResult.IsCursorConsumed {
		t.Error("expected is_cursor_consumed=false when throttled")
	}
	if len(backend.calls) != 0 {
		t.Errorf("expected no GraphQL call when throttled, got %d", len(backend.calls))
	}
}

// TestSearchTimelineNotThrottledForOlderCursor は古い方向の取得が抑制されないことを確認する。
func TestSearchTimelineNotThrottledForOlderCursor(t *testing.T) {
	// 検索レスポンスを返すフェイクを用意する
	fixture := loadExpectedFixture(t)
	var searchRaw rawResponse
	for _, scenario := range fixture.QueryScenarios {
		if scenario.Method == "searchTimeline" {
			searchRaw = scenario.Raw
			break
		}
	}
	if searchRaw.ParsedResponse == nil {
		t.Fatal("no searchTimeline scenario in fixture")
	}

	backend := newFakeBackend(rawResponseFromFixture(t, searchRaw))
	client := newTestClient(t, backend, nil)
	now := float64(1759800000)
	client.SetNowForTesting(func() time.Time { return time.Unix(int64(now), 0) })
	client.lastSearchFetchedAt = now - 5

	cursorID := "SEARCH_CURSOR"
	result := client.SearchTimeline(context.Background(), "Latest", "新型車", &cursorID, "Bottom")
	if _, ok := result.(*TimelineTweetsResult); !ok {
		t.Fatalf("expected *TimelineTweetsResult, got %T", result)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("expected 1 GraphQL call, got %d", len(backend.calls))
	}
}
