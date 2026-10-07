package twitter

import (
	"fmt"
	"regexp"
	"time"
)

// tweetCreatedAtLayout は Twitter の created_at ("Wed Oct 07 12:00:00 +0000 2026") を
// 解釈するための Go の time layout (Python の '%a %b %d %H:%M:%S %z %Y' 相当) 。
const tweetCreatedAtLayout = "Mon Jan 02 15:04:05 -0700 2006"

// tcoSuffixPattern は画像 / 動画付きツイートの本文末尾に残る t.co URL を除去するための正規表現。
// Python 版の re.sub(r'\s*https://t\.co/\w+$', ”, text) 相当。
// Go の正規表現 (RE2) は Python と異なり \w が ASCII のみのため、
// Unicode の単語文字を明示的に指定している。
var tcoSuffixPattern = regexp.MustCompile(`\s*https://t[.]co/[\p{L}\p{N}_]+$`)

// htmlTagPattern はツイートの via (source) から HTML タグを除去するための正規表現。
var htmlTagPattern = regexp.MustCompile(`<.+?>`)

// getTimelineInstructions はタイムライン系レスポンスから命令列を取得する。
func getTimelineInstructions(response map[string]any) []any {
	if home, ok := getObject(response, "home"); ok {
		if timeline, ok := getObject(home, "home_timeline_urt"); ok {
			return getListDefault(timeline, "instructions")
		}
		return []any{}
	}
	if search, ok := getObject(response, "search_by_raw_query"); ok {
		if searchTimeline, ok := getObject(search, "search_timeline"); ok {
			if timeline, ok := getObject(searchTimeline, "timeline"); ok {
				return getListDefault(timeline, "instructions")
			}
		}
		return []any{}
	}
	// それ以外のレスポンス (通常あり得ないため、ここに到達した場合はレスポンス構造が変わった可能性が高い)
	return nil
}

// getRawTweetObjectFromTimelineItemContent はタイムライン項目から生のツイートオブジェクトを取得する。
func getRawTweetObjectFromTimelineItemContent(itemContent map[string]any) map[string]any {
	if pyString(itemContent["itemType"]) != "TimelineTweet" {
		return nil
	}
	tweetResults, ok := getObject(itemContent, "tweet_results")
	if !ok {
		return nil
	}
	result, ok := getObject(tweetResults, "result")
	if !ok {
		return nil
	}
	typename := pyString(result["__typename"])
	if typename == "Tweet" || typename == "TweetWithVisibilityResults" {
		return result
	}
	return nil
}

// getRawTweetObjectFromTimelineModule はタイムラインモジュールから代表ツイート 1 件分の生オブジェクトを取得する。
func getRawTweetObjectFromTimelineModule(content map[string]any) map[string]any {
	// Twitter Web App はスレッドや返信チェーンを TimelineTimelineModule として返す
	if pyString(content["entryType"]) != "TimelineTimelineModule" || pyString(content["displayType"]) != "VerticalConversation" {
		return nil
	}

	rawTweetObjects := []map[string]any{}
	for _, itemValue := range getListDefault(content, "items") {
		moduleItem, ok := itemValue.(map[string]any)
		if !ok {
			continue
		}
		moduleItemBody, ok := getObject(moduleItem, "item")
		if !ok {
			continue
		}
		itemContent, ok := getObject(moduleItemBody, "itemContent")
		if !ok {
			// レスポンス形状が少し変わった場合でも、同じ項目内の itemContent だけは候補にする
			itemContent, ok = getObject(moduleItem, "itemContent")
			if !ok {
				continue
			}
		}
		if rawTweetObject := getRawTweetObjectFromTimelineItemContent(itemContent); rawTweetObject != nil {
			rawTweetObjects = append(rawTweetObjects, rawTweetObject)
		}
	}

	if len(rawTweetObjects) == 0 {
		return nil
	}

	// 返信チェーンではタイムラインに流れてきた最新投稿を代表として抜き出す
	var newestRawTweetObject map[string]any
	var newestCreatedAt *time.Time
	for _, rawTweetObject := range rawTweetObjects {
		createdAt, err := getCreatedAtFromRawTweetObject(rawTweetObject)
		if err != nil {
			continue
		}
		if newestCreatedAt == nil || createdAt.After(*newestCreatedAt) {
			newestRawTweetObject = rawTweetObject
			newestCreatedAt = createdAt
		}
	}
	if newestRawTweetObject != nil {
		return newestRawTweetObject
	}
	return rawTweetObjects[len(rawTweetObjects)-1]
}

