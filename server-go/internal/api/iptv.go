package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
)

// iptvMaxPerPage はチャンネル一覧 API の1ページあたりの最大件数。
const iptvMaxPerPage = 500

// iptvDefaultPerPage はチャンネル一覧 API の1ページあたりの既定件数。
const iptvDefaultPerPage = 60

// iptvProxyAcceptHeaders はプロキシのリクエストで送信する Accept 系ヘッダー。
var iptvProxyAcceptHeaders = map[string]string{
	"Accept":          "*/*",
	"Accept-Language": "ja,en-US;q=0.9,en;q=0.8",
}

// iptvChannelResponse はチャンネル一覧 API のレスポンス。
type iptvChannelsResponse struct {
	Total     int                     `json:"total"`
	Page      int                     `json:"page"`
	PerPage   int                     `json:"per_page"`
	MaxPage   int                     `json:"max_page"`
	AllTotal  int                     `json:"all_total"`
	UpdatedAt *float64                `json:"updated_at"`
	Sources   []string                `json:"sources"`
	Errors    map[string]string       `json:"errors"`
	Channels  []*iptv.ChannelResponse `json:"channels"`
}

// iptvCountriesResponse は国一覧 API のレスポンス。
type iptvCountriesResponse struct {
	Total     int            `json:"total"`
	UpdatedAt *float64       `json:"updated_at"`
	AllTotal  int            `json:"all_total"`
	Countries []iptv.Country `json:"countries"`
}

// iptvGroupsResponse はグループ一覧 API のレスポンス。
type iptvGroupsResponse struct {
	Total  int          `json:"total"`
	Groups []iptv.Group `json:"groups"`
}

// iptvSourcesResponse はプレイリストソース一覧 API のレスポンス。
type iptvSourcesResponse struct {
	ConfigSources []string `json:"config_sources"`
	UserSources   []string `json:"user_sources"`
}

// iptvTVUIChannelResponse はテレビ視聴 UI 登録チャンネルの1件分。
type iptvTVUIChannelResponse struct {
	DisplayChannelID string  `json:"display_channel_id"`
	Name             string  `json:"name"`
	LogoURL          *string `json:"logo_url"`
	CountryName      *string `json:"country_name"`
}

// iptvTVUIChannelsResponse はテレビ視聴 UI 登録チャンネル一覧 API のレスポンス。
type iptvTVUIChannelsResponse struct {
	Total    int                       `json:"total"`
	Channels []iptvTVUIChannelResponse `json:"channels"`
}

// parseIPTVIntQuery は整数のクエリパラメーターを検証して返す。
// 検証に失敗した場合は FastAPI 互換のエラーメッセージを返す。
func parseIPTVIntQuery(query url.Values, name string, defaultValue int, minValue int, maxValue int) (int, string) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return defaultValue, ""
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, "Input should be a valid integer, unable to parse string as an integer"
	}
	if value < minValue {
		return 0, fmt.Sprintf("Input should be greater than or equal to %d", minValue)
	}
	if maxValue > 0 && value > maxValue {
		return 0, fmt.Sprintf("Input should be less than or equal to %d", maxValue)
	}
	return value, ""
}

// optionalQuery はクエリパラメーターを取得し、指定されていない場合は nil を返す。
func optionalQuery(query url.Values, name string) *string {
	if !query.Has(name) {
		return nil
	}
	value := query.Get(name)
	return &value
}

// updatedAtPointer は更新時刻を JSON 出力用のポインタに変換する (0 の場合は null) 。
func updatedAtPointer(updatedAt float64) *float64 {
	if updatedAt == 0 {
		return nil
	}
	return &updatedAt
}

