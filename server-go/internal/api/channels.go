package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo"
)

// logoCacheControl はチャンネルロゴの Cache-Control ヘッダー (1ヶ月キャッシュ) 。
const logoCacheControl = "public, no-transform, immutable, max-age=2592000"

// iptvChannelIDPrefix は IPTV の疑似チャンネル ID の接頭辞 (server/app/utils/IPTVUtil.py と同じ) 。
const iptvChannelIDPrefix = "IPTV-"

// programResponse は schemas.Program 互換のレスポンス。
// フィールドの順序は Pydantic のスキーマ定義と揃えること。
type programResponse struct {
	ID                         string          `json:"id"`
	ChannelID                  string          `json:"channel_id"`
	NetworkID                  int             `json:"network_id"`
	ServiceID                  int             `json:"service_id"`
	EventID                    int             `json:"event_id"`
	Title                      string          `json:"title"`
	Description                string          `json:"description"`
	Detail                     json.RawMessage `json:"detail"`
	StartTime                  string          `json:"start_time"`
	EndTime                    string          `json:"end_time"`
	Duration                   pydanticFloat64 `json:"duration"`
	IsFree                     bool            `json:"is_free"`
	Genres                     json.RawMessage `json:"genres"`
	VideoType                  *string         `json:"video_type"`
	VideoCodec                 *string         `json:"video_codec"`
	VideoResolution            *string         `json:"video_resolution"`
	PrimaryAudioType           string          `json:"primary_audio_type"`
	PrimaryAudioLanguage       string          `json:"primary_audio_language"`
	PrimaryAudioSamplingRate   string          `json:"primary_audio_sampling_rate"`
	SecondaryAudioType         *string         `json:"secondary_audio_type"`
	SecondaryAudioLanguage     *string         `json:"secondary_audio_language"`
	SecondaryAudioSamplingRate *string         `json:"secondary_audio_sampling_rate"`
}

// liveChannelResponse は schemas.LiveChannel 互換のレスポンス。
// フィールドの順序は Pydantic のスキーマ定義と揃えること。
type liveChannelResponse struct {
	ID                 string           `json:"id"`
	DisplayChannelID   string           `json:"display_channel_id"`
	NetworkID          int              `json:"network_id"`
	ServiceID          int              `json:"service_id"`
	TransportStreamID  *int             `json:"transport_stream_id"`
	RemoconID          int              `json:"remocon_id"`
	ChannelNumber      string           `json:"channel_number"`
	Type               string           `json:"type"`
	Name               string           `json:"name"`
	TerrestrialRegions []string         `json:"terrestrial_regions"`
	JikkyoForce        *int             `json:"jikkyo_force"`
	IsSubchannel       bool             `json:"is_subchannel"`
	IsRadiochannel     bool             `json:"is_radiochannel"`
	IsWatchable        bool             `json:"is_watchable"`
	IsDisplay          bool             `json:"is_display"`
	ViewerCount        int              `json:"viewer_count"`
	ProgramPresent     *programResponse `json:"program_present"`
	ProgramFollowing   *programResponse `json:"program_following"`
}