// getRawTweetObjectFromTimelineEntry はタイムラインエントリから生のツイートオブジェクトを取得する。
func getRawTweetObjectFromTimelineEntry(entry map[string]any) map[string]any {
	// 広告ツイートは表示対象外なので、境界計算にも含めない
	if hasStringPrefix(pyString(entry["entryId"]), "promoted-") {
		return nil
	}
	content, ok := getObject(entry, "content")
	if !ok {
		return nil
	}
	if pyString(content["entryType"]) == "TimelineTimelineItem" {
		itemContent, ok := getObject(content, "itemContent")
		if !ok {
			return nil
		}
		return getRawTweetObjectFromTimelineItemContent(itemContent)
	}
	return getRawTweetObjectFromTimelineModule(content)
}

// getCreatedAtFromRawTweetObject は生のツイートオブジェクトから作成日時を取得する。
func getCreatedAtFromRawTweetObject(rawTweetObject map[string]any) (*time.Time, error) {
	// 可視性制限付きツイートは実体が tweet に入るため、通常ツイートと同じ位置へ揃える
	if pyString(rawTweetObject["__typename"]) == "TweetWithVisibilityResults" {
		tweet, ok := getObject(rawTweetObject, "tweet")
		if !ok {
			return nil, fmt.Errorf("tweet object not found in TweetWithVisibilityResults")
		}
		rawTweetObject = tweet
	}
	legacy, ok := getObject(rawTweetObject, "legacy")
	if !ok {
		return nil, fmt.Errorf("legacy not found in raw tweet object")
	}
	createdAtText, ok := legacy["created_at"].(string)
	if !ok {
		return nil, fmt.Errorf("created_at is not a string")
	}
	return parseTweetCreatedAt(createdAtText)
}

// parseTweetCreatedAt は Twitter の created_at 文字列をパースする。
func parseTweetCreatedAt(value string) (*time.Time, error) {
	parsed, err := time.Parse(tweetCreatedAtLayout, value)
	if err != nil {
		return nil, err
	}
	// Python 版は astimezone(JST) するため、JST に変換した時刻を保持する
	converted := parsed.In(jstZone)
	return &converted, nil
}

// GetTweetsFromTimelineAPIResponse はタイムライン系レスポンスからツイートリストを取得する
// (TwitterGraphQLAPI.__getTweetsFromTimelineAPIResponse 相当) 。
func GetTweetsFromTimelineAPIResponse(response map[string]any) ([]Tweet, error) {
	tweets := []Tweet{}
	for _, instructionValue := range getTimelineInstructions(response) {
		instruction, ok := instructionValue.(map[string]any)
		if !ok {
			continue
		}
		switch pyString(instruction["type"]) {
		case "TimelineAddEntries":
			for _, entryValue := range getListDefault(instruction, "entries") {
				entry, ok := entryValue.(map[string]any)
				if !ok {
					continue
				}
				rawTweetObject := getRawTweetObjectFromTimelineEntry(entry)
				if rawTweetObject == nil {
					continue
				}
				tweet, err := formatTweet(rawTweetObject)
				if err != nil {
					return nil, err
				}
				tweets = append(tweets, *tweet)
			}
		case "TimelineReplaceEntry":
			entry, ok := getObject(instruction, "entry")
			if !ok {
				continue
			}
			rawTweetObject := getRawTweetObjectFromTimelineEntry(entry)
			if rawTweetObject == nil {
				continue
			}
			tweet, err := formatTweet(rawTweetObject)
			if err != nil {
				return nil, err
			}
			tweets = append(tweets, *tweet)
		}
	}
	return tweets, nil
}

