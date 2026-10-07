// Package twitter は Twitter Web App 連携 (server/app/utils/TwitterGraphQLAPI.py 相当) の Go 実装。
//
// Python 版はヘッドレスブラウザ (Zendriver) 経由で Twitter Web App 自身の内部 GraphQL クライアントを
// 呼び出している。Go 版ではブラウザ操作を Backend インターフェースとして抽象化し、テストでは
// httptest やフェイク実装に差し替えられるようにしている。
//
// 重要: GraphQL のクエリ ID・HTTP ヘッダー・X-Client-Transaction-ID は Twitter Web App の
// JavaScript (main.js) が内部的に付与する値であり、Python 版のコードにも静的な定義は存在しない。
// そのため Go 版ではこれらを再現せず、Backend 実装側 (ブラウザ駆動) の責務としている。
package twitter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// TwitterAPIResult は schemas.TwitterAPIResult と互換のレスポンス。
type TwitterAPIResult struct {
	IsSuccess bool   `json:"is_success"`
	Detail    string `json:"detail"`
}

// TweetUser は schemas.TweetUser と互換。
type TweetUser struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	ScreenName string `json:"screen_name"`
	IconURL    string `json:"icon_url"`
}

// Tweet は schemas.Tweet と互換 (フィールド順も Python 版と同じに保つ) 。
type Tweet struct {
	Source         string    `json:"source"`
	ID             string    `json:"id"`
	CreatedAt      JSONTime  `json:"created_at"`
	User           TweetUser `json:"user"`
	Text           string    `json:"text"`
	Lang           string    `json:"lang"`
	Via            string    `json:"via"`
	ImageURLs      []string  `json:"image_urls"`
	MovieURL       *string   `json:"movie_url"`
	RetweetCount   int       `json:"retweet_count"`
	Retweeted      bool      `json:"retweeted"`
	FavoriteCount  int       `json:"favorite_count"`
	Favorited      bool      `json:"favorited"`
	RetweetedTweet *Tweet    `json:"retweeted_tweet"`
	QuotedTweet    *Tweet    `json:"quoted_tweet"`
}

// PostTweetResult は schemas.PostTweetResult と互換。
type PostTweetResult struct {
	IsSuccess bool    `json:"is_success"`
	Detail    string  `json:"detail"`
	TweetURL  string  `json:"tweet_url"`
	TweetID   *string `json:"tweet_id"`
	PostURI   *string `json:"post_uri"`
	PostCID   *string `json:"post_cid"`
}

// TimelineLoadMoreCursor は schemas.TimelineLoadMoreCursor と互換。
type TimelineLoadMoreCursor struct {
	CursorType     string    `json:"cursor_type"`
	CursorID       string    `json:"cursor_id"`
	EntryID        *string   `json:"entry_id"`
	UpperCreatedAt *JSONTime `json:"upper_created_at"`
	LowerCreatedAt *JSONTime `json:"lower_created_at"`
}

// TimelineTweetsResult は schemas.TimelineTweetsResult と互換。
type TimelineTweetsResult struct {
	IsSuccess        bool                     `json:"is_success"`
	Detail           string                   `json:"detail"`
	Tweets           []Tweet                  `json:"tweets"`
	NewerCursorID    *string                  `json:"newer_cursor_id"`
	LoadMoreCursors  []TimelineLoadMoreCursor `json:"load_more_cursors"`
	IsCursorConsumed bool                     `json:"is_cursor_consumed"`
}

// JSONTime は Pydantic v2 / FastAPI と同じ ISO8601 (JST) 形式で日時を JSON 出力する time.Time。
// 例: "2026-10-07T21:00:00+09:00"
type JSONTime struct {
	time.Time
}

// MarshalJSON は Pydantic v2 と同じ形式で日時を出力する。
func (value JSONTime) MarshalJSON() ([]byte, error) {
	inJST := value.Time.In(constants.JST)
	var text string
	if inJST.Nanosecond() == 0 {
		text = inJST.Format("2006-01-02T15:04:05-07:00")
	} else {
		text = inJST.Format("2006-01-02T15:04:05.000000-07:00")
	}
	return json.Marshal(text)
}

// UnmarshalJSON は ISO8601 形式の日時文字列を読み込む (テスト用) 。
func (value *JSONTime) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return err
	}
	value.Time = parsed
	return nil
}

// jsonTimeOf は time.Time から JST の JSONTime を作る。
func jsonTimeOf(value time.Time) JSONTime {
	return JSONTime{Time: value.In(constants.JST)}
}

// pyStr は Python の str() 相当の文字列化を行う (Pydantic の緩い型変換の再現用) 。
func pyStr(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed)), true
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", typed), "0"), "."), true
	case bool:
		if typed {
			return "True", true
		}
		return "False", true
	}
	return "", false
}
