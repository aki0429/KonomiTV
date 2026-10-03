package api

import (
	"net/http"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
)

// liveChannelsResponse は schemas.LiveChannels 互換のレスポンス。
// チャンネルタイプごとにチャンネルを分類して返す。
type liveChannelsResponse struct {
	GR   []*liveChannelResponse `json:"GR"`
	BS   []*liveChannelResponse `json:"BS"`
	CS   []*liveChannelResponse `json:"CS"`
	CATV []*liveChannelResponse `json:"CATV"`
	SKY  []*liveChannelResponse `json:"SKY"`
	BS4K []*liveChannelResponse `json:"BS4K"`
	// IPTV ページからテレビ視聴 UI に登録した IPTV チャンネルの疑似チャンネル
	IPTV []*liveChannelResponse `json:"IPTV"`
}

// channelProgramKey は番組情報をチャンネルに紐付けるためのキー。
type channelProgramKey struct {
	NetworkID int
	ServiceID int
}

// handleChannels は GET /api/channels (チャンネル情報一覧 API) を処理する。
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().In(constants.JST)

	// チャンネル情報 (リモコン番号 → チャンネル番号の順) を取得する
	channels, err := database.ListWatchableChannels(ctx, s.db)
	if err != nil {
		s.logger.Error("failed to list channels", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 現在と次の番組情報を一度に取得し、チャンネルごとに紐付ける
	presentFollowingPrograms, err := database.ListPresentAndFollowingPrograms(ctx, s.db, now)
	if err != nil {
		s.logger.Error("failed to list present and following programs", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	programsByChannel := map[channelProgramKey][]*database.PresentFollowingProgram{}
	for _, pfProgram := range presentFollowingPrograms {
		key := channelProgramKey{NetworkID: pfProgram.Program.NetworkID, ServiceID: pfProgram.Program.ServiceID}
		programsByChannel[key] = append(programsByChannel[key], pfProgram)
	}

	// レスポンスの雛形
	result := &liveChannelsResponse{
		GR:   []*liveChannelResponse{},
		BS:   []*liveChannelResponse{},
		CS:   []*liveChannelResponse{},
		CATV: []*liveChannelResponse{},
		SKY:  []*liveChannelResponse{},
		BS4K: []*liveChannelResponse{},
		IPTV: []*liveChannelResponse{},
	}

	for _, channel := range channels {
		key := channelProgramKey{NetworkID: channel.NetworkID, ServiceID: channel.ServiceID}
		present, following := pickPresentAndFollowingPrograms(programsByChannel[key])

		response, err := buildLiveChannelResponse(channel, present, following, 0)
		if err != nil {
			s.logger.Error("failed to build channel response", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}

		// 現在の視聴者数を取得する
		response.ViewerCount = s.liveStreams.GetViewerCount(response.DisplayChannelID)

		// せっかくチャンネルごとにループで回しているので、ここでチャンネルタイプごとの分類もやっておく
		switch response.Type {
		case "GR":
			result.GR = append(result.GR, response)
		case "BS":
			result.BS = append(result.BS, response)
		case "CS":
			result.CS = append(result.CS, response)
		case "CATV":
			result.CATV = append(result.CATV, response)
		case "SKY":
			result.SKY = append(result.SKY, response)
		case "BS4K":
			result.BS4K = append(result.BS4K, response)
		}
	}

	// 呼び出し元 (ログイン中のユーザー、または Cookie の匿名 ID) が IPTV ページから
	// テレビ視聴 UI に登録した IPTV チャンネルを追加する (登録内容は呼び出し元ごとに分離されている)
	userKey := s.auth.ResolveUserKey(w, r)
	for _, iptvChannel := range s.iptv.GetTVUIChannels(userKey) {
		response := toLiveChannelResponse(iptvChannel.ToLiveChannelResponse())
		// 現在の視聴者数を取得する
		response.ViewerCount = s.liveStreams.GetViewerCount(response.DisplayChannelID)
		result.IPTV = append(result.IPTV, response)
	}

	writeJSON(w, http.StatusOK, result)
}

// toLiveChannelResponse は IPTV の疑似チャンネルを API のレスポンス型に変換する。
func toLiveChannelResponse(channel *iptv.LiveChannelResponse) *liveChannelResponse {
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
		TerrestrialRegions: nil,
		JikkyoForce:        channel.JikkyoForce,
		IsSubchannel:       channel.IsSubchannel,
		IsRadiochannel:     channel.IsRadiochannel,
		IsWatchable:        channel.IsWatchable,
		IsDisplay:          channel.IsDisplay,
		ViewerCount:        channel.ViewerCount,
		// IPTV チャンネルには番組情報 (EPG) が存在しない
		ProgramPresent:   nil,
		ProgramFollowing: nil,
	}
}

// pickPresentAndFollowingPrograms は現在放送中の番組と次の番組を選び出す。
//
// Python 版 (ChannelsRouter) と同じ判定ロジックを再現する。
func pickPresentAndFollowingPrograms(
	programs []*database.PresentFollowingProgram,
) (present *database.Program, following *database.Program) {
	if len(programs) == 0 {
		return nil, nil
	}

	// 番組情報が1つしか取得できていない場合
	if len(programs) == 1 {
		if programs[0].IsPresent {
			// 現在放送中の番組のみを program_present にセットする
			return programs[0].Program, nil
		}
		// 次以降の番組情報のみを program_following にセットする
		return nil, programs[0].Program
	}

	// 番組情報が2つ取得できており、いずれも現在放送中の場合 (DB に放送時刻が重複する番組がある場合)
	if programs[0].IsPresent && programs[1].IsPresent {
		return programs[0].Program, nil
	}

	// 番組情報が2つ取得できており、いずれも次以降の番組情報の場合 (放送休止中やサブチャンネルなど)
	if !programs[0].IsPresent && !programs[1].IsPresent {
		// program_order が 1 になっている方 (= より現在時刻に近い) をセットする
		for _, program := range programs {
			if program.ProgramOrder == 1 {
				return nil, program.Program
			}
		}
		return nil, programs[0].Program
	}

	// それ以外 (どちらかが現在放送中で、どちらかが次の番組情報の場合)
	for _, program := range programs {
		if program.IsPresent && present == nil {
			present = program.Program
		} else if !program.IsPresent && following == nil {
			following = program.Program
		}
	}
	return present, following
}