// formatTweet は API レスポンスから取得したツイート情報を Tweet に変換する。
func formatTweet(rawTweetObject map[string]any) (*Tweet, error) {
	// '__typename' が 'TweetWithVisibilityResults' なら、ツイート情報がさらにネストされている
	if pyString(rawTweetObject["__typename"]) == "TweetWithVisibilityResults" {
		nested, ok := getObject(rawTweetObject, "tweet")
		if !ok {
			return nil, fmt.Errorf("tweet not found in TweetWithVisibilityResults")
		}
		rawTweetObject = nested
	}

	legacy, ok := getObject(rawTweetObject, "legacy")
	if !ok {
		return nil, fmt.Errorf("legacy not found in tweet object")
	}

	// リツイートがある場合は、リツイート元のツイートの情報を取得
	var retweetedTweet *Tweet
	if retweetedStatusResult, ok := getObject(legacy, "retweeted_status_result"); ok {
		inner, ok := getObject(retweetedStatusResult, "result")
		if !ok {
			return nil, fmt.Errorf("result not found in retweeted_status_result")
		}
		formatted, err := formatTweet(inner)
		if err != nil {
			return nil, err
		}
		retweetedTweet = formatted
	}

	// 引用リツイートがある場合は、引用リツイート元のツイートの情報を取得
	var quotedTweet *Tweet
	if quotedStatusResult, ok := getObject(rawTweetObject, "quoted_status_result"); ok {
		inner, ok := getObject(quotedStatusResult, "result")
		if !ok {
			// ごく稀に quoted_status_result.result が空のツイート情報が返ってくるので、その場合は警告を出した上で無視する
			// (Python 版は logging.warning のみで処理を継続する)
		} else {
			formatted, err := formatTweet(inner)
			if err != nil {
				return nil, err
			}
			quotedTweet = formatted
		}
	}

	// 画像の URL を取得
	imageURLs := []string{}
	var movieURL *string
	if extendedEntities, ok := getObject(legacy, "extended_entities"); ok {
		for _, mediaValue := range getListDefault(extendedEntities, "media") {
			media, ok := mediaValue.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("media is not an object")
			}
			switch pyString(media["type"]) {
			case "photo":
				mediaURL, ok := media["media_url_https"].(string)
				if !ok {
					return nil, fmt.Errorf("media_url_https is not a string")
				}
				imageURLs = append(imageURLs, mediaURL)
			case "video", "animated_gif":
				selected, err := selectHighestBitrateMP4(media)
				if err != nil {
					return nil, err
				}
				if selected != nil {
					movieURL = selected
				}
			}
		}
	}

	// t.co の URL を展開した URL に置換
	fullText, ok := legacy["full_text"].(string)
	if !ok {
		return nil, fmt.Errorf("full_text is not a string")
	}
	expandedText := fullText
	if entities, ok := getObject(legacy, "entities"); ok {
		for _, urlEntityValue := range getListDefault(entities, "urls") {
			urlEntity, ok := urlEntityValue.(map[string]any)
			if !ok {
				continue
			}
			expandedURL, ok := urlEntity["expanded_url"].(string)
			if !ok {
				continue
			}
			shortURL, ok := urlEntity["url"].(string)
			if !ok {
				return nil, fmt.Errorf("url is not a string")
			}
			expandedText = replaceAll(expandedText, shortURL, expandedURL)
		}
	}

	// 残った t.co の URL を削除
	if len(imageURLs) > 0 || movieURL != nil {
		expandedText = tcoSuffixPattern.ReplaceAllString(expandedText, "")
	}

	createdAtText, ok := legacy["created_at"].(string)
	if !ok {
		return nil, fmt.Errorf("created_at is not a string")
	}
	createdAt, err := parseTweetCreatedAt(createdAtText)
	if err != nil {
		return nil, err
	}

	idString, ok := legacy["id_str"].(string)
	if !ok {
		return nil, fmt.Errorf("id_str is not a string")
	}

	user, err := extractTweetUser(rawTweetObject)
	if err != nil {
		return nil, err
	}

	lang, _ := legacy["lang"].(string)
	source, _ := rawTweetObject["source"].(string)
	retweetCount, err := requireInt(legacy["retweet_count"])
	if err != nil {
		return nil, err
	}
	favoriteCount, err := requireInt(legacy["favorite_count"])
	if err != nil {
		return nil, err
	}
	retweeted, err := requireBool(legacy["retweeted"])
	if err != nil {
		return nil, err
	}
	favorited, err := requireBool(legacy["favorited"])
	if err != nil {
		return nil, err
	}

	tweet := &Tweet{
		Source:         "Twitter",
		ID:             idString,
		CreatedAt:      jsonTimeOf(*createdAt),
		User:           *user,
		Text:           expandedText,
		Lang:           lang,
		Via:            htmlTagPattern.ReplaceAllString(source, ""),
		RetweetCount:   retweetCount,
		Retweeted:      retweeted,
		FavoriteCount:  favoriteCount,
		Favorited:      favorited,
		RetweetedTweet: retweetedTweet,
		QuotedTweet:    quotedTweet,
		MovieURL:       movieURL,
	}
	if len(imageURLs) > 0 {
		tweet.ImageURLs = imageURLs
	}
	return tweet, nil
}