// handleIPTVChannels は GET /api/iptv/channels を処理する。
func (s *Server) handleIPTVChannels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, message := parseIPTVIntQuery(query, "page", 1, 1, 0)
	if message != "" {
		writeError(w, http.StatusUnprocessableEntity, message)
		return
	}
	perPage, message := parseIPTVIntQuery(query, "per_page", iptvDefaultPerPage, 1, iptvMaxPerPage)
	if message != "" {
		writeError(w, http.StatusUnprocessableEntity, message)
		return
	}
	refresh := parseBoolQuery(query.Get("refresh"))
	withQuality := parseBoolQuery(query.Get("with_quality"))

	// IPTV 機能が無効の場合は空の一覧を返す
	if !s.config.IPTV.Enabled {
		writeJSON(w, http.StatusOK, &iptvChannelsResponse{
			Total: 0, Page: page, PerPage: perPage, MaxPage: 1, AllTotal: 0,
			UpdatedAt: nil, Sources: []string{}, Errors: map[string]string{}, Channels: []*iptv.ChannelResponse{},
		})
		return
	}

	// チャンネル一覧を取得する (キャッシュが有効な間は再取得しない)
	s.iptv.Refresh(r.Context(), refresh)

	// 絞り込みを適用する
	filtered := s.iptv.FilterChannels(optionalQuery(query, "country"), optionalQuery(query, "group"), optionalQuery(query, "search"))

	// ページネーションを適用する
	total := len(filtered)
	maxPage := max((total+perPage-1)/perPage, 1)
	if page > maxPage {
		page = maxPage
	}
	start := (page - 1) * perPage
	end := min(start+perPage, total)
	pageChannels := filtered[start:end]

	// 呼び出し元を識別するキーを取得する (テレビ視聴 UI への登録状態はユーザーごとに異なる)
	userKey := s.auth.ResolveUserKey(w, r)
	tvuiDisplayChannelIDs := map[string]bool{}
	for _, displayChannelID := range s.iptv.LoadTVUIChannelIDs(userKey) {
		tvuiDisplayChannelIDs[displayChannelID] = true
	}

	// ページ内のチャンネルを辞書に変換する
	channelResponses := make([]*iptv.ChannelResponse, 0, len(pageChannels))
	for _, channel := range pageChannels {
		response := channel.ToResponse()
		response.IsTVUIRegistered = tvuiDisplayChannelIDs[response.DisplayChannelID]
		channelResponses = append(channelResponses, response)
	}

	// 元配信の画質を検出して含める (検出はストリームへのアクセスを伴うため、要求された場合のみ実行する)
	if withQuality {
		detected := s.iptv.DetectChannelsQualities(r.Context(), pageChannels)
		for _, response := range channelResponses {
			qualities, exists := detected[response.ID]
			if !exists {
				continue
			}
			response.SourceQuality = qualities.SourceQuality
			response.SourceCodec = qualities.Codec
			if qualities.Qualities != nil {
				response.Qualities = qualities.Qualities
			}
		}
	}

	writeJSON(w, http.StatusOK, &iptvChannelsResponse{
		Total:     total,
		Page:      page,
		PerPage:   perPage,
		MaxPage:   maxPage,
		AllTotal:  len(s.iptv.Channels()),
		UpdatedAt: updatedAtPointer(s.iptv.UpdatedAt()),
		Sources:   s.iptv.AllSourceURLs(),
		Errors:    s.iptv.SourceErrors(),
		Channels:  channelResponses,
	})
}

// handleIPTVCountries は GET /api/iptv/countries を処理する。
func (s *Server) handleIPTVCountries(w http.ResponseWriter, r *http.Request) {
	// IPTV 機能が無効の場合は空の一覧を返す
	if !s.config.IPTV.Enabled {
		writeJSON(w, http.StatusOK, &iptvCountriesResponse{
			Total: 0, UpdatedAt: nil, AllTotal: 0, Countries: []iptv.Country{},
		})
		return
	}

	s.iptv.Refresh(r.Context(), parseBoolQuery(r.URL.Query().Get("refresh")))
	countries := s.iptv.Countries(nil)
	writeJSON(w, http.StatusOK, &iptvCountriesResponse{
		Total:     len(countries),
		UpdatedAt: updatedAtPointer(s.iptv.UpdatedAt()),
		AllTotal:  len(s.iptv.Channels()),
		Countries: countries,
	})
}

