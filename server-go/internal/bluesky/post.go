package bluesky

import (
	"encoding/json"
	"fmt"
)

// PostAuthor は app.bsky.actor.defs#profileViewBasic のサブセット。
type PostAuthor struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Avatar      string `json:"avatar"`
}

// ReasonRepost は app.bsky.feed.defs#reasonRepost 。
type ReasonRepost struct {
	By PostAuthor `json:"by"`
}

// PostViewer は app.bsky.feed.defs#viewerState のサブセット。
type PostViewer struct {
	Repost *string `json:"repost"`
	Like   *string `json:"like"`
}

// PostView は app.bsky.feed.defs#postView 。
type PostView struct {
	URI         string          `json:"uri"`
	CID         string          `json:"cid"`
	Author      PostAuthor      `json:"author"`
	Record      json.RawMessage `json:"record"`
	Embed       json.RawMessage `json:"embed"`
	ReplyCount  *int            `json:"replyCount"`
	RepostCount *int            `json:"repostCount"`
	LikeCount   *int            `json:"likeCount"`
	IndexedAt   string          `json:"indexedAt"`
	Viewer      *PostViewer     `json:"viewer"`
}

// postRecord は app.bsky.feed.post のレコード (受信用) 。
type postRecord struct {
	Text      string  `json:"text"`
	CreatedAt string  `json:"createdAt"`
	Facets    []Facet `json:"facets"`
}

// ParsePostView は正規化済みの raw レスポンスから PostView を組み立てる。
func ParsePostView(raw map[string]any) (*PostView, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var post PostView
	if err := json.Unmarshal(encoded, &post); err != nil {
		return nil, fmt.Errorf("failed to parse PostView: %w", err)
	}
	return &post, nil
}

// postRecord は record が app.bsky.feed.post の場合のみその内容を返す。
func (p *PostView) postRecord() (*postRecord, error) {
	if len(p.Record) == 0 {
		return nil, nil
	}
	var header struct {
		Type string `json:"$type"`
	}
	if err := json.Unmarshal(p.Record, &header); err != nil {
		return nil, err
	}
	if header.Type != "app.bsky.feed.post" {
		return nil, nil
	}
	var record postRecord
	if err := json.Unmarshal(p.Record, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// imageURLsFromEmbed は画像系 embed から表示用サムネイル URL を取り出す。
// Python 版と同じく images#view は thumb を、gallery#view は thumbnail を拾う。
func imageURLsFromEmbed(embed json.RawMessage) []string {
	if len(embed) == 0 {
		return nil
	}
	var header struct {
		Type string `json:"$type"`
	}
	if err := json.Unmarshal(embed, &header); err != nil {
		return nil
	}

	var imageURLs []string
	switch header.Type {
	case "app.bsky.embed.images#view":
		var parsed struct {
			Images []struct {
				Thumb string `json:"thumb"`
			} `json:"images"`
		}
		if err := json.Unmarshal(embed, &parsed); err != nil {
			return nil
		}
		for _, image := range parsed.Images {
			imageURLs = append(imageURLs, image.Thumb)
		}
	case "app.bsky.embed.gallery#view":
		// gallery embed は現行 Lexicon 上すべて画像項目なので、表示用サムネイル URL をそのまま拾う
		var parsed struct {
			Items []struct {
				Thumbnail string `json:"thumbnail"`
			} `json:"items"`
		}
		if err := json.Unmarshal(embed, &parsed); err != nil {
			return nil
		}
		for _, item := range parsed.Items {
			imageURLs = append(imageURLs, item.Thumbnail)
		}
	default:
		return nil
	}

	if len(imageURLs) == 0 {
		return nil
	}
	return imageURLs
}

// FormatPostView は Bluesky の PostView を KonomiTV 共通の Tweet スキーマへ変換する。
// Python 版 BlueskyAPI._formatPostView() の移植。
func FormatPostView(post *PostView, reason *ReasonRepost) (*Tweet, error) {
	// Bluesky のフィード投稿は実態として常に app.bsky.feed.post レコードだが、
	// それ以外のレコードは本文と投稿日時にアクセスできないため空文字と indexed_at で代替する
	text := ""
	createdAtText := post.IndexedAt
	if record, err := post.postRecord(); err != nil {
		return nil, err
	} else if record != nil {
		text = ExpandFacetLinksInText(record.Text, record.Facets)
		createdAtText = record.CreatedAt
	}

	createdAt, err := ParseDateTime(createdAtText)
	if err != nil {
		return nil, err
	}

	repostCount := 0
	if post.RepostCount != nil {
		repostCount = *post.RepostCount
	}
	likeCount := 0
	if post.LikeCount != nil {
		likeCount = *post.LikeCount
	}

	tweet := &Tweet{
		Source:        "Bluesky",
		ID:            post.URI,
		CreatedAt:     NewJSTDateTime(createdAt),
		User:          tweetUserFromAuthor(post.Author),
		Text:          text,
		Lang:          "",
		Via:           "",
		ImageURLs:     imageURLsFromEmbed(post.Embed),
		MovieURL:      nil,
		RetweetCount:  repostCount,
		FavoriteCount: likeCount,
		Retweeted:     post.Viewer != nil && post.Viewer.Repost != nil,
		Favorited:     post.Viewer != nil && post.Viewer.Like != nil,
	}

	// Bluesky のリポストは Twitter の RT セマンティクスに合わせて、
	// リポストした人を投稿者として表示しつつ元の投稿を retweeted_tweet にネストする
	if reason != nil {
		original := *tweet
		tweet.User = tweetUserFromAuthor(reason.By)
		tweet.RetweetedTweet = &original
	}

	return tweet, nil
}

// tweetUserFromAuthor は PostAuthor を schemas.TweetUser へ変換する。
func tweetUserFromAuthor(author PostAuthor) TweetUser {
	name := author.DisplayName
	if name == "" {
		name = author.Handle
	}
	return TweetUser{
		Source:     "Bluesky",
		ID:         author.DID,
		Name:       name,
		ScreenName: author.Handle,
		IconURL:    author.Avatar,
	}
}