// extractTweetUser はツイートオブジェクトからユーザー情報を取り出す。
func extractTweetUser(rawTweetObject map[string]any) (*TweetUser, error) {
	core, ok := getObject(rawTweetObject, "core")
	if !ok {
		return nil, fmt.Errorf("core not found in tweet object")
	}
	userResults, ok := getObject(core, "user_results")
	if !ok {
		return nil, fmt.Errorf("user_results not found in tweet object")
	}
	result, ok := getObject(userResults, "result")
	if !ok {
		return nil, fmt.Errorf("user result not found in tweet object")
	}
	userCore, ok := getObject(result, "core")
	if !ok {
		return nil, fmt.Errorf("user core not found in tweet object")
	}
	avatar, ok := getObject(result, "avatar")
	if !ok {
		return nil, fmt.Errorf("avatar not found in tweet object")
	}
	name, _ := userCore["name"].(string)
	screenName, _ := userCore["screen_name"].(string)
	iconURL, _ := avatar["image_url"].(string)
	restID, ok := pyStr(result["rest_id"])
	if !ok {
		return nil, fmt.Errorf("rest_id not found in tweet object")
	}
	return &TweetUser{
		Source:     "Twitter",
		ID:         restID,
		Name:       name,
		ScreenName: screenName,
		// (ランダムな文字列)_normal.jpg だと画像サイズが小さいので、(ランダムな文字列).jpg に置換
		IconURL: replaceAll(iconURL, "_normal", ""),
	}, nil
}

// selectHighestBitrateMP4 は video / animated_gif メディアから最高ビットレートの mp4 URL を選ぶ。
func selectHighestBitrateMP4(media map[string]any) (*string, error) {
	videoInfo, ok := getObject(media, "video_info")
	if !ok {
		return nil, fmt.Errorf("video_info not found in media")
	}
	var highest *string
	highestBitrate := -1
	for _, variantValue := range getListDefault(videoInfo, "variants") {
		variant, ok := variantValue.(map[string]any)
		if !ok || pyString(variant["content_type"]) != "video/mp4" {
			continue
		}
		bitrate := 0
		if bitrateValue, exists := variant["bitrate"]; exists {
			parsed, err := requireInt(bitrateValue)
			if err != nil {
				return nil, err
			}
			bitrate = parsed
		}
		if bitrate > highestBitrate {
			if url, ok := variant["url"].(string); ok {
				selected := url
				highest = &selected
				highestBitrate = bitrate
			}
		}
	}
	return highest, nil
}

