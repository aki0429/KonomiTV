package bluesky

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// JST は Asia/Tokyo の固定タイムゾーン (server/app/constants.py の JST と同じ) 。
var JST = constants.JST

// JSTDateTime は Python 版が Pydantic 経由で出力する datetime (JST, ISO 8601) を再現する型。
//
// Python 版は datetime.isoformat() で出力するため、マイクロ秒が 0 の場合は小数部を省略する。
// (例: "2026-10-07T12:34:56+09:00" / "2026-10-07T12:34:56.789000+09:00")
type JSTDateTime struct {
	Time time.Time
}

// NewJSTDateTime は time.Time から JSTDateTime を生成する。
func NewJSTDateTime(value time.Time) JSTDateTime {
	return JSTDateTime{Time: value.In(JST)}
}

// MarshalJSON は Python の datetime.isoformat() と同じ形式で出力する。
func (d JSTDateTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + FormatJSTDateTimeISO(d.Time, "T") + `"`), nil
}

// FormatJSTDateTimeISO は datetime.isoformat(sep) 互換の文字列を返す。
func FormatJSTDateTimeISO(value time.Time, separator string) string {
	localized := value.In(JST)
	text := localized.Format("2006-01-02" + separator + "15:04:05")
	if microsecond := localized.Nanosecond() / 1000; microsecond != 0 {
		text += fmt.Sprintf(".%06d", microsecond)
	}
	return text + localized.Format("-07:00")
}

// ParseDateTime は Bluesky API の日時文字列を JST の time.Time に変換する。
// Python 版 BlueskyAPI._parseDateTime() と同じく 'Z' を '+00:00' に置換してから解釈し、JST へ変換する。
func ParseDateTime(text string) (time.Time, error) {
	normalized := strings.ReplaceAll(text, "Z", "+00:00")
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999-0700",
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, normalized); err == nil {
			return parsed.In(JST), nil
		}
	}
	return time.Time{}, fmt.Errorf("failed to parse datetime: %s", text)
}

// ***** Bluesky のレスポンス・リクエストモデル *****

// Profile は app.bsky.actor.defs#profileViewBasic (または profileView) のサブセット。
type Profile struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Avatar      string `json:"avatar"`
}

// Name は Python 版と同じく display_name が空なら handle を返す。
func (p *Profile) Name() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Handle
}

// AvatarURL は Python 版と同じく avatar が空なら空文字を返す。
func (p *Profile) AvatarURL() string {
	return p.Avatar
}

// StrongRef は com.atproto.repo.strongRef 。
type StrongRef struct {
	CID  string `json:"cid"`
	URI  string `json:"uri"`
	Type string `json:"$type"`
}

// ReplyRef は app.bsky.feed.post#replyRef 。
type ReplyRef struct {
	Parent StrongRef `json:"parent"`
	Root   StrongRef `json:"root"`
	Type   string    `json:"$type"`
}

// ReplyReference は KonomiTV 側で扱うリプライ先情報。
type ReplyReference struct {
	RootURI   string
	RootCID   string
	ParentURI string
	ParentCID string
}

// AspectRatio は app.bsky.embed.defs#aspectRatio 。
type AspectRatio struct {
	Height int    `json:"height"`
	Width  int    `json:"width"`
	Type   string `json:"$type"`
}

// EmbedImage は app.bsky.embed.images#image 。
type EmbedImage struct {
	Alt         string          `json:"alt"`
	Image       json.RawMessage `json:"image"`
	AspectRatio AspectRatio     `json:"aspectRatio"`
	Type        string          `json:"$type"`
}

// EmbedImages は app.bsky.embed.images 。
type EmbedImages struct {
	Images []EmbedImage `json:"images"`
	Type   string       `json:"$type"`
}

// PostRecord は app.bsky.feed.post のレコード (送信用) 。
// フィールドの並び順は atproto SDK の JSON 出力と同じにする。
type PostRecord struct {
	CreatedAt string       `json:"createdAt"`
	Text      string       `json:"text"`
	Embed     *EmbedImages `json:"embed,omitempty"`
	Facets    []Facet      `json:"facets"`
	Langs     []string     `json:"langs"`
	Reply     *ReplyRef    `json:"reply,omitempty"`
	Type      string       `json:"$type"`
}