// handleIPTVGroups は GET /api/iptv/groups を処理する。
func (s *Server) handleIPTVGroups(w http.ResponseWriter, r *http.Request) {
	// IPTV 機能が無効の場合は空の一覧を返す
	if !s.config.IPTV.Enabled {
		writeJSON(w, http.StatusOK, &iptvGroupsResponse{Total: 0, Groups: []iptv.Group{}})
		return
	}

	query := r.URL.Query()
	s.iptv.Refresh(r.Context(), parseBoolQuery(query.Get("refresh")))
	filtered := s.iptv.FilterChannels(optionalQuery(query, "country"), nil, nil)
	groups := s.iptv.Groups(filtered)
	writeJSON(w, http.StatusOK, &iptvGroupsResponse{Total: len(groups), Groups: groups})
}

// handleIPTVPlaylist は GET /api/iptv/playlist.m3u を処理する。
func (s *Server) handleIPTVPlaylist(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	s.iptv.Refresh(r.Context(), parseBoolQuery(query.Get("refresh")))
	filtered := s.iptv.FilterChannels(optionalQuery(query, "country"), optionalQuery(query, "group"), nil)

	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(iptv.BuildM3UPlaylist(filtered)))
}

// iptvSources はソース一覧 API のレスポンスを生成する。
func (s *Server) iptvSources() *iptvSourcesResponse {
	userSources := s.iptv.LoadUserSources()
	if userSources == nil {
		userSources = []string{}
	}
	return &iptvSourcesResponse{
		ConfigSources: s.iptv.ConfigSources(),
		UserSources:   userSources,
	}
}

// handleIPTVSources は GET /api/iptv/sources を処理する。
func (s *Server) handleIPTVSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.iptvSources())
}

// handleIPTVSourceAdd は POST /api/iptv/sources を処理する。
func (s *Server) handleIPTVSourceAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL string `json:"url"`
	}
	if !decodeJSONBody(r, &payload) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}

	source := strings.TrimSpace(payload.URL)
	if source == "" {
		writeError(w, http.StatusBadRequest, "The playlist URL must not be empty.")
		return
	}

	// 追加登録分のソースに追加する (既に登録済みの場合は何もしない)
	userSources := s.iptv.LoadUserSources()
	found := false
	for _, existing := range userSources {
		if existing == source {
			found = true
			break
		}
	}
	if !found {
		userSources = append(userSources, source)
		if err := s.iptv.SaveUserSources(userSources); err != nil {
			s.logger.Error("failed to save IPTV sources", "error", err)
		}
		s.logger.Info("IPTV playlist source added", "source", source)
	}

	// 追加したソースを即座に反映させる
	s.iptv.Refresh(r.Context(), true)
	writeJSON(w, http.StatusOK, s.iptvSources())
}

// handleIPTVSourceDelete は DELETE /api/iptv/sources を処理する。
func (s *Server) handleIPTVSourceDelete(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")

	userSources := s.iptv.LoadUserSources()
	remaining := make([]string, 0, len(userSources))
	removed := false
	for _, existing := range userSources {
		if existing == target {
			removed = true
			continue
		}
		remaining = append(remaining, existing)
	}
	if removed {
		if err := s.iptv.SaveUserSources(remaining); err != nil {
			s.logger.Error("failed to save IPTV sources", "error", err)
		}
		s.logger.Info("IPTV playlist source removed", "source", target)
		// ソースの削除を即座に反映させる
		s.iptv.Refresh(r.Context(), true)
	}

	writeJSON(w, http.StatusOK, s.iptvSources())
}

// buildIPTVTVUIResponse は指定されたユーザーが登録した IPTV チャンネルの一覧を生成する。
func (s *Server) buildIPTVTVUIResponse(userKey string) *iptvTVUIChannelsResponse {
	channels := s.iptv.GetTVUIChannels(userKey)
	responses := make([]iptvTVUIChannelResponse, 0, len(channels))
	for _, channel := range channels {
		responses = append(responses, iptvTVUIChannelResponse{
			DisplayChannelID: iptv.BuildDisplayChannelID(channel.URL),
			Name:             channel.Name,
			LogoURL:          channel.LogoURL,
			CountryName:      channel.CountryName,
		})
	}
	return &iptvTVUIChannelsResponse{Total: len(responses), Channels: responses}
}