// GetCursorsFromTimelineAPIResponse はタイムライン系レスポンスから KonomiTV が扱うカーソル情報を取得する
// (TwitterGraphQLAPI.__getCursorsFromTimelineAPIResponse 相当) 。
func GetCursorsFromTimelineAPIResponse(response map[string]any) (*string, []TimelineLoadMoreCursor, error) {
	type parsedEntry struct {
		entryType  string
		createdAt  *time.Time
		entryID    *string
		cursorType string
		cursorID   string
	}

	timelineEntries := []map[string]any{}
	for _, instructionValue := range getTimelineInstructions(response) {
		instruction, ok := instructionValue.(map[string]any)
		if !ok {
			continue
		}
		switch pyString(instruction["type"]) {
		case "TimelineAddEntries":
			for _, entryValue := range getListDefault(instruction, "entries") {
				if entry, ok := entryValue.(map[string]any); ok {
					timelineEntries = append(timelineEntries, entry)
				}
			}
		case "TimelineReplaceEntry":
			if entry, ok := getObject(instruction, "entry"); ok {
				timelineEntries = append(timelineEntries, entry)
			}
		}
	}

	parsedEntries := []parsedEntry{}
	for _, entry := range timelineEntries {
		content, ok := getObject(entry, "content")
		if !ok {
			continue
		}
		rawTweetObject := getRawTweetObjectFromTimelineEntry(entry)
		if rawTweetObject != nil {
			createdAt, err := getCreatedAtFromRawTweetObject(rawTweetObject)
			if err != nil {
				return nil, nil, err
			}
			parsedEntries = append(parsedEntries, parsedEntry{entryType: "Tweet", createdAt: createdAt})
			continue
		}
		if pyString(content["entryType"]) != "TimelineTimelineCursor" {
			continue
		}
		twitterCursorType := pyString(content["cursorType"])
		cursorID, ok := content["value"].(string)
		if !ok {
			continue
		}
		switch twitterCursorType {
		case "Top", "Bottom", "Gap", "ShowMore":
			entryID, _ := entry["entryId"].(string)
			var entryIDPointer *string
			if _, exists := entry["entryId"]; exists {
				entryIDPointer = &entryID
			}
			parsedEntries = append(parsedEntries, parsedEntry{
				entryType:  "Cursor",
				entryID:    entryIDPointer,
				cursorType: twitterCursorType,
				cursorID:   cursorID,
			})
		case "ShowMoreThreads", "ShowMoreThreadsPrompt":
			// スレッド展開用カーソルは追加取得ボタンでは使わないため無視する
		}
	}

	var newerCursorID *string
	loadMoreCursors := []TimelineLoadMoreCursor{}
	for index, entry := range parsedEntries {
		if entry.entryType != "Cursor" {
			continue
		}
		if entry.cursorType == "Top" {
			selected := entry.cursorID
			newerCursorID = &selected
			continue
		}
		cursorType := map[string]string{"Bottom": "Older", "Gap": "Gap", "ShowMore": "ShowMore"}[entry.cursorType]
		if cursorType == "" {
			continue
		}

		// カーソルの前後にある最も近い投稿を時刻境界にする
		var upperCreatedAt *time.Time
		var lowerCreatedAt *time.Time
		for upperIndex := index - 1; upperIndex >= 0; upperIndex-- {
			if parsedEntries[upperIndex].createdAt != nil {
				upperCreatedAt = parsedEntries[upperIndex].createdAt
				break
			}
		}
		for lowerIndex := index + 1; lowerIndex < len(parsedEntries); lowerIndex++ {
			if parsedEntries[lowerIndex].createdAt != nil {
				lowerCreatedAt = parsedEntries[lowerIndex].createdAt
				break
			}
		}

		cursor := TimelineLoadMoreCursor{
			CursorType: cursorType,
			CursorID:   entry.cursorID,
			EntryID:    entry.entryID,
		}
		if upperCreatedAt != nil {
			value := jsonTimeOf(*upperCreatedAt)
			cursor.UpperCreatedAt = &value
		}
		if lowerCreatedAt != nil {
			value := jsonTimeOf(*lowerCreatedAt)
			cursor.LowerCreatedAt = &value
		}
		loadMoreCursors = append(loadMoreCursors, cursor)
	}

	return newerCursorID, loadMoreCursors, nil
}
