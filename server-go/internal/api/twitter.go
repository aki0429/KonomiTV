package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/twitter"
)

// newTwitterBackendFactory は Twitter アカウントごとのブラウザ Backend を生成する。
//
// Go 版のヘッドレスブラウザ駆動は未実装のため、既定では UnavailableBackend を使う。
// テストではフェイク Backend を返すファクトリに差し替えてエンドポイントを検証する。
var newTwitterBackendFactory twitter.BackendFactory = twitter.NewUnavailableBackend

// twitterProxyUserAgent は Twitter 動画プロキシで送信する User-Agent
// (constants.py の API_REQUEST_HEADERS['User-Agent'] 相当) 。
var twitterProxyUserAgent = "KonomiTV/" + constants.Version

// twitterHTTPClientFactory は Twitter 動画プロキシに使う HTTP クライアントを生成する (テストで差し替える) 。
var twitterHTTPClientFactory = func() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// twitterCookieEncryptionPrefix は constants.py の TWITTER_ACCOUNT_COOKIE_ENCRYPTION_PREFIX と一致。
const twitterCookieEncryptionPrefix = "enc:"

// allowedVideoProxyDomains は Twitter 動画プロキシで許可するドメイン。
var allowedVideoProxyDomains = []string{"video.twimg.com", "pbs.twimg.com"}

// videoProxyRequestHeaders は上流へ転送するリクエストヘッダー。
var videoProxyRequestHeaders = []string{"Range", "Accept", "Accept-Encoding", "If-Range", "If-None-Match", "If-Modified-Since"}

// videoProxyResponseHeaders は下流へ転送するレスポンスヘッダー。
var videoProxyResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control", "ETag", "Last-Modified",
}

// registerTwitterRoutes は Twitter 関連のルートを mux に登録する。
//
// server.go の Handler() から呼び出す (server.go は他の担当者が編集するため、呼び出し 1 行のみを追加する) 。
func (s *Server) registerTwitterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/twitter/auth", s.handleTwitterCookieAuth)
	mux.HandleFunc("DELETE /api/twitter/accounts/{screen_name}", s.handleTwitterAccountDelete)
	mux.HandleFunc("POST /api/twitter/accounts/{screen_name}/keep-alive", s.handleTwitterKeepAlive)
	mux.HandleFunc("POST /api/twitter/accounts/{screen_name}/tweets", s.handleTwitterTweet)
	mux.HandleFunc("PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet", s.handleTwitterRetweet)
	mux.HandleFunc("DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet", s.handleTwitterRetweetCancel)
	mux.HandleFunc("PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite", s.handleTwitterFavorite)
	mux.HandleFunc("DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite", s.handleTwitterFavoriteCancel)
	mux.HandleFunc("GET /api/twitter/accounts/{screen_name}/timeline", s.handleTwitterTimeline)
	mux.HandleFunc("GET /api/twitter/accounts/{screen_name}/search", s.handleTwitterSearch)
	mux.HandleFunc("GET /api/twitter/video-proxy", s.handleTwitterVideoProxy)
}

// ----------------------------------------------------------------------------
// twitter_accounts テーブルへのアクセス
// ----------------------------------------------------------------------------

// twitterAccountRecord は twitter_accounts テーブルの 1 行。database.TwitterAccount に
// 認証情報カラムを加えたもの (internal/database は他担当のため、本ファイル内で SQL を直接扱う) 。
type twitterAccountRecord struct {
	ID                int64
	UserID            int64
	Name              string
	ScreenName        string
	IconURL           string
	AccessToken       string
	AccessTokenSecret string
	CookieBrowserInfo *string
}