// RepostRecord は app.bsky.feed.repost のレコード (送信用) 。
type RepostRecord struct {
	CreatedAt string    `json:"createdAt"`
	Subject   StrongRef `json:"subject"`
	Type      string    `json:"$type"`
}

// LikeRecord は app.bsky.feed.like のレコード (送信用) 。
type LikeRecord struct {
	CreatedAt string    `json:"createdAt"`
	Subject   StrongRef `json:"subject"`
	Type      string    `json:"$type"`
}

// CreateRecordInput は com.atproto.repo.createRecord のリクエストボディ。
// フィールドの並び順は atproto SDK の JSON 出力と同じにする。
type CreateRecordInput struct {
	Collection string `json:"collection"`
	Record     any    `json:"record"`
	Repo       string `json:"repo"`
	Validate   bool   `json:"validate"`
}

// CreateRecordResult は com.atproto.repo.createRecord のレスポンス。
type CreateRecordResult struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// DeleteRecordInput は com.atproto.repo.deleteRecord のリクエストボディ。
type DeleteRecordInput struct {
	Collection string `json:"collection"`
	Repo       string `json:"repo"`
	Rkey       string `json:"rkey"`
}

// UploadBlobResult は com.atproto.repo.uploadBlob のレスポンス。
// blob はそのまま再送できるよう raw JSON で保持する。
type UploadBlobResult struct {
	Blob json.RawMessage `json:"blob"`
}

// ***** KonomiTV 共通スキーマ (schemas.py) *****

// TweetUser は schemas.TweetUser 互換。
type TweetUser struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	ScreenName string `json:"screen_name"`
	IconURL    string `json:"icon_url"`
}

// Tweet は schemas.Tweet 互換。
// フィールドの並び順は schemas.Tweet の定義順 (JSON 出力順) と同じにする。
type Tweet struct {
	Source         string      `json:"source"`
	ID             string      `json:"id"`
	CreatedAt      JSTDateTime `json:"created_at"`
	User           TweetUser   `json:"user"`
	Text           string      `json:"text"`
	Lang           string      `json:"lang"`
	Via            string      `json:"via"`
	ImageURLs      []string    `json:"image_urls"`
	MovieURL       *string     `json:"movie_url"`
	RetweetCount   int         `json:"retweet_count"`
	Retweeted      bool        `json:"retweeted"`
	FavoriteCount  int         `json:"favorite_count"`
	Favorited      bool        `json:"favorited"`
	RetweetedTweet *Tweet      `json:"retweeted_tweet"`
	QuotedTweet    *Tweet      `json:"quoted_tweet"`
}

// TwitterAPIResult は schemas.TwitterAPIResult 互換。
type TwitterAPIResult struct {
	IsSuccess bool   `json:"is_success"`
	Detail    string `json:"detail"`
}

// PostTweetResult は schemas.PostTweetResult 互換。
type PostTweetResult struct {
	IsSuccess bool    `json:"is_success"`
	Detail    string  `json:"detail"`
	TweetURL  string  `json:"tweet_url"`
	TweetID   *string `json:"tweet_id"`
	PostURI   *string `json:"post_uri"`
	PostCID   *string `json:"post_cid"`
}

// TimelineLoadMoreCursor は schemas.TimelineLoadMoreCursor 互換。
type TimelineLoadMoreCursor struct {
	CursorType     string       `json:"cursor_type"`
	CursorID       string       `json:"cursor_id"`
	EntryID        *string      `json:"entry_id"`
	UpperCreatedAt *JSTDateTime `json:"upper_created_at"`
	LowerCreatedAt *JSTDateTime `json:"lower_created_at"`
}

// TimelineTweetsResult は schemas.TimelineTweetsResult 互換。
type TimelineTweetsResult struct {
	IsSuccess        bool                     `json:"is_success"`
	Detail           string                   `json:"detail"`
	Tweets           []Tweet                  `json:"tweets"`
	NewerCursorID    *string                  `json:"newer_cursor_id"`
	LoadMoreCursors  []TimelineLoadMoreCursor `json:"load_more_cursors"`
	IsCursorConsumed bool                     `json:"is_cursor_consumed"`
}

// maxImageCount は Bluesky の画像 embed で許可される画像枚数。
const maxImageCount = 4

// maxImageBytes は app.bsky.embed.images の blob 上限。
const maxImageBytes = 2_000_000

// ImageInput はアップロードする 1 枚の画像。
type ImageInput struct {
	Data        []byte
	ContentType string
}