// handleIPTVTVUIChannels は GET /api/iptv/tvui を処理する。
func (s *Server) handleIPTVTVUIChannels(w http.ResponseWriter, r *http.Request) {
	userKey := s.auth.ResolveUserKey(w, r)
	writeJSON(w, http.StatusOK, s.buildIPTVTVUIResponse(userKey))
}

// handleIPTVTVUIRegister は POST /api/iptv/tvui を処理する。
func (s *Server) handleIPTVTVUIRegister(w http.ResponseWriter, r *http.Request) {
	if !s.config.IPTV.Enabled {
		writeError(w, http.StatusBadRequest, "IPTV is disabled.")
		return
	}

	var payload struct {
		DisplayChannelID string `json:"display_channel_id"`
	}
	if !decodeJSONBody(r, &payload) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}

	// 登録前にチャンネルが存在するか確認する (プレイリストが未取得の場合はここで取得される)
	s.iptv.Refresh(r.Context(), false)
	channel := s.iptv.GetChannelByDisplayChannelID(payload.DisplayChannelID)
	if channel == nil {
		writeError(w, http.StatusNotFound, "Specified IPTV channel was not found.")
		return
	}

	userKey := s.auth.ResolveUserKey(w, r)
	s.iptv.RegisterTVUIChannel(userKey, payload.DisplayChannelID)
	s.logger.Info(
		"IPTV channel registered to TV UI",
		"channel", channel.Name, "display_channel_id", payload.DisplayChannelID, "user_key", userKey,
	)
	writeJSON(w, http.StatusOK, s.buildIPTVTVUIResponse(userKey))
}

// handleIPTVTVUIUnregister は DELETE /api/iptv/tvui を処理する。
func (s *Server) handleIPTVTVUIUnregister(w http.ResponseWriter, r *http.Request) {
	displayChannelID := r.URL.Query().Get("display_channel_id")
	userKey := s.auth.ResolveUserKey(w, r)
	s.iptv.UnregisterTVUIChannel(userKey, displayChannelID)
	writeJSON(w, http.StatusOK, s.buildIPTVTVUIResponse(userKey))
}

// validateProxyURL はプロキシ対象の URL を検証する。
func validateProxyURL(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("Proxy URL must be started with http:// or https://.")
	}
	return nil
}

// iptvProxyRequestHeaders はプロキシのリクエストに送信するヘッダーを組み立てる。
func (s *Server) iptvProxyRequestHeaders(target string, rangeHeader string) map[string]string {
	headers := map[string]string{"User-Agent": s.config.IPTV.UserAgent}
	for key, value := range iptvProxyAcceptHeaders {
		headers[key] = value
	}
	// プレイリストで指定された User-Agent / Referer を反映する
	for key, value := range s.iptv.StreamHeaders(target) {
		headers[key] = value
	}
	headers["Range"] = rangeHeader
	return headers
}

// fetchIPTVStreamError はストリームの取得に失敗した場合の 502 エラーメッセージを組み立てる。
//
// Python 版の HTTPException と同じ書式 (httpx の例外クラス名) に寄せている。
func fetchIPTVStreamError(err error) string {
	name := "NetworkError"
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		name = "ReadTimeout"
	} else if errors.Is(err, context.DeadlineExceeded) {
		name = "ReadTimeout"
	}
	return "Failed to fetch IPTV stream: " + name
}

