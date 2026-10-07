package bluesky

import "strings"

// atproto SDK が現在受け入れられる $type の集合。
// server/app/utils/BlueskyAPI.py が固定バージョンの SDK から実行時に収集している値を定数化したもの。
// (atproto==0.0.68 で収集した結果と一致することを TestSupportedUnionTypeTags で確認している)
var (
	// supportedMainEmbedTypes は app.bsky.feed.post の record.embed が取りうる $type 。
	supportedMainEmbedTypes = map[string]bool{
		"app.bsky.embed.external":        true,
		"app.bsky.embed.gallery":         true,
		"app.bsky.embed.images":          true,
		"app.bsky.embed.record":          true,
		"app.bsky.embed.recordWithMedia": true,
		"app.bsky.embed.video":           true,
	}
	// supportedViewEmbedTypes は app.bsky.feed.defs#postView の embed が取りうる $type 。
	supportedViewEmbedTypes = map[string]bool{
		"app.bsky.embed.external#view":        true,
		"app.bsky.embed.gallery#view":         true,
		"app.bsky.embed.images#view":          true,
		"app.bsky.embed.record#view":          true,
		"app.bsky.embed.recordWithMedia#view": true,
		"app.bsky.embed.video#view":           true,
	}
	// supportedFeedReasonTypes は app.bsky.feed.defs#feedViewPost の reason が取りうる $type 。
	supportedFeedReasonTypes = map[string]bool{
		"app.bsky.feed.defs#reasonPin":    true,
		"app.bsky.feed.defs#reasonRepost": true,
	}
)

// NormalizeUnknownEmbedForSDK は SDK 未対応の Bluesky embed だけをメディアなし embed へ正規化する。
// Python 版 _NormalizeUnknownEmbedForSDK() の移植。
func NormalizeUnknownEmbedForSDK(embed map[string]any, isViewEmbed *bool) {
	embedType, ok := embed["$type"].(string)
	if !ok {
		return
	}

	// recordWithMedia は media 内に別の embed Union を持つため、外側を保ったまま内側だけを処理する
	if embedType == "app.bsky.embed.recordWithMedia" || embedType == "app.bsky.embed.recordWithMedia#view" {
		if media, ok := embed["media"].(map[string]any); ok {
			isView := strings.HasSuffix(embedType, "#view")
			NormalizeUnknownEmbedForSDK(media, &isView)
		}
		return
	}

	// SDK がすでに知っている型は、将来の追加フィールドを含んでいてもそのまま SDK に渡す
	if supportedMainEmbedTypes[embedType] || supportedViewEmbedTypes[embedType] {
		return
	}

	shouldTreatAsViewEmbed := strings.HasSuffix(embedType, "#view")
	if isViewEmbed != nil {
		shouldTreatAsViewEmbed = *isViewEmbed
	}

	// 未知 embed は投稿本文まで巻き込んで落ちるため、既知の空画像 embed として扱う
	for key := range embed {
		delete(embed, key)
	}
	if shouldTreatAsViewEmbed {
		embed["$type"] = "app.bsky.embed.images#view"
	} else {
		embed["$type"] = "app.bsky.embed.images"
	}
	embed["images"] = []any{}
}

// NormalizeUnknownSchemaForSDK は API レスポンス内の SDK 未対応 Union 要素を再帰的に正規化する。
// Python 版 _NormalizeUnknownSchemaForSDK() の移植。
func NormalizeUnknownSchemaForSDK(value any) {
	switch typed := value.(type) {
	case map[string]any:
		// SDK が closed union で落ちる既知フィールドだけを触り、通常の拡張フィールドはそのまま残す
		if embed, ok := typed["embed"].(map[string]any); ok {
			isView := typed["$type"] != "app.bsky.feed.post"
			NormalizeUnknownEmbedForSDK(embed, &isView)
		}

		// quoted post の ViewRecord.embeds は view 側 embed の配列なので、未知要素だけをメディアなしにする
		if embeds, ok := typed["embeds"].([]any); ok {
			for _, child := range embeds {
				if embed, ok := child.(map[string]any); ok {
					isView := true
					NormalizeUnknownEmbedForSDK(embed, &isView)
				}
			}
		}

		// 未知の reason は表示理由が分からないだけなので、投稿自体を残すために理由なしとして扱う
		if reason, ok := typed["reason"].(map[string]any); ok {
			if reasonType, ok := reason["$type"].(string); ok && !supportedFeedReasonTypes[reasonType] {
				delete(typed, "reason")
			}
		}

		for _, child := range typed {
			NormalizeUnknownSchemaForSDK(child)
		}
	case []any:
		for _, child := range typed {
			NormalizeUnknownSchemaForSDK(child)
		}
	}
}
