package bluesky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Service は 1 つの Bluesky アカウントに対する API 操作を提供する共有クライアント。
type Service struct {
	manager *Manager
	account *Account
	client  *Client

	// mu はセッション復元の排他制御に使う。
	mu sync.Mutex
	// isSessionRestored は保存済みセッションの復元が完了しているかどうか。
	isSessionRestored bool
	// isSessionChangeHandlerRegistered はセッション更新通知を登録済みかどうか。
	isSessionChangeHandlerRegistered bool
}

// Account は現在操作対象のアカウントを返す。
func (s *Service) Account() *Account {
	return s.account
}

// logPrefix はログ出力時にアカウントを識別する接頭辞を返す。
func (s *Service) logPrefix() string {
	return "[BlueskyAPI][" + s.account.Handle + "]"
}

// restoreSession は API 操作前に Bluesky セッションを復元する。
// 復元に失敗した場合は API 結果を返す (成功時は nil) 。
// Python 版 BlueskyAPI._restoreSession() の移植。
func (s *Service) restoreSession(ctx context.Context) *TwitterAPIResult {
	if s.isSessionRestored {
		return nil
	}

	// 同じアカウントへの初回リクエストが重なった場合も、セッション復元とプロフィール取得は 1 回だけ実行する
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isSessionRestored {
		return nil
	}

	// セッション更新通知は追加登録型なので、再試行時に登録すると保存処理が重複する
	if !s.isSessionChangeHandlerRegistered {
		s.client.SetOnSessionChange(func(event SessionEvent, session *Session) error {
			if event != SessionEventCreate && event != SessionEventRefresh {
				return nil
			}
			encrypted, err := s.manager.encryptSessionString(session.Encode())
			if err != nil {
				return err
			}
			s.account.SessionString = encrypted
			// コールバックにはリクエストの context が無いため、保存だけ独立した context で行う
			if err := UpdateAccountSessionString(context.Background(), s.manager.writeDB, s.account.ID, encrypted); err != nil {
				return err
			}
			s.manager.logf("%s Bluesky session string updated.", s.logPrefix())
			return nil
		})
		s.isSessionChangeHandlerRegistered = true
	}

	decrypted, err := s.manager.decryptSessionString(s.account.SessionString)
	if err != nil {
		// 復号不能な session_string はユーザーの再連携で解消する状態
		s.manager.logf("%s Failed to restore Bluesky session due to authorization error: %v", s.logPrefix(), err)
		return &TwitterAPIResult{
			IsSuccess: false,
			Detail:    "Bluesky のセッションが期限切れです。設定画面から再連携してください。",
		}
	}

	if _, err := s.client.ImportSessionString(ctx, decrypted); err != nil {
		// PDS 側の認証拒否はユーザーの再連携で解消する状態
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.isUnauthorized() {
			s.manager.logf("%s Failed to restore Bluesky session due to authorization error: %v", s.logPrefix(), err)
			return &TwitterAPIResult{
				IsSuccess: false,
				Detail:    "Bluesky のセッションが期限切れです。設定画面から再連携してください。",
			}
		}
		s.manager.logf("%s Failed to restore Bluesky session: %v", s.logPrefix(), err)
		return &TwitterAPIResult{
			IsSuccess: false,
			Detail:    "Bluesky との通信エラーまたは内部エラーが発生しました。後でもう一度お試しください。",
		}
	}

	s.isSessionRestored = true
	return nil
}

// nowForTest は現在時刻を差し替えるためのフック (テスト用) 。
// service.go 内の currentTimeISO() は投稿レコードの createdAt に現在時刻を埋め込むため、
// フィクスチャとの照合時はこれを固定する。
var nowForTest func() time.Time

// currentTimeISO は atproto SDK の get_current_time_iso() と同じ形式の現在時刻を返す。
func currentTimeISO() string {
	now := time.Now()
	if nowForTest != nil {
		now = nowForTest()
	}
	return FormatUTCDateTimeISO(now)
}

// FormatUTCDateTimeISO は datetime.now(timezone.utc).isoformat() 互換の文字列を返す。
func FormatUTCDateTimeISO(value time.Time) string {
	utc := value.UTC()
	text := utc.Format("2006-01-02T15:04:05")
	if microsecond := utc.Nanosecond() / 1000; microsecond != 0 {
		text += fmt.Sprintf(".%06d", microsecond)
	}
	return text + "+00:00"
}