// listTwitterAccountsByUserAndScreenName は指定ユーザー・スクリーンネームのレコードを取得する
// (TwitterRouter.GetCurrentTwitterAccount の TwitterAccount.filter(...).all() 相当) 。
func (s *Server) listTwitterAccountsByUserAndScreenName(ctx context.Context, userID int64, screenName string) ([]twitterAccountRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, screen_name, icon_url, access_token, access_token_secret, cookie_browser_info
		FROM twitter_accounts WHERE user_id = ? AND screen_name = ?
	`, userID, screenName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	records := make([]twitterAccountRecord, 0)
	for rows.Next() {
		var record twitterAccountRecord
		var cookieBrowserInfo sql.NullString
		if err := rows.Scan(
			&record.ID, &record.UserID, &record.Name, &record.ScreenName, &record.IconURL,
			&record.AccessToken, &record.AccessTokenSecret, &cookieBrowserInfo,
		); err != nil {
			return nil, err
		}
		if cookieBrowserInfo.Valid {
			record.CookieBrowserInfo = &cookieBrowserInfo.String
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// deleteTwitterAccount は Twitter アカウントのレコードを削除する。
func (s *Server) deleteTwitterAccount(ctx context.Context, id int64) error {
	_, err := s.writeDB.ExecContext(ctx, `DELETE FROM twitter_accounts WHERE id = ?`, id)
	return err
}

// updateTwitterAccount は Twitter アカウントのレコードを更新する。
func (s *Server) updateTwitterAccount(ctx context.Context, record twitterAccountRecord) error {
	_, err := s.writeDB.ExecContext(ctx, `
		UPDATE twitter_accounts
		SET name = ?, icon_url = ?, access_token = ?, access_token_secret = ?, cookie_browser_info = ?, updated_at = ?
		WHERE id = ?
	`, record.Name, record.IconURL, record.AccessToken, record.AccessTokenSecret, record.CookieBrowserInfo,
		database.NowForDB(), record.ID)
	return err
}

// insertTwitterAccount は Twitter アカウントのレコードを挿入し、採番された ID を返す。
func (s *Server) insertTwitterAccount(ctx context.Context, record twitterAccountRecord) (int64, error) {
	now := database.NowForDB()
	result, err := s.writeDB.ExecContext(ctx, `
		INSERT INTO twitter_accounts (user_id, name, screen_name, icon_url, access_token, access_token_secret, cookie_browser_info, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.UserID, record.Name, record.ScreenName, record.IconURL, record.AccessToken, record.AccessTokenSecret,
		record.CookieBrowserInfo, now, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// deleteOldFormatTwitterAccounts は古い形式 (access_token != NETSCAPE_COOKIE_FILE) のレコードを削除する。
func (s *Server) deleteOldFormatTwitterAccounts(ctx context.Context, userID int64) (int64, error) {
	result, err := s.writeDB.ExecContext(ctx,
		`DELETE FROM twitter_accounts WHERE user_id = ? AND access_token != 'NETSCAPE_COOKIE_FILE'`, userID)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ----------------------------------------------------------------------------
// 認証 / クライアント取得ヘルパー
// ----------------------------------------------------------------------------

// twitterAccountToModel は DB のレコードを twitter.Account に変換する。
func (s *Server) twitterAccountToModel(record twitterAccountRecord) *twitter.Account {
	account := &twitter.Account{
		ID:                record.ID,
		UserID:            record.UserID,
		Name:              record.Name,
		ScreenName:        record.ScreenName,
		IconURL:           record.IconURL,
		AccessToken:       record.AccessToken,
		AccessTokenSecret: record.AccessTokenSecret,
	}
	if record.CookieBrowserInfo != nil {
		account.CookieBrowserInfo = *record.CookieBrowserInfo
	}
	// Cookie が更新されたら暗号化して DB へ保存する
	account.PersistCookies = func(ctx context.Context, cookiesTxt string) error {
		encrypted, err := s.encryptTwitterCookie(cookiesTxt)
		if err != nil {
			return err
		}
		_, err = s.writeDB.ExecContext(ctx,
			`UPDATE twitter_accounts SET access_token_secret = ?, updated_at = ? WHERE id = ?`,
			encrypted, database.NowForDB(), record.ID)
		return err
	}
	return account
}

// getTwitterGraphQLClient は Twitter アカウントに紐づくシングルトンクライアントを取得する。
func (s *Server) getTwitterGraphQLClient(record twitterAccountRecord) *twitter.GraphQLClient {
	return twitter.GetInstance(s.twitterAccountToModel(record), newTwitterBackendFactory, s.logger)
}

// twitterHTTPClient は Twitter 動画プロキシに使う HTTP クライアントを返す。
func (s *Server) twitterHTTPClient() *http.Client {
	return twitterHTTPClientFactory()
}

// requireCurrentTwitterAccount はログイン中ユーザーに紐づく Twitter アカウントを取得する
// (TwitterRouter.GetCurrentTwitterAccount 相当) 。失敗時は 422 を返して false を返す。
func (s *Server) requireCurrentTwitterAccount(w http.ResponseWriter, r *http.Request, screenName string) (twitterAccountRecord, bool) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return twitterAccountRecord{}, false
	}

	records, err := s.listTwitterAccountsByUserAndScreenName(r.Context(), currentUser.ID, screenName)
	if err != nil {
		s.logger.Error("[TwitterRouter][GetCurrentTwitterAccount] Failed to query TwitterAccount.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return twitterAccountRecord{}, false
	}
	if len(records) == 0 {
		s.logger.Error("[TwitterRouter][GetCurrentTwitterAccount] TwitterAccount associated with screen_name does not exist.", "screen_name", screenName)
		writeError(w, http.StatusUnprocessableEntity, "TwitterAccount associated with screen_name does not exist")
		return twitterAccountRecord{}, false
	}

	// 古い形式のレコード (access_token が "NETSCAPE_COOKIE_FILE" でない) は利用できないため削除する
	isRemoved := false
	validAccounts := make([]twitterAccountRecord, 0, len(records))
	for _, record := range records {
		if record.AccessToken != "NETSCAPE_COOKIE_FILE" {
			s.logger.Error("[TwitterRouter][GetCurrentTwitterAccount] Old cookie format or OAuth session is no longer available.",
				"screen_name", record.ScreenName, "id", record.ID)
			if err := s.deleteTwitterAccount(r.Context(), record.ID); err != nil {
				s.logger.Error("failed to delete old format TwitterAccount.", "error", err)
				writeError(w, http.StatusInternalServerError, "Internal Server Error")
				return twitterAccountRecord{}, false
			}
			isRemoved = true
		} else {
			validAccounts = append(validAccounts, record)
		}
	}
	if isRemoved {
		writeError(w, http.StatusUnprocessableEntity, "Old cookie format or OAuth session is no longer available")
		return twitterAccountRecord{}, false
	}
	if len(validAccounts) == 0 {
		s.logger.Error("[TwitterRouter][GetCurrentTwitterAccount] No valid TwitterAccount found after cleanup.", "screen_name", screenName)
		writeError(w, http.StatusUnprocessableEntity, "TwitterAccount associated with screen_name does not exist")
		return twitterAccountRecord{}, false
	}
	return validAccounts[0], true
}

// encryptTwitterCookie は Netscape 形式 Cookie を Fernet で暗号化し、接頭辞を付けて返す
// (TwitterAccount.encryptAccessTokenSecret 相当) 。
func (s *Server) encryptTwitterCookie(plainText string) (string, error) {
	if plainText == "" {
		return "", nil
	}
	secret, err := auth.LoadOrCreateSecret(filepath.Join(s.paths.DataDir, "jwt_secret.dat"))
	if err != nil {
		return "", err
	}
	instance := twitter.NewFernetFromSecret(secret)
	encrypted, err := instance.Encrypt(plainText)
	if err != nil {
		return "", err
	}
	return twitterCookieEncryptionPrefix + encrypted, nil
}

// ----------------------------------------------------------------------------
// エンドポイント
// ----------------------------------------------------------------------------

// handleTwitterCookieAuth は POST /api/twitter/auth (Twitter 認証 API) を処理する。
func (s *Server) handleTwitterCookieAuth(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	cookiesTxt, browserInfo, details := parseTwitterCookieAuthRequest(r)
	if len(details) > 0 {
		writeValidationDetails(w, details)
		return
	}

	// cookies.txt (Netscape 形式) をパースして Cookie が取得できるかを試す
	cookies, err := twitter.ParseNetscapeCookieFile(cookiesTxt)
	if err != nil {
		errorMessage := fmt.Sprintf("Failed to parse cookies.txt: %s", err)
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] " + errorMessage)
		writeError(w, http.StatusUnprocessableEntity, errorMessage)
		return
	}
	if !twitter.HasTwitterCookie(cookies) {
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] No valid cookies found in the provided cookies.txt.")
		writeError(w, http.StatusUnprocessableEntity, "No valid cookies found in the provided cookies.txt")
		return
	}

	// Cookie 採取元ブラウザの環境情報を組み立てる (browser_info 未指定なら null)
	var cookieBrowserInfo *string
	if browserInfo != nil {
		serialized, err := buildCookieBrowserInfo(r, browserInfo)
		if err != nil {
			s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to build cookie browser info.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		cookieBrowserInfo = serialized
	}

	// 一時的な TwitterAccount を用意してログイン中の Twitter アカウント情報を取得する
	plainCookies := cookiesTxt
	temporaryAccount := &twitter.Account{
		UserID:       currentUser.ID,
		Name:         "Temporary",
		ScreenName:   "Temporary",
		IconURL:      "Temporary",
		AccessToken:  "NETSCAPE_COOKIE_FILE",
		PlainCookies: plainCookies,
	}
	temporaryID := temporaryAccount.ID // 0 (未保存)
	client := twitter.GetInstance(temporaryAccount, newTwitterBackendFactory, s.logger)

	viewerResult := client.FetchLoggedViewer(r.Context())
	if result, ok := viewerResult.(*twitter.TwitterAPIResult); ok && !result.IsSuccess {
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to get user information: " + result.Detail)
		writeError(w, http.StatusInternalServerError, result.Detail)
		return
	}
	viewer, ok := viewerResult.(*twitter.TweetUser)
	if !ok {
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to get user information: Invalid response type")
		writeError(w, http.StatusInternalServerError, "Failed to get user information")
		return
	}

	// アカウント名 / スクリーンネーム / アイコン URL を設定し、Cookie を暗号化して保持する
	temporaryAccount.Name = viewer.Name
	temporaryAccount.ScreenName = viewer.ScreenName
	temporaryAccount.IconURL = viewer.IconURL
	encryptedCookies, err := s.encryptTwitterCookie(plainCookies)
	if err != nil {
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to encrypt cookies.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	existingRecords, err := s.listTwitterAccountsByUserAndScreenName(r.Context(), currentUser.ID, viewer.ScreenName)
	if err != nil {
		s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to query existing accounts.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	var persisted twitterAccountRecord
	if len(existingRecords) > 0 {
		// 最も古いアカウント情報を更新する
		oldest := existingRecords[0]
		for _, record := range existingRecords[1:] {
			if record.ID < oldest.ID {
				oldest = record
			}
		}
		oldest.Name = viewer.Name
		oldest.IconURL = viewer.IconURL
		oldest.AccessToken = "NETSCAPE_COOKIE_FILE"
		oldest.AccessTokenSecret = encryptedCookies
		oldest.CookieBrowserInfo = cookieBrowserInfo
		if err := s.updateTwitterAccount(r.Context(), oldest); err != nil {
			s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to update existing account.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		// 他の重複アカウントを削除する
		for _, record := range existingRecords {
			if record.ID != oldest.ID {
				if err := s.deleteTwitterAccount(r.Context(), record.ID); err != nil {
					s.logger.Warn("[TwitterRouter][TwitterCookieAuthAPI] Failed to delete duplicate account.", "error", err)
				}
			}
		}
		persisted = oldest
	} else {
		newRecord := twitterAccountRecord{
			UserID:            currentUser.ID,
			Name:              viewer.Name,
			ScreenName:        viewer.ScreenName,
			IconURL:           viewer.IconURL,
			AccessToken:       "NETSCAPE_COOKIE_FILE",
			AccessTokenSecret: encryptedCookies,
			CookieBrowserInfo: cookieBrowserInfo,
		}
		newID, err := s.insertTwitterAccount(r.Context(), newRecord)
		if err != nil {
			s.logger.Error("[TwitterRouter][TwitterCookieAuthAPI] Failed to create new account.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		newRecord.ID = newID
		persisted = newRecord
	}

	// 一時アカウントで立ち上げたインスタンスを永続化後の ID に紐づけ直す
	twitter.RebindInstance(&temporaryID, s.twitterAccountToModel(persisted), newTwitterBackendFactory, s.logger)

	// 古い形式のレコードを自動削除する
	if deletedCount, err := s.deleteOldFormatTwitterAccounts(r.Context(), currentUser.ID); err == nil && deletedCount > 0 {
		s.logger.Info(fmt.Sprintf("[TwitterRouter][TwitterCookieAuthAPI] Deleted %d old format account(s).", deletedCount))
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleTwitterAccountDelete は DELETE /api/twitter/accounts/{screen_name} を処理する。
func (s *Server) handleTwitterAccountDelete(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	accountID := record.ID
	if err := s.deleteTwitterAccount(r.Context(), accountID); err != nil {
		s.logger.Error("[TwitterRouter][TwitterAccountDeleteAPI] Failed to delete TwitterAccount.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	twitter.RemoveInstance(accountID)
	w.WriteHeader(http.StatusNoContent)
}

// handleTwitterKeepAlive は POST /api/twitter/accounts/{screen_name}/keep-alive を処理する。
func (s *Server) handleTwitterKeepAlive(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	s.getTwitterGraphQLClient(record).KeepAlive()
	w.WriteHeader(http.StatusNoContent)
}

// handleTwitterTweet は POST /api/twitter/accounts/{screen_name}/tweets (ツイート送信 API) を処理する。
func (s *Server) handleTwitterTweet(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil && err != http.ErrNotMultipart {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	tweetText := r.FormValue("tweet")
	inReplyToStatusID := optionalFormValue(r, "in_reply_to_status_id")

	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["images"]
	}
	if len(files) > twitter.MaxTweetImages {
		s.logger.Error(fmt.Sprintf("[TwitterRouter][TwitterTweetAPI] Can tweet up to 4 images. [image length: %d]", len(files)))
		writeError(w, http.StatusUnprocessableEntity, "Can tweet up to 4 images")
		return
	}

	images := make([]twitter.UploadedImage, 0, len(files))
	for _, file := range files {
		opened, err := file.Open()
		if err != nil {
			s.logger.Error("[TwitterRouter][TwitterTweetAPI] Failed to open uploaded image.", "error", err)
			writeError(w, http.StatusUnprocessableEntity, "Invalid image file")
			return
		}
		data, readErr := io.ReadAll(opened)
		_ = opened.Close()
		if readErr != nil {
			s.logger.Error("[TwitterRouter][TwitterTweetAPI] Failed to read uploaded image.", "error", readErr)
			writeError(w, http.StatusUnprocessableEntity, "Invalid image file")
			return
		}
		contentType := file.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}
		images = append(images, twitter.UploadedImage{
			Filename:    file.Filename,
			ContentType: contentType,
			Data:        data,
		})
	}

	result := s.getTwitterGraphQLClient(record).CreateTweet(r.Context(), tweetText, images, inReplyToStatusID)
	writeJSON(w, http.StatusOK, result)
}

// handleTwitterRetweet は PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet を処理する。
func (s *Server) handleTwitterRetweet(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.getTwitterGraphQLClient(record).CreateRetweet(r.Context(), r.PathValue("tweet_id")))
}

// handleTwitterRetweetCancel は DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/retweet を処理する。
func (s *Server) handleTwitterRetweetCancel(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.getTwitterGraphQLClient(record).DeleteRetweet(r.Context(), r.PathValue("tweet_id")))
}

// handleTwitterFavorite は PUT /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite を処理する。
func (s *Server) handleTwitterFavorite(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.getTwitterGraphQLClient(record).FavoriteTweet(r.Context(), r.PathValue("tweet_id")))
}

// handleTwitterFavoriteCancel は DELETE /api/twitter/accounts/{screen_name}/tweets/{tweet_id}/favorite を処理する。
func (s *Server) handleTwitterFavoriteCancel(w http.ResponseWriter, r *http.Request) {
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.getTwitterGraphQLClient(record).UnfavoriteTweet(r.Context(), r.PathValue("tweet_id")))
}

// handleTwitterTimeline は GET /api/twitter/accounts/{screen_name}/timeline を処理する。
func (s *Server) handleTwitterTimeline(w http.ResponseWriter, r *http.Request) {
	// FastAPI はエンドポイント自身のクエリ検証より先に依存 (GetCurrentTwitterAccount) を解決する。
	// 依存が 401 / 422 を返した場合はその応答が確定し、クエリ検証エラーは出力されない
	// (実機 Python でも未認証時は 401 Not authenticated になり、cursor_type の検証エラーは出ない) 。
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}

	// 依存を通過した場合のみクエリパラメータを検証する
	v := newFastAPIValidation(r)
	cursorType := v.queryLiteral("cursor_type",
		[]string{"Top", "Bottom", "Gap", "ShowMore"}, "Top", "'Top', 'Bottom', 'Gap' or 'ShowMore'")
	if v.writeIfInvalid(w) {
		return
	}

	cursorID := optionalQueryValue(r, "cursor_id")
	seenTweetIDs := []string{}
	if raw := r.URL.Query().Get("seen_tweet_ids"); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			if value != "" {
				seenTweetIDs = append(seenTweetIDs, value)
			}
		}
	}

	result := s.getTwitterGraphQLClient(record).HomeLatestTimeline(r.Context(), cursorID, cursorType, seenTweetIDs)
	writeJSON(w, http.StatusOK, result)
}

// handleTwitterSearch は GET /api/twitter/accounts/{screen_name}/search を処理する。
func (s *Server) handleTwitterSearch(w http.ResponseWriter, r *http.Request) {
	// timeline と同じく、依存 (GetCurrentTwitterAccount) をクエリ検証より先に解決する
	record, ok := s.requireCurrentTwitterAccount(w, r, r.PathValue("screen_name"))
	if !ok {
		return
	}

	// Python 側の宣言順 (query → search_type → cursor_id → cursor_type) で検証する。
	// cursor_id は str | None のため検証エラーになり得ず、ここでは扱わない
	v := newFastAPIValidation(r)
	searchQuery := v.queryString("query")
	searchType := v.queryLiteral("search_type", []string{"Top", "Latest"}, "Latest", "'Top' or 'Latest'")
	cursorType := v.queryLiteral("cursor_type",
		[]string{"Top", "Bottom", "Gap", "ShowMore"}, "Top", "'Top', 'Bottom', 'Gap' or 'ShowMore'")
	if v.writeIfInvalid(w) {
		return
	}

	result := s.getTwitterGraphQLClient(record).SearchTimeline(
		r.Context(), searchType, searchQuery, optionalQueryValue(r, "cursor_id"), cursorType)
	writeJSON(w, http.StatusOK, result)
}

// handleTwitterVideoProxy は GET /api/twitter/video-proxy を処理する。
func (s *Server) handleTwitterVideoProxy(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	targets, hasURL := query["url"]
	// url は str 必須パラメータで空文字も有効値のため、未指定のときだけ missing を返す。
	// 空文字は下の scheme 検証に回し、Python と同じく 422 "URL scheme must be https." にする
	if !hasURL || len(targets) == 0 {
		writeValidationDetails(w, []validationDetail{
			missingDetail([]any{"query", "url"}, nil),
		})
		return
	}
	target := lastQueryValue(targets)

	parsedURL, err := url.Parse(target)
	if err != nil || parsedURL.Scheme != "https" {
		s.logger.Error("[TwitterRouter][TwitterVideoProxyAPI] URL scheme must be https.")
		writeError(w, http.StatusUnprocessableEntity, "URL scheme must be https.")
		return
	}
	if !containsString(allowedVideoProxyDomains, parsedURL.Hostname()) {
		s.logger.Error(fmt.Sprintf("[TwitterRouter][TwitterVideoProxyAPI] URL domain is not allowed: %s", parsedURL.Hostname()))
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("URL domain is not allowed. Only %s are allowed.", strings.Join(allowedVideoProxyDomains, ", ")))
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to request upstream: "+err.Error())
		return
	}
	request.Header.Set("User-Agent", twitterProxyUserAgent)
	for _, headerName := range videoProxyRequestHeaders {
		if value := r.Header.Get(headerName); value != "" {
			request.Header.Set(headerName, value)
		}
	}

	response, err := s.twitterHTTPClient().Do(request)
	if err != nil {
		s.logger.Error("[TwitterRouter][TwitterVideoProxyAPI] Failed to request upstream.", "error", err)
		writeError(w, http.StatusBadGateway, "Failed to request upstream: "+err.Error())
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		s.logger.Error(fmt.Sprintf("[TwitterRouter][TwitterVideoProxyAPI] Upstream returned HTTP %d: %s",
			response.StatusCode, string(errorBody)))
		writeError(w, response.StatusCode, fmt.Sprintf("Upstream returned HTTP %d.", response.StatusCode))
		return
	}

	for _, headerName := range videoProxyResponseHeaders {
		if value := response.Header.Get(headerName); value != "" {
			w.Header().Set(headerName, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	// 上流のレスポンスボディをチャンク単位で転送する
	buffer := make([]byte, 65536)
	for {
		length, readErr := response.Body.Read(buffer)
		if length > 0 {
			if _, writeErr := w.Write(buffer[:length]); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// ----------------------------------------------------------------------------
// リクエスト型 / 小ヘルパー
// ----------------------------------------------------------------------------

// twitterCookieAuthRequest は schemas.TwitterCookieAuthRequest の生ボディ。
// Pydantic のバリデーションエラーを再現するため、型検証は手動で行う。
type twitterCookieAuthRequest struct {
	CookiesTxt  json.RawMessage `json:"cookies_txt"`
	BrowserInfo json.RawMessage `json:"browser_info"`
}

// browserEnvironmentInfoRequest は schemas.BrowserEnvironmentInfoRequest と互換。
type browserEnvironmentInfoRequest struct {
	UserAgentData     map[string]any `json:"user_agent_data"`
	NavigatorPlatform string         `json:"navigator_platform"`
	Locale            string         `json:"locale"`
	Timezone          string         `json:"timezone"`
}

// parseTwitterCookieAuthRequest は TwitterCookieAuthRequest を Pydantic 相当の規則で検証する。
//
// 戻り値の details が空でない場合は 422 を返すべき状態を表す (TwitterRouter の
// schemas.TwitterCookieAuthRequest バリデーション相当) 。
func parseTwitterCookieAuthRequest(r *http.Request) (string, *browserEnvironmentInfoRequest, []validationDetail) {
	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		body = nil
	}
	if len(body) == 0 {
		return "", nil, []validationDetail{missingDetail([]any{"body", "cookies_txt"}, map[string]any{})}
	}

	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", nil, []validationDetail{missingDetail([]any{"body", "cookies_txt"}, map[string]any{})}
	}
	bodyObject, isObject := decoded.(map[string]any)
	if !isObject {
		return "", nil, []validationDetail{missingDetail([]any{"body", "cookies_txt"}, decoded)}
	}

	// cookies_txt: 欠落は missing、null / 非文字列は string_type
	rawCookies, hasCookies := bodyObject["cookies_txt"]
	if !hasCookies {
		return "", nil, []validationDetail{missingDetail([]any{"body", "cookies_txt"}, bodyObject)}
	}
	cookiesTxt, isString := rawCookies.(string)
	if !isString {
		return "", nil, []validationDetail{stringTypeDetail([]any{"body", "cookies_txt"}, rawCookies)}
	}

	// browser_info: 省略 / null は許容。存在する場合は必須フィールドを検証する
	rawBrowserInfo, hasBrowserInfo := bodyObject["browser_info"]
	if !hasBrowserInfo || rawBrowserInfo == nil {
		return cookiesTxt, nil, nil
	}
	browserObject, isBrowserObject := rawBrowserInfo.(map[string]any)
	if !isBrowserObject {
		return "", nil, []validationDetail{missingDetail([]any{"body", "browser_info"}, rawBrowserInfo)}
	}

	details := []validationDetail{}
	for _, field := range []string{"user_agent_data", "navigator_platform", "locale", "timezone"} {
		if _, exists := browserObject[field]; !exists {
			details = append(details, missingDetail([]any{"body", "browser_info", field}, browserObject))
		}
	}
	if len(details) > 0 {
		return "", nil, details
	}

	// 必須フィールドが揃っている場合は browserEnvironmentInfoRequest に変換する
	marshaled, err := json.Marshal(browserObject)
	if err != nil {
		return "", nil, []validationDetail{missingDetail([]any{"body", "browser_info"}, browserObject)}
	}
	var browserInfo browserEnvironmentInfoRequest
	if err := json.Unmarshal(marshaled, &browserInfo); err != nil {
		return "", nil, []validationDetail{missingDetail([]any{"body", "browser_info"}, browserObject)}
	}
	return cookiesTxt, &browserInfo, nil
}

// containsString は文字列スライスに値が含まれるかを返す。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// FastAPI/Pydantic 互換の 422 バリデーションエラー
// ----------------------------------------------------------------------------

// validationContext は Pydantic の ctx フィールド。
type validationContext struct {
	Expected string `json:"expected"`
}

// validationDetail は Pydantic のエラー 1 件分 (フィールド順は FastAPI の出力と同じ) 。
type validationDetail struct {
	Type  string             `json:"type"`
	Loc   []any              `json:"loc"`
	Msg   string             `json:"msg"`
	Input any                `json:"input"`
	Ctx   *validationContext `json:"ctx,omitempty"`
}

// writeValidationDetails は FastAPI 互換の 422 バリデーションエラーを書き出す。
func writeValidationDetails(w http.ResponseWriter, details []validationDetail) {
	// detail が空の場合は空配列として出力する
	if details == nil {
		details = []validationDetail{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, struct {
		Detail []validationDetail `json:"detail"`
	}{Detail: details})
}

// missingDetail は Pydantic の "missing" エラーを生成する。
func missingDetail(loc []any, input any) validationDetail {
	return validationDetail{Type: "missing", Loc: loc, Msg: "Field required", Input: input}
}

// literalDetail は Pydantic の "literal_error" エラーを生成する。
func literalDetail(loc []any, input string, expected string) validationDetail {
	return validationDetail{
		Type:  "literal_error",
		Loc:   loc,
		Msg:   "Input should be " + expected,
		Input: input,
		Ctx:   &validationContext{Expected: expected},
	}
}

// stringTypeDetail は Pydantic の "string_type" エラーを生成する。
func stringTypeDetail(loc []any, input any) validationDetail {
	return validationDetail{Type: "string_type", Loc: loc, Msg: "Input should be a valid string", Input: input}
}

// ----------------------------------------------------------------------------
// cookie_browser_info の組み立て
// ----------------------------------------------------------------------------

// userAgentDataPayload は schemas.BrowserEnvironmentUserAgentData と互換。
type userAgentDataPayload struct {
	Platform        *string `json:"platform"`
	PlatformVersion *string `json:"platform_version"`
	Architecture    *string `json:"architecture"`
	Bitness         *string `json:"bitness"`
	Mobile          *bool   `json:"mobile"`
	Model           *string `json:"model"`
	Wow64           *bool   `json:"wow64"`
}

// buildCookieBrowserInfo は TwitterAccount.cookie_browser_info に永続化する環境情報を組み立てる
// (TwitterRouter.BuildCookieBrowserInfo 相当) 。
//
// Python 版は json.dumps(value, ensure_ascii=False) で保存するため、
// キー順も含めて一致させるために twitter.OrderedMap / twitter.PythonJSON を使う。
func buildCookieBrowserInfo(r *http.Request, request *browserEnvironmentInfoRequest) (*string, error) {
	// user_agent_data を OrderedMap へ変換する (Pydantic モデルと同じキー順を維持)
	userAgentData := twitter.NewOrderedMap()
	userAgentDataJSON, err := json.Marshal(request.UserAgentData)
	if err != nil {
		return nil, err
	}
	var payload userAgentDataPayload
	if err := json.Unmarshal(userAgentDataJSON, &payload); err != nil {
		return nil, err
	}
	userAgentData.Set("platform", pointerToAny(payload.Platform))
	userAgentData.Set("platform_version", pointerToAny(payload.PlatformVersion))
	userAgentData.Set("architecture", pointerToAny(payload.Architecture))
	userAgentData.Set("bitness", pointerToAny(payload.Bitness))
	userAgentData.Set("mobile", pointerToAny(payload.Mobile))
	userAgentData.Set("model", pointerToAny(payload.Model))
	userAgentData.Set("wow64", pointerToAny(payload.Wow64))

	acceptLanguage := r.Header.Get("Accept-Language")
	acceptLanguages := twitter.ParseAcceptLanguageHeader(optionalHeaderValue(r, "Accept-Language"))

	httpHeaders := twitter.NewOrderedMap()
	httpHeaders.Set("user_agent", pointerToAny(optionalHeaderValue(r, "User-Agent")))
	httpHeaders.Set("accept_language", optionalEmptyToNil(acceptLanguage))
	languageValues := make([]any, 0, len(acceptLanguages))
	for _, language := range acceptLanguages {
		languageValues = append(languageValues, language)
	}
	httpHeaders.Set("accept_languages", languageValues)
	httpHeaders.Set("sec_ch_ua", pointerToAny(optionalHeaderValue(r, "Sec-Ch-Ua")))
	httpHeaders.Set("sec_ch_ua_mobile", pointerToAny(optionalHeaderValue(r, "Sec-Ch-Ua-Mobile")))
	httpHeaders.Set("sec_ch_ua_platform", pointerToAny(optionalHeaderValue(r, "Sec-Ch-Ua-Platform")))

	info := twitter.NewOrderedMap()
	info.Set("http_headers", httpHeaders)
	info.Set("user_agent_data", userAgentData)
	info.Set("navigator_platform", request.NavigatorPlatform)
	info.Set("locale", request.Locale)
	info.Set("timezone", request.Timezone)

	encoded, err := twitter.PythonJSON(info)
	if err != nil {
		return nil, err
	}
	serialized := string(encoded)
	return &serialized, nil
}

// optionalHeaderValue はヘッダーが存在する場合のみ値を返す。
func optionalHeaderValue(r *http.Request, key string) *string {
	value := r.Header.Get(key)
	if value == "" {
		return nil
	}
	return &value
}

// optionalEmptyToNil は空文字列を nil にする (Python の request.headers.get() 相当) 。
func optionalEmptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// pointerToAny はポインタを any へ変換する (nil は nil のまま) 。
func pointerToAny[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}