// handleIPTVProxy は GET /api/iptv/proxy を処理する。
func (s *Server) handleIPTVProxy(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if err := validateProxyURL(target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// ストリームを取得するためのリクエスト
	// ライブストリームは長時間流れ続けるため、読み取りには (HLS プレイリスト以外) 総タイムアウトを設定しない
	ctx := r.Context()
	if iptv.IsHLSPlaylist(target) {
		// HLS プレイリストは全体を読み込む必要があるため、読み取りに 60 秒のタイムアウトを設定する
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to fetch IPTV stream: NetworkError")
		return
	}
	rangeHeader := r.URL.Query().Get("range")
	for key, value := range s.iptvProxyRequestHeaders(target, rangeHeader) {
		if key == "Range" && value == "" {
			continue
		}
		request.Header.Set(key, value)
	}
	response, err := s.iptv.StreamClient().Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, fetchIPTVStreamError(err))
		return
	}
	defer func() { _ = response.Body.Close() }()

	// 上流が 4xx / 5xx を返した場合はそのままエラーとして扱う
	if response.StatusCode >= 400 {
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("The IPTV stream server returned an error. (HTTP Error %d)", response.StatusCode))
		return
	}

	// HLS プレイリストの場合は URI を書き換えて返す
	if iptv.IsHLSPlaylist(target) {
		content, err := io.ReadAll(response.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, fetchIPTVStreamError(err))
			return
		}
		// レスポンスの文字コードは UTF-8 として扱う (HLS の仕様上 UTF-8 が必須)
		rewritten := iptv.RewriteHLSPlaylist(strings.ToValidUTF8(string(content), "\ufffd"), response.Request.URL.String())
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		// ライブプレイリストは常に最新を取得する必要があるためキャッシュさせない
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write([]byte(rewritten))
		return
	}

	// 上流のレスポンスヘッダーのうち、転送する必要があるものを引き継ぐ
	w.Header().Set("Cache-Control", "no-store")
	for _, headerName := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if value := response.Header.Get(headerName); value != "" {
			w.Header().Set(headerName, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(response.StatusCode)

	// 上流のレスポンスボディをチャンク単位で転送する
	buffer := make([]byte, 32*1024)
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

// iptvHTTPClient は IPTV のロゴの取得に利用する HTTP クライアントを返す。
func (s *Server) iptvHTTPClient() *http.Client {
	return s.iptv.HTTPClient()
}

// handleIPTVLogo は GET /api/iptv/logo を処理する。
func (s *Server) handleIPTVLogo(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if err := validateProxyURL(target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// ロゴ画像を取得する
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		s.serveIPTVDefaultLogo(w, r)
		return
	}
	request.Header.Set("User-Agent", s.config.IPTV.UserAgent)
	for key, value := range iptvProxyAcceptHeaders {
		request.Header.Set(key, value)
	}
	response, err := s.iptvHTTPClient().Do(request)
	if err != nil {
		// 取得できなかった場合は既定のロゴを返す
		s.serveIPTVDefaultLogo(w, r)
		return
	}
	defer func() { _ = response.Body.Close() }()

	// 上流がエラーを返した場合も既定のロゴを返す (ロゴが無いだけなので、クライアント側でエラーを出さない)
	if response.StatusCode != 200 {
		s.serveIPTVDefaultLogo(w, r)
		return
	}
	logoData, err := io.ReadAll(response.Body)
	if err != nil {
		s.serveIPTVDefaultLogo(w, r)
		return
	}
	mediaType := response.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	if index := strings.Index(mediaType, ";"); index >= 0 {
		mediaType = strings.TrimSpace(mediaType[:index])
	}

	w.Header().Set("Content-Type", mediaType)
	// ロゴは頻繁に変わらないため、1日キャッシュさせる
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logoData)
}

// serveIPTVDefaultLogo は既定のロゴ画像 (server/static/logos/default.png) を書き出す。
func (s *Server) serveIPTVDefaultLogo(w http.ResponseWriter, r *http.Request) {
	logoPath := filepath.Join(s.paths.StaticDir, "logos", "default.png")
	logoData, err := os.ReadFile(logoPath)
	if err != nil {
		s.logger.Error("failed to read default logo", "error", err, "path", logoPath)
		writeError(w, http.StatusNotImplemented, "Not Implemented")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logoData)
}