// buildTimelineResult は Tweet 配列から TimelineTweetsResult を組み立てる。
func buildTimelineResult(tweets []Tweet, cursor *string, detail string) *TimelineTweetsResult {
	loadMoreCursors := []TimelineLoadMoreCursor{}
	if cursor != nil {
		var upperCreatedAt *JSTDateTime
		if len(tweets) > 0 {
			value := tweets[len(tweets)-1].CreatedAt
			upperCreatedAt = &value
		}
		loadMoreCursors = append(loadMoreCursors, TimelineLoadMoreCursor{
			CursorType:     "Older",
			CursorID:       *cursor,
			UpperCreatedAt: upperCreatedAt,
		})
	}
	return &TimelineTweetsResult{
		IsSuccess:        true,
		Detail:           detail,
		Tweets:           tweets,
		NewerCursorID:    nil,
		LoadMoreCursors:  loadMoreCursors,
		IsCursorConsumed: true,
	}
}

// parseReasonRepost は feedViewPost の reason を ReasonRepost として解釈する。
// ReasonRepost 以外 (reasonPin など) はリポスト表示の対象外なので nil を返す。
func parseReasonRepost(raw []byte) *ReasonRepost {
	if len(raw) == 0 {
		return nil
	}
	var header struct {
		Type string `json:"$type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil
	}
	if header.Type != "app.bsky.feed.defs#reasonRepost" {
		return nil
	}
	var reason ReasonRepost
	if err := json.Unmarshal(raw, &reason); err != nil {
		return nil
	}
	return &reason
}

// HomeLatestTimeline は Bluesky のホームタイムラインを取得する。
// Python 版 BlueskyAPI.homeLatestTimeline() の移植。
func (s *Service) HomeLatestTimeline(ctx context.Context, cursorID *string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	params := url.Values{}
	params.Set("limit", "30")
	if cursorID != nil {
		params.Set("cursor", *cursorID)
	}

	var response struct {
		Cursor string `json:"cursor"`
		Feed   []struct {
			Post   map[string]any  `json:"post"`
			Reason json.RawMessage `json:"reason"`
		} `json:"feed"`
	}
	if err := s.client.invokeQueryRaw(ctx, "app.bsky.feed.getTimeline", params, &response); err != nil {
		s.manager.logf("%s Failed to fetch home timeline: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のタイムライン取得に失敗しました。"}, nil
	}

	tweets := make([]Tweet, 0, len(response.Feed))
	for _, item := range response.Feed {
		post, err := ParsePostView(item.Post)
		if err != nil {
			s.manager.logf("%s Failed to parse timeline post: %v", s.logPrefix(), err)
			return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のタイムライン取得に失敗しました。"}, nil
		}
		tweet, err := FormatPostView(post, parseReasonRepost(item.Reason))
		if err != nil {
			s.manager.logf("%s Failed to format timeline post: %v", s.logPrefix(), err)
			return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のタイムライン取得に失敗しました。"}, nil
		}
		tweets = append(tweets, *tweet)
	}

	cursor := optionalString(response.Cursor)
	return buildTimelineResult(tweets, cursor, "Bluesky のタイムラインを取得しました。"), nil
}

// SearchTimeline は Bluesky の投稿を検索する。
// Python 版 BlueskyAPI.searchTimeline() の移植。
func (s *Service) SearchTimeline(ctx context.Context, query string, cursorID *string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("limit", "30")
	params.Set("sort", "latest")
	if cursorID != nil {
		params.Set("cursor", *cursorID)
	}

	var response struct {
		Cursor string           `json:"cursor"`
		Posts  []map[string]any `json:"posts"`
	}
	if err := s.client.invokeQueryRaw(ctx, "app.bsky.feed.searchPosts", params, &response); err != nil {
		s.manager.logf("%s Failed to search posts: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky の検索に失敗しました。"}, nil
	}

	// search_posts は reason を持たない PostView 配列なので、リポスト表示用の変換は通さない
	tweets := make([]Tweet, 0, len(response.Posts))
	for _, rawPost := range response.Posts {
		post, err := ParsePostView(rawPost)
		if err != nil {
			s.manager.logf("%s Failed to parse search post: %v", s.logPrefix(), err)
			return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky の検索に失敗しました。"}, nil
		}
		tweet, err := FormatPostView(post, nil)
		if err != nil {
			s.manager.logf("%s Failed to format search post: %v", s.logPrefix(), err)
			return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky の検索に失敗しました。"}, nil
		}
		tweets = append(tweets, *tweet)
	}

	cursor := optionalString(response.Cursor)
	return buildTimelineResult(tweets, cursor, "Bluesky の検索結果を取得しました。"), nil
}

// CreatePost は Bluesky にポストを送信する。
// Python 版 BlueskyAPI.createPost() の移植。
func (s *Service) CreatePost(ctx context.Context, text string, images []ImageInput, replyTo *ReplyReference) (any, error) {
	// Bluesky の画像 embed は最大 4 枚なので、アップロード処理に入る前に明示的に拒否する
	if len(images) > maxImageCount {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky では画像は 4 枚まで添付できます。"}, nil
	}

	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	// 画像 embed と facets 付き本文を組み立ててから投稿し、Bluesky 上で URL / ハッシュタグがリンク化されるようにする
	buildText, facets := BuildFacets(text)

	var embed *EmbedImages
	if len(images) > 0 {
		embedImages := make([]EmbedImage, 0, len(images))
		for _, image := range images {
			prepared, err := s.prepareAndUploadImage(ctx, image)
			if err != nil {
				s.manager.logf("%s Failed to create post: %v", s.logPrefix(), err)
				return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky へのポストに失敗しました。"}, nil
			}
			embedImages = append(embedImages, *prepared)
		}
		embed = &EmbedImages{Images: embedImages, Type: "app.bsky.embed.images"}
	}

	var replyRef *ReplyRef
	if replyTo != nil {
		// Bluesky のリプライはツリー全体のルートと直前の親ポストの両方を要求する
		replyRef = &ReplyRef{
			Parent: StrongRef{CID: replyTo.ParentCID, URI: replyTo.ParentURI, Type: "com.atproto.repo.strongRef"},
			Root:   StrongRef{CID: replyTo.RootCID, URI: replyTo.RootURI, Type: "com.atproto.repo.strongRef"},
			Type:   "app.bsky.feed.post#replyRef",
		}
	}

	session := s.client.Session()
	if session == nil {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky へのポストに失敗しました。"}, nil
	}

	record := &PostRecord{
		CreatedAt: currentTimeISO(),
		Text:      buildText,
		Embed:     embed,
		Facets:    facets,
		Langs:     []string{"ja"},
		Reply:     replyRef,
		Type:      "app.bsky.feed.post",
	}
	input := CreateRecordInput{Collection: "app.bsky.feed.post", Record: record, Repo: session.DID, Validate: true}

	var result CreateRecordResult
	if err := s.client.invokeProcedureRaw(ctx, "com.atproto.repo.createRecord", input, &result); err != nil {
		s.manager.logf("%s Failed to create post: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky へのポストに失敗しました。"}, nil
	}

	// 投稿完了通知から直接開けるよう、AT URI の record key を bsky.app の URL へ変換する
	postURL := "https://bsky.app/profile/" + s.account.Handle + "/post/" + ExtractRecordKey(result.URI)
	uri := result.URI
	cid := result.CID
	return &PostTweetResult{
		IsSuccess: true,
		Detail:    "Bluesky にポストしました。",
		TweetURL:  postURL,
		PostURI:   &uri,
		PostCID:   &cid,
	}, nil
}

// fetchPostByURI は AT URI から Bluesky 投稿情報を取得する。
// Python 版 BlueskyAPI._fetchPostByURI() の移植。
func (s *Service) fetchPostByURI(ctx context.Context, postURI string) (*PostView, error) {
	// API パスから受け取る ID は AT URI そのものなので、不正な文字列は Bluesky API に渡す前に弾く
	if !strings.HasPrefix(postURI, "at://") {
		return nil, nil
	}

	params := url.Values{}
	params.Set("uris", postURI)
	var response struct {
		Posts []map[string]any `json:"posts"`
	}
	if err := s.client.invokeQueryRaw(ctx, "app.bsky.feed.getPosts", params, &response); err != nil {
		return nil, err
	}
	if len(response.Posts) == 0 {
		return nil, nil
	}
	return ParsePostView(response.Posts[0])
}

// CreateRepost は Bluesky の投稿をリポストする。
// Python 版 BlueskyAPI.createRepost() の移植。
func (s *Service) CreateRepost(ctx context.Context, postID string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	// リポスト作成には StrongRef (URI + CID) が必要なので、AT URI から現在の CID を取得する
	post, err := s.fetchPostByURI(ctx, postID)
	if err != nil {
		s.manager.logf("%s Failed to repost: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のリポストに失敗しました。"}, nil
	}
	if post == nil {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky 投稿情報を取得できませんでした。"}, nil
	}

	if err := s.createSubjectRecord(ctx, "app.bsky.feed.repost", post); err != nil {
		s.manager.logf("%s Failed to repost: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のリポストに失敗しました。"}, nil
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "Bluesky の投稿をリポストしました。"}, nil
}

// DeleteRepost は Bluesky のリポストを取り消す。
// Python 版 BlueskyAPI.deleteRepost() の移植。
func (s *Service) DeleteRepost(ctx context.Context, postID string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	post, err := s.fetchPostByURI(ctx, postID)
	if err != nil {
		s.manager.logf("%s Failed to delete repost: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のリポスト取り消しに失敗しました。"}, nil
	}
	if post == nil || post.Viewer == nil || post.Viewer.Repost == nil {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のリポスト情報を取得できませんでした。"}, nil
	}

	if err := s.deleteRecordByURI(ctx, "app.bsky.feed.repost", *post.Viewer.Repost); err != nil {
		s.manager.logf("%s Failed to delete repost: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のリポスト取り消しに失敗しました。"}, nil
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "Bluesky のリポストを取り消しました。"}, nil
}

// FavoritePost は Bluesky の投稿をいいねする。
// Python 版 BlueskyAPI.favoritePost() の移植。
func (s *Service) FavoritePost(ctx context.Context, postID string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	post, err := s.fetchPostByURI(ctx, postID)
	if err != nil {
		s.manager.logf("%s Failed to like post: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のいいねに失敗しました。"}, nil
	}
	if post == nil {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky 投稿情報を取得できませんでした。"}, nil
	}

	if err := s.createSubjectRecord(ctx, "app.bsky.feed.like", post); err != nil {
		s.manager.logf("%s Failed to like post: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のいいねに失敗しました。"}, nil
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "Bluesky の投稿をいいねしました。"}, nil
}

// UnfavoritePost は Bluesky のいいねを取り消す。
// Python 版 BlueskyAPI.unfavoritePost() の移植。
func (s *Service) UnfavoritePost(ctx context.Context, postID string) (any, error) {
	if result := s.restoreSession(ctx); result != nil {
		return result, nil
	}

	post, err := s.fetchPostByURI(ctx, postID)
	if err != nil {
		s.manager.logf("%s Failed to delete like: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のいいね取り消しに失敗しました。"}, nil
	}
	if post == nil || post.Viewer == nil || post.Viewer.Like == nil {
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のいいね情報を取得できませんでした。"}, nil
	}

	if err := s.deleteRecordByURI(ctx, "app.bsky.feed.like", *post.Viewer.Like); err != nil {
		s.manager.logf("%s Failed to delete like: %v", s.logPrefix(), err)
		return &TwitterAPIResult{IsSuccess: false, Detail: "Bluesky のいいね取り消しに失敗しました。"}, nil
	}
	return &TwitterAPIResult{IsSuccess: true, Detail: "Bluesky のいいねを取り消しました。"}, nil
}

// createSubjectRecord は StrongRef を subject に持つレコード (repost / like) を作成する。
func (s *Service) createSubjectRecord(ctx context.Context, collection string, post *PostView) error {
	session := s.client.Session()
	if session == nil {
		return errors.New("login required")
	}
	subject := StrongRef{CID: post.CID, URI: post.URI, Type: "com.atproto.repo.strongRef"}
	createdAt := currentTimeISO()

	var record any
	switch collection {
	case "app.bsky.feed.repost":
		record = &RepostRecord{CreatedAt: createdAt, Subject: subject, Type: "app.bsky.feed.repost"}
	default:
		record = &LikeRecord{CreatedAt: createdAt, Subject: subject, Type: "app.bsky.feed.like"}
	}
	input := CreateRecordInput{Collection: collection, Record: record, Repo: session.DID, Validate: true}

	var result CreateRecordResult
	return s.client.invokeProcedureRaw(ctx, "com.atproto.repo.createRecord", input, &result)
}

// deleteRecordByURI は AT URI が示すレコードを削除する (repost / like の取り消し) 。
func (s *Service) deleteRecordByURI(ctx context.Context, collection string, uri string) error {
	session := s.client.Session()
	if session == nil {
		return errors.New("login required")
	}
	input := DeleteRecordInput{Collection: collection, Repo: session.DID, Rkey: ExtractRecordKey(uri)}
	// 204 No Content が返るためレスポンスボディは使わない
	return s.client.invokeProcedureRaw(ctx, "com.atproto.repo.deleteRecord", input, nil)
}

// optionalString は空文字列を nil に変換する (Python の `value or None` に相当) 。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