// handleChannel は GET /api/channels/{channel_id} (チャンネル情報 API) を処理する。
func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channel_id")

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

	// 現在と次の番組情報を取得する
	present, following, err := database.GetCurrentAndNextProgram(r.Context(), s.db, channel.ID)
	if err != nil {
		s.logger.Error("failed to get current and next program", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	response, err := buildLiveChannelResponse(channel, present, following)
	if err != nil {
		s.logger.Error("failed to build channel response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// buildLiveChannelResponse は Channel と現在・次の番組情報から schemas.LiveChannel 互換のレスポンスを構築する。
func buildLiveChannelResponse(channel *database.Channel, present *database.Program, following *database.Program) (*liveChannelResponse, error) {
	// 地デジチャンネルの地域名のリストを設定 (デバッグ用)
	var terrestrialRegions []string
	if channel.Type == "GR" {
		terrestrialRegions = tsinfo.GetRegionNamesFromNetworkID(channel.NetworkID)
	}

	presentResponse, err := buildProgramResponse(present)
	if err != nil {
		return nil, err
	}
	followingResponse, err := buildProgramResponse(following)
	if err != nil {
		return nil, err
	}

	// is_display は Channel.is_display プロパティと同じ判定
	isDisplay := true
	if channel.IsWatchable == false {
		isDisplay = false
	} else if channel.IsSubchannel == true && present == nil {
		isDisplay = false
	}

	return &liveChannelResponse{
		ID:                 channel.ID,
		DisplayChannelID:   channel.DisplayChannelID,
		NetworkID:          channel.NetworkID,
		ServiceID:          channel.ServiceID,
		TransportStreamID:  channel.TransportStreamID,
		RemoconID:          channel.RemoconID,
		ChannelNumber:      channel.ChannelNumber,
		Type:               channel.Type,
		Name:               channel.Name,
		TerrestrialRegions: terrestrialRegions,
		JikkyoForce:        channel.JikkyoForce,
		IsSubchannel:       channel.IsSubchannel,
		IsRadiochannel:     channel.IsRadiochannel,
		IsWatchable:        channel.IsWatchable,
		IsDisplay:          isDisplay,
		// Python 版は LiveStream (Python プロセス上のメモリ) から視聴者数を取得するため、
		// Go 版では常に 0 になる。ストリーミングを Go へ移行する際に実装する。
		ViewerCount:      0,
		ProgramPresent:   presentResponse,
		ProgramFollowing: followingResponse,
	}, nil
}

// buildProgramResponse は Program から schemas.Program 互換のレスポンスを構築する。
func buildProgramResponse(program *database.Program) (*programResponse, error) {
	if program == nil {
		return nil, nil
	}
	// detail / genres は SQLite に JSON 文字列として保存されている
	if !json.Valid([]byte(program.Detail)) {
		return nil, fmt.Errorf("invalid JSON in programs.detail (%q)", program.ID)
	}
	if !json.Valid([]byte(program.Genres)) {
		return nil, fmt.Errorf("invalid JSON in programs.genres (%q)", program.ID)
	}
	return &programResponse{
		ID:                         program.ID,
		ChannelID:                  program.ChannelID,
		NetworkID:                  program.NetworkID,
		ServiceID:                  program.ServiceID,
		EventID:                    program.EventID,
		Title:                      program.Title,
		Description:                program.Description,
		Detail:                     json.RawMessage(program.Detail),
		StartTime:                  database.FormatJSONTime(program.StartTime),
		EndTime:                    database.FormatJSONTime(program.EndTime),
		Duration:                   pydanticFloat64(program.Duration),
		IsFree:                     program.IsFree,
		Genres:                     json.RawMessage(program.Genres),
		VideoType:                  program.VideoType,
		VideoCodec:                 program.VideoCodec,
		VideoResolution:            program.VideoResolution,
		PrimaryAudioType:           program.PrimaryAudioType,
		PrimaryAudioLanguage:       program.PrimaryAudioLanguage,
		PrimaryAudioSamplingRate:   program.PrimaryAudioSamplingRate,
		SecondaryAudioType:         program.SecondaryAudioType,
		SecondaryAudioLanguage:     program.SecondaryAudioLanguage,
		SecondaryAudioSamplingRate: program.SecondaryAudioSamplingRate,
	}, nil
}

// handleChannelLogo は GET /api/channels/{channel_id}/logo (チャンネルロゴ API) を処理する。
func (s *Server) handleChannelLogo(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channel_id")

	// "NID0-SID0" "gr000" はフロントエンド側のチャンネル情報のデフォルト値になっているため、
	// 特別にデフォルトのロゴ画像を返す
	if channelID == "NID0-SID0" || channelID == "gr000" {
		s.serveChannelLogoFile(w, r, "default.png", sha256HexString("default"))
		return
	}

	// IPTV の疑似チャンネルの場合は、IPTV のロゴをプロキシして返す (取得できない場合は既定のロゴ)
	if strings.HasPrefix(channelID, iptvChannelIDPrefix) {
		if s.serveIPTVChannelLogo(w, r, strings.TrimPrefix(channelID, iptvChannelIDPrefix)) {
			return
		}
		s.serveChannelLogoFile(w, r, "default.png", sha256HexString("iptv-default"+constants.Version))
		return
	}

	// チャンネル情報を取得する
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

	// 同梱されているロゴがあればそれを返す
	logoFileName, err := s.lookupBundledLogoFileName(r, channel)
	if err != nil {
		s.logger.Error("failed to lookup bundled logo", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if logoFileName != "" {
		// ETag はロゴファイルのパスとバージョン情報のハッシュから生成する
		logoPath := filepath.Join(s.paths.StaticDir, "logos", logoFileName)
		s.serveChannelLogoFileWithETag(w, r, logoPath, sha256HexString(logoPath+constants.Version))
		return
	}

	// Mirakurun バックエンドの場合は Mirakurun / mirakc の API からロゴを取得する
	if s.config.General.Backend == "Mirakurun" {
		logoData, mediaType, ok := s.fetchMirakurunChannelLogo(channel)
		if ok {
			s.writeChannelLogoData(w, r, logoData, mediaType)
			return
		}
	}

	// 同梱のロゴファイルも Mirakurun からのロゴもない場合は、デフォルトのロゴ画像を返す
	// (EDCB バックエンドのロゴ取得 (CtrlCmdUtil) は未移植のため、常にデフォルトのロゴになる)
	s.serveChannelLogoFile(w, r, "default.png", sha256HexString("default"))
}

// lookupBundledLogoFileName は同梱されているロゴの中からチャンネルに対応するロゴファイル名を探す。
// 対応するロゴがない場合は空文字列を返す。
func (s *Server) lookupBundledLogoFileName(r *http.Request, channel *database.Channel) (string, error) {
	logoDir := filepath.Join(s.paths.StaticDir, "logos")

	// 放送波から取得できるロゴは画質が悪いし取得できていないケースもあるため、同梱されているロゴがあればそれを利用する
	if fileExists(filepath.Join(logoDir, channel.ID+".png")) {
		return channel.ID + ".png", nil
	}

	// ***** ロゴが全国共通なので、チャンネル名の前方一致で決め打ち *****

	// NHK総合
	if channel.Type == "GR" && strings.HasPrefix(channel.Name, "NHK総合") {
		return "NID32736-SID1024.png", nil
	}
	// NHKEテレ
	if channel.Type == "GR" && strings.HasPrefix(channel.Name, "NHKEテレ") {
		return "NID32737-SID1032.png", nil
	}

	// コミュニティチャンネル (自主放送) は NID-SID が地域ごとに異なり、CATV 間で稀に重複するためチャンネル名から決め打ちで判定する
	communityChannels := []struct {
		Prefix   string
		FileName string
	}{
		{"J:COMテレビ", "J：COMテレビ.png"},
		{"J:COMチャンネル", "J：COMチャンネル.png"},
		{"イッツコムch10", "イッツコムch10.png"},
		{"イッツコムch11", "イッツコムch11.png"},
		{"スカパー！ナビ1", "スカパー！ナビ1.png"},
		{"スカパー！ナビ2", "スカパー！ナビ2.png"},
		{"eo光チャンネル", "eo光チャンネル.png"},
		{"ZTV", "ZTV.png"},
		{"BaycomCH", "BaycomCH.png"},
		{"ベイコム12CH", "ベイコム12CH.png"},
	}
	if channel.Type == "GR" {
		for _, communityChannel := range communityChannels {
			if strings.HasPrefix(channel.Name, communityChannel.Prefix) {
				return filepath.Join("community-channels", communityChannel.FileName), nil
			}
		}
	}

	// スターデジオ (本来は局ロゴは存在しないが、見栄えが悪いので 100 チャンネルすべてで同じ局ロゴを表示する)
	if channel.Type == "SKY" && 400 <= channel.ServiceID && channel.ServiceID <= 499 {
		return "NID1-SID400.png", nil
	}

	// ***** サブチャンネルのロゴを取得 *****

	// 地デジでかつサブチャンネルのみ、メインチャンネルにロゴがあればそれを利用する
	if channel.Type == "GR" && channel.IsSubchannel {
		mainChannel, err := database.GetMainChannelByNetworkID(r.Context(), s.db, channel.NetworkID)
		if err != nil && !errors.Is(err, database.ErrChannelNotFound) {
			return "", err
		}
		if mainChannel != nil {
			if fileName := mainChannel.ID + ".png"; fileExists(filepath.Join(logoDir, fileName)) {
				return fileName, nil
			}
		}
	}

	// BS でかつサブチャンネルのみ、メインチャンネルにロゴがあればそれを利用する
	if channel.Type == "BS" && channel.IsSubchannel {
		// メインチャンネルのサービス ID を算出する
		// NHKBS1 と NHKBSプレミアム だけ特別に、それ以外は一の位が1のサービス ID を算出する
		var mainServiceID int
		if channel.ServiceID == 102 {
			mainServiceID = 101
		} else if channel.ServiceID == 104 {
			mainServiceID = 103
		} else if len(channel.ChannelNumber) >= 2 {
			parsed, err := strconv.Atoi(channel.ChannelNumber[0:2] + "1")
			if err != nil {
				return "", nil
			}
			mainServiceID = parsed
		} else {
			return "", nil
		}
		mainChannel, err := database.GetChannelByNetworkIDAndServiceID(r.Context(), s.db, channel.NetworkID, mainServiceID)
		if err != nil && !errors.Is(err, database.ErrChannelNotFound) {
			return "", err
		}
		if mainChannel != nil {
			if fileName := mainChannel.ID + ".png"; fileExists(filepath.Join(logoDir, fileName)) {
				return fileName, nil
			}
		}
	}

	return "", nil
}

// fetchMirakurunChannelLogo は Mirakurun / mirakc の API からチャンネルロゴを取得する。
// 取得できなかった場合は ok に false を返す (エラーにはしない) 。
func (s *Server) fetchMirakurunChannelLogo(channel *database.Channel) ([]byte, string, bool) {
	// Mirakurun 形式のサービス ID (NID と SID を 5 桁でゼロ埋めした上で int に変換する)
	mirakurunServiceID := fmt.Sprintf("%05d%05d", channel.NetworkID, channel.ServiceID)
	logoURL := strings.TrimRight(s.config.General.MirakurunURL, "/") + "/api/services/" + mirakurunServiceID + "/logo"

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(logoURL)
	if err != nil {
		// API に接続できなかった際は特にエラーは吐かず、デフォルトのロゴ画像を利用する
		return nil, "", false
	}
	defer func() { _ = response.Body.Close() }()

	// ステータスコードが 503 の場合はロゴデータが存在しない
	if response.StatusCode != http.StatusOK {
		return nil, "", false
	}
	logoData, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", false
	}
	return logoData, "image/png", true
}

// writeChannelLogoData はロゴデータを ETag 付きで書き出す。
func (s *Server) writeChannelLogoData(w http.ResponseWriter, r *http.Request, logoData []byte, mediaType string) {
	etag := sha256HexBytes(logoData)
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("Cache-Control", logoCacheControl)
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Cache-Control", logoCacheControl)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logoData)
}

// serveIPTVChannelLogo は IPTV チャンネルのロゴを取得して書き出す。
// ロゴを取得できなかった場合は false を返す (呼び出し側で既定のロゴを返す) 。
func (s *Server) serveIPTVChannelLogo(w http.ResponseWriter, r *http.Request, displayChannelID string) bool {
	channel := s.iptv.GetChannelByDisplayChannelID(displayChannelID)
	if channel == nil || channel.LogoURL == nil {
		return false
	}

	// ロゴは CORS やホットリンク制限を受けていることがあるため、サーバー経由で取得する
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, *channel.LogoURL, nil)
	if err != nil {
		return false
	}
	request.Header.Set("User-Agent", s.config.IPTV.UserAgent)
	response, err := s.iptv.HTTPClient().Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return false
	}
	logoData, err := io.ReadAll(response.Body)
	if err != nil || len(logoData) == 0 {
		return false
	}

	mediaType := response.Header.Get("Content-Type")
	if index := strings.Index(mediaType, ";"); index >= 0 {
		mediaType = mediaType[:index]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	s.writeChannelLogoData(w, r, logoData, mediaType)
	return true
}

// serveChannelLogoFile は同梱のロゴファイルを ETag 付きで書き出す。
func (s *Server) serveChannelLogoFile(w http.ResponseWriter, r *http.Request, fileName string, etag string) {
	s.serveChannelLogoFileWithETag(w, r, filepath.Join(s.paths.StaticDir, "logos", fileName), etag)
}

// serveChannelLogoFileWithETag はロゴファイルを ETag 付きで書き出す。
func (s *Server) serveChannelLogoFileWithETag(w http.ResponseWriter, r *http.Request, logoPath string, etag string) {
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("Cache-Control", logoCacheControl)
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	file, err := os.Open(logoPath)
	if err != nil {
		s.logger.Error("failed to open logo file", "error", err, "path", logoPath)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		s.logger.Error("failed to stat logo file", "error", err, "path", logoPath)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	w.Header().Set("Cache-Control", logoCacheControl)
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, filepath.Base(logoPath), stat.ModTime(), file)
}

// fileExists は指定されたパスのファイルが存在するかどうかを返す。
func fileExists(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.Mode().IsRegular()
}

// sha256HexString は文字列の SHA-256 ハッシュを 16 進数文字列で返す。
func sha256HexString(value string) string {
	return sha256HexBytes([]byte(value))
}

// sha256HexBytes はバイト列の SHA-256 ハッシュを 16 進数文字列で返す。
func sha256HexBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
