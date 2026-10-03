package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
)

// niconicoOAuthClientID はニコニコ OAuth のクライアント ID (server/app/constants.py と同じ) 。
const niconicoOAuthClientID = "4JTJdyBZLwMJwaI7"

// niconicoRequestTimeout はニコニコ関連 API のタイムアウト (HTTPX_CLIENT と同じ 3 秒) 。
const niconicoRequestTimeout = 3 * time.Second

// niconicoLiveProgramIDPattern は現在放送中のニコニコ生放送番組のリンクから番組 ID (lv...) を抽出する。
var niconicoLiveProgramIDPattern = regexp.MustCompile(`https://live\.nicovideo\.jp/watch/(lv[0-9]+)`)

// テストから差し替えられるように URL を変数として定義する。
var (
	niconicoChannelPageURLPattern = "https://ch.nicovideo.jp/%s/live"
	nicoliveWSEndpointURL         = "https://api.live2.nicovideo.jp/api/v1/wsendpoint"
	niconicoTokenURL              = "https://oauth.nicovideo.jp/oauth2/token"
	niconicoUserAPIURLPattern     = "https://nvapi.nicovideo.jp/v1/users/%d"
)

// jikkyoWebSocketInfoResponse は schemas.JikkyoWebSocketInfo 互換のレスポンス。
type jikkyoWebSocketInfoResponse struct {
	// 視聴セッション維持用 WebSocket API の URL (NX-Jikkyo)
	WatchSessionURL *string `json:"watch_session_url"`
	// 視聴セッション維持用 WebSocket API の URL (ニコニコ生放送)
	NicoLiveWatchSessionURL *string `json:"nicolive_watch_session_url"`
	// 視聴セッション維持用 WebSocket API のエラー情報 (ニコニコ生放送)
	NicoLiveWatchSessionError *string `json:"nicolive_watch_session_error"`
	// コメント受信用 WebSocket API の URL (NX-Jikkyo)
	CommentSessionURL *string `json:"comment_session_url"`
	// 現在は NX-Jikkyo のみ存在するニコニコ実況チャンネルかどうか
	IsNXJikkyoExclusive bool `json:"is_nxjikkyo_exclusive"`
}

// handleChannelJikkyo は GET /api/channels/{channel_id}/jikkyo (ニコニコ実況 WebSocket URL API) を処理する。
func (s *Server) handleChannelJikkyo(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channel_id")

	// IPTV の疑似チャンネルにはニコニコ実況のチャンネルが存在しないため、空の情報を返す
	if strings.HasPrefix(channelID, iptvChannelIDPrefix) {
		writeJSON(w, http.StatusOK, jikkyoWebSocketInfoResponse{IsNXJikkyoExclusive: false})
		return
	}

	channel, err := database.GetChannelByIDOrDisplayChannelID(r.Context(), s.db, channelID)
	if errors.Is(err, database.ErrChannelNotFound) {
		s.logger.Error("[ChannelsRouter][GetChannel] Specified display_channel_id was not found.", "channel_id", channelID)
		writeError(w, http.StatusUnprocessableEntity, "Specified display_channel_id was not found")
		return
	}
	if err != nil {
		s.logger.Error("failed to get channel", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// Authorization ヘッダーがある場合のみ、ログイン中のユーザーアカウントを取得する
	// (JWT が不正でもエラーにはしない)
	response := s.fetchJikkyoWebSocketInfo(r.Context(), channel, s.optionalCurrentUser(r))
	writeJSON(w, http.StatusOK, response)
}

// fetchJikkyoWebSocketInfo はニコニコ実況・NX-Jikkyo とコメントを送受信するための WebSocket API の情報を取得する。
func (s *Server) fetchJikkyoWebSocketInfo(ctx context.Context, channel *database.Channel, currentUser *database.User) jikkyoWebSocketInfoResponse {
	// ネットワーク ID + サービス ID に対応する実況チャンネルを解決する
	jikkyoID, nicoChannelID := "", ""
	if channelMap := s.jikkyoChannelMap(); channelMap != nil {
		jikkyoID, nicoChannelID, _ = channelMap.Resolve(channel.NetworkID, channel.ServiceID)
	}

	// 現在は NX-Jikkyo のみ存在するニコニコ実況チャンネルかどうかを表すフラグ
	// 実況チャンネル ID に対応するニコニコチャンネル ID が存在しない場合、NX-Jikkyo 固有のニコニコ実況チャンネルと判定する
	isNXJikkyoExclusive := nicoChannelID == ""

	// ネットワーク ID + サービス ID に対応するニコニコ実況チャンネルがない場合
	if jikkyoID == "" {
		return jikkyoWebSocketInfoResponse{IsNXJikkyoExclusive: isNXJikkyoExclusive}
	}

	watchSessionURL := jikkyo.WatchSessionURL(jikkyoID)
	commentSessionURL := jikkyo.CommentSessionURL(jikkyoID)

	// 現在は NX-Jikkyo のみ存在する実況チャンネル or 未ログイン or ニコニコアカウントと連携していない場合は、
	// NX-Jikkyo の WebSocket API の URL のみを返す
	if isNXJikkyoExclusive || !hasNiconicoCredentials(currentUser) {
		return jikkyoWebSocketInfoResponse{
			WatchSessionURL:     &watchSessionURL,
			CommentSessionURL:   &commentSessionURL,
			IsNXJikkyoExclusive: isNXJikkyoExclusive,
		}
	}

	// ログイン中かつニコニコアカウントと連携している場合のみ、ニコ生側の「視聴セッション維持用 WebSocket API」の URL を取得する
	nicoliveWatchSessionURL, nicoliveWatchSessionError := s.fetchNicoLiveWatchSessionURL(ctx, nicoChannelID, currentUser)
	return jikkyoWebSocketInfoResponse{
		WatchSessionURL:           &watchSessionURL,
		NicoLiveWatchSessionURL:   nicoliveWatchSessionURL,
		NicoLiveWatchSessionError: nicoliveWatchSessionError,
		CommentSessionURL:         &commentSessionURL,
		IsNXJikkyoExclusive:       isNXJikkyoExclusive,
	}
}

// fetchNicoLiveWatchSessionURL はニコニコ生放送の「視聴セッション維持用 WebSocket API」の URL を取得する。
// 取得できなかった場合は URL が nil になり、エラーメッセージが設定される。
func (s *Server) fetchNicoLiveWatchSessionURL(ctx context.Context, nicoChannelID string, currentUser *database.User) (*string, *string) {
	networkErrorMessage := "ニコニコ生放送に接続できませんでした。ニコニコで障害が発生している可能性があります。"

	// 実況チャンネル ID に対応するニコニコチャンネルで現在放送中のニコニコ生放送番組の ID を取得する
	programID, err := s.fetchNicoLiveProgramID(ctx, nicoChannelID)
	if err != nil {
		s.logger.Warn("[fetchWebSocketInfo] Failed to connect to nicolive.", "nico_channel_id", nicoChannelID, "error", err)
		return nil, &networkErrorMessage
	}
	if programID == "" {
		s.logger.Warn("[fetchWebSocketInfo] Failed to get currently broadcasting nicolive program id.", "nico_channel_id", nicoChannelID)
		message := "現在放送中のニコニコ実況番組が見つかりませんでした。"
		return nil, &message
	}

	// 視聴セッションの WebSocket URL を取得する
	accessToken := ""
	if currentUser.NiconicoAccessToken != nil {
		accessToken = *currentUser.NiconicoAccessToken
	}
	statusCode, responseBody, err := s.requestNicoLiveWSEndpoint(ctx, programID, *currentUser.NiconicoUserID, accessToken)
	if err != nil {
		s.logger.Warn("[fetchWebSocketInfo] Failed to connect to nicolive.", "error", err)
		return nil, &networkErrorMessage
	}

	// アクセストークンの有効期限が切れているため、リフレッシュトークンでアクセストークンを更新してからやり直す
	if statusCode == http.StatusUnauthorized {
		newAccessToken, refreshErrorMessage := s.refreshNiconicoAccessToken(ctx, currentUser)
		if refreshErrorMessage != "" {
			return nil, &refreshErrorMessage
		}
		statusCode, responseBody, err = s.requestNicoLiveWSEndpoint(ctx, programID, *currentUser.NiconicoUserID, newAccessToken)
		if err != nil {
			s.logger.Warn("[fetchWebSocketInfo] Failed to connect to nicolive.", "error", err)
			return nil, &networkErrorMessage
		}
	}

	// ステータスコードが 200 以外
	if statusCode != http.StatusOK {
		errorCode := ""
		var parsed struct {
			Meta struct {
				ErrorCode string `json:"errorCode"`
			} `json:"meta"`
		}
		if json.Unmarshal(responseBody, &parsed) == nil && parsed.Meta.ErrorCode != "" {
			errorCode = " (" + parsed.Meta.ErrorCode + ")"
		}
		s.logger.Warn("[fetchWebSocketInfo] Failed to get nicolive watch session url.", "status_code", statusCode, "error_code", errorCode)
		message := fmt.Sprintf("現在、ニコニコ生放送でエラーが発生しています。(HTTP Error %d%s)", statusCode, errorCode)
		return nil, &message
	}

	var parsed struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &parsed); err != nil || parsed.Data.URL == "" {
		s.logger.Warn("[fetchWebSocketInfo] Failed to parse nicolive watch session url.", "error", err)
		return nil, &networkErrorMessage
	}
	return &parsed.Data.URL, nil
}

// fetchNicoLiveProgramID は現在放送中のニコニコ生放送番組の ID (ex: lv123456) を取得する。
// 番組が見つからなかった場合は空文字列を返す。
func (s *Server) fetchNicoLiveProgramID(ctx context.Context, nicoChannelID string) (string, error) {
	pageURL := fmt.Sprintf(niconicoChannelPageURLPattern, nicoChannelID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	response, err := newNiconicoHTTPClient().Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return parseNicoLiveProgramID(body), nil
}

// parseNicoLiveProgramID はニコニコチャンネルのページ HTML から現在放送中の番組 ID を抽出する。
// BeautifulSoup で div#live_now 内の live.nicovideo.jp/watch/lv... へのリンクを探す処理の移植。
func parseNicoLiveProgramID(body []byte) string {
	index := bytes.Index(body, []byte("live_now"))
	if index < 0 {
		return ""
	}
	match := niconicoLiveProgramIDPattern.FindSubmatch(body[index:])
	if match == nil {
		return ""
	}
	return string(match[1])
}

// requestNicoLiveWSEndpoint はニコニコ生放送の wsendpoint API にリクエストする。
func (s *Server) requestNicoLiveWSEndpoint(ctx context.Context, programID string, userID int64, accessToken string) (int, []byte, error) {
	endpoint := fmt.Sprintf("%s?nicoliveProgramId=%s&userId=%d", nicoliveWSEndpointURL, programID, userID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := newNiconicoHTTPClient().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, body, nil
}

// refreshNiconicoAccessToken はリフレッシュトークンでニコニコのアクセストークンを更新し、DB に保存する。
// 更新後のアクセストークンと、失敗した場合のエラーメッセージを返す。
// Python 版 User.refreshNiconicoAccessToken() 相当。
func (s *Server) refreshNiconicoAccessToken(ctx context.Context, currentUser *database.User) (string, string) {
	refreshToken := ""
	if currentUser.NiconicoRefreshToken != nil {
		refreshToken = *currentUser.NiconicoRefreshToken
	}

	// クライアントシークレットは難読化された状態で同梱されている
	clientSecret, err := interlacedClientSecret(s.paths.StaticDir)
	if err != nil {
		s.logger.Error("failed to load niconico client secret", "error", err)
		return "", "アクセストークンの更新に失敗しました。"
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", niconicoOAuthClientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, niconicoTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "アクセストークンの更新に失敗しました。"
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := newNiconicoHTTPClient().Do(request)
	if err != nil {
		return "", "アクセストークンの更新リクエストがタイムアウトしました。"
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", "アクセストークンの更新リクエストがタイムアウトしました。"
	}

	// ステータスコードが 200 以外
	if response.StatusCode != http.StatusOK {
		errorCode := ""
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
			errorCode = " (" + parsed.Error + ")"
		}
		return "", fmt.Sprintf("アクセストークンの更新に失敗しました。(HTTP Error %d%s)", response.StatusCode, errorCode)
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return "", "アクセストークンの更新に失敗しました。"
	}

	// ついでにユーザー情報を取得し直す (取れなくてもセッション取得に支障はない)
	userName := currentUser.NiconicoUserName
	userPremium := currentUser.NiconicoUserPremium
	if nickname, isPremium, ok := s.fetchNiconicoUserInfo(ctx, currentUser.NiconicoUserID); ok {
		userName = &nickname
		userPremium = &isPremium
	}

	// 変更をデータベースに保存する
	if err := database.UpdateNiconicoAccount(
		ctx, s.writeDB, currentUser.ID, parsed.AccessToken, parsed.RefreshToken, userName, userPremium,
	); err != nil {
		s.logger.Error("failed to save refreshed niconico account", "error", err)
	}
	return parsed.AccessToken, ""
}

// fetchNiconicoUserInfo はニコニコのユーザー API からユーザー名とプレミアム会員かどうかを取得する。
func (s *Server) fetchNiconicoUserInfo(ctx context.Context, userID *int64) (string, bool, bool) {
	if userID == nil {
		return "", false, false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(niconicoUserAPIURLPattern, *userID), nil)
	if err != nil {
		return "", false, false
	}
	// X-Frontend-Id がないと INVALID_PARAMETER になる
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	request.Header.Set("X-Frontend-Id", "6")
	response, err := newNiconicoHTTPClient().Do(request)
	if err != nil {
		return "", false, false
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", false, false
	}
	var parsed struct {
		Data struct {
			User struct {
				Nickname  string `json:"nickname"`
				IsPremium bool   `json:"isPremium"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		return "", false, false
	}
	return parsed.Data.User.Nickname, parsed.Data.User.IsPremium, true
}

// hasNiconicoCredentials はユーザーがニコニコアカウントと連携しているかどうかを返す。
func hasNiconicoCredentials(user *database.User) bool {
	if user == nil || user.NiconicoUserID == nil {
		return false
	}
	if user.NiconicoUserName == nil || *user.NiconicoUserName == "" {
		return false
	}
	if user.NiconicoAccessToken == nil || *user.NiconicoAccessToken == "" {
		return false
	}
	if user.NiconicoRefreshToken == nil || *user.NiconicoRefreshToken == "" {
		return false
	}
	return true
}

// newNiconicoHTTPClient はニコニコ関連 API 用の HTTP クライアントを生成する。
// HTTPX_CLIENT と同じく 3 秒でタイムアウトする。
func newNiconicoHTTPClient() *http.Client {
	return &http.Client{Timeout: niconicoRequestTimeout}
}

// jikkyoChannelMap は実況チャンネルの対応表を返す (初回のみ読み込む) 。
func (s *Server) jikkyoChannelMap() *jikkyo.ChannelMap {
	s.jikkyoChannelsOnce.Do(func() {
		channelMap, err := jikkyo.LoadChannelMap(s.paths.StaticDir)
		if err != nil {
			s.logger.Error("failed to load jikkyo_channels.json", "error", err)
			return
		}
		s.jikkyoChannels = channelMap
	})
	return s.jikkyoChannels
}

// interlacedClientSecret は難読化されたニコニコ OAuth のクライアントシークレットを復号する。
// server/app/utils/__init__.py の Interlaced() の移植 (static/interlaced.dat を参照する) 。
func interlacedClientSecret(staticDir string) (string, error) {
	value, err := os.ReadFile(filepath.Join(staticDir, "interlaced.dat"))
	if err != nil {
		return "", err
	}
	secret := new(big.Int)
	if _, ok := secret.SetString(strings.TrimSpace(string(value)), 16); !ok {
		return "", fmt.Errorf("failed to parse interlaced.dat")
	}
	// Python 版: format(int(data, 0x10) << 8 >> 43, 'x')
	shifted := new(big.Int).Lsh(secret, 8)
	shifted.Rsh(shifted, 43)
	parts := strings.Split(shifted.Text(16), "abf01d")
	if len(parts) < 3 {
		return "", fmt.Errorf("failed to decode interlaced.dat")
	}
	// 16 進数文字列を反転してからデコードする
	reversed := reverseString(parts[2])
	decoded, err := hex.DecodeString(reversed)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// reverseString は文字列を反転する。
func reverseString(value string) string {
	runes := []rune(value)
	for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
		runes[left], runes[right] = runes[right], runes[left]
	}
	return string(runes)
}

// optionalCurrentUser は Authorization ヘッダーがある場合のみログイン中のユーザーを取得する。
// トークンが不正な場合や未ログインの場合は nil を返す (エラーにはしない) 。
func (s *Server) optionalCurrentUser(r *http.Request) *database.User {
	if r.Header.Get("Authorization") == "" {
		return nil
	}
	user, err := s.auth.AuthenticateRequest(r)
	if err != nil {
		return nil
	}
	return user
}
