package api

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo"
)

// timeTableSubchannelGroupKey は番組表でサブチャンネルを同じ列へ入れるためのグループキー。
type timeTableSubchannelGroupKey struct {
	Kind      string // "TS" or "BSService"
	NetworkID int
	Value     int
}

// channelNumberPattern はチャンネル番号 (枝番つき) のパターン。
var channelNumberPattern = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

// timeTableChannelResponse は schemas.Channel 互換のレスポンス (terrestrial_regions は常に null) 。
type timeTableChannelResponse struct {
	ID                string `json:"id"`
	DisplayChannelID  string `json:"display_channel_id"`
	NetworkID         int    `json:"network_id"`
	ServiceID         int    `json:"service_id"`
	TransportStreamID *int   `json:"transport_stream_id"`
	RemoconID         int    `json:"remocon_id"`
	ChannelNumber     string `json:"channel_number"`
	Type              string `json:"type"`
	Name              string `json:"name"`
	TerrestrialRegion any    `json:"terrestrial_regions"`
	JikkyoForce       *int   `json:"jikkyo_force"`
	IsSubchannel      bool   `json:"is_subchannel"`
	IsRadiochannel    bool   `json:"is_radiochannel"`
	IsWatchable       bool   `json:"is_watchable"`
}

// timeTableProgramResponse は schemas.TimeTableProgram 互換のレスポンス。
type timeTableProgramResponse struct {
	programResponse
	Reservation *timeTableProgramReservationResponse `json:"reservation"`
}

// timeTableProgramReservationResponse は schemas.TimeTableProgramReservation 互換のレスポンス。
type timeTableProgramReservationResponse struct {
	ID                    int    `json:"id"`
	Status                string `json:"status"`
	RecordingAvailability string `json:"recording_availability"`
}

// timeTableSubchannelResponse は schemas.TimeTableSubchannel 互換のレスポンス。
type timeTableSubchannelResponse struct {
	Channel  timeTableChannelResponse   `json:"channel"`
	Programs []timeTableProgramResponse `json:"programs"`
}

// timeTableChannelEntry は schemas.TimeTableChannel 互換のレスポンス。
type timeTableChannelEntry struct {
	Channel     timeTableChannelResponse      `json:"channel"`
	Programs    []timeTableProgramResponse    `json:"programs"`
	Subchannels []timeTableSubchannelResponse `json:"subchannels"`
}

// timeTableDateRangeResponse は schemas.TimeTableDateRange 互換のレスポンス。
type timeTableDateRangeResponse struct {
	Earliest string `json:"earliest"`
	Latest   string `json:"latest"`
}

// timeTableResponse は schemas.TimeTable 互換のレスポンス。
type timeTableResponse struct {
	Channels  []timeTableChannelEntry    `json:"channels"`
	DateRange timeTableDateRangeResponse `json:"date_range"`
}

// handleProgramSearch は POST /api/programs/search (番組検索 API) を処理する。
// この API は EDCB バックエンド専用のため、それ以外のバックエンドでは 422 を返し、
// EDCB バックエンドの場合は Go 側に EDCB クライアントの実装がないため Python 版へプロキシする。
func (s *Server) handleProgramSearch(w http.ResponseWriter, r *http.Request) {
	if s.config.General.Backend != "EDCB" {
		s.logger.Warn("[ReservationsRouter][GetCtrlCmdUtil] This API is only available when the backend is EDCB.")
		writeError(w, http.StatusUnprocessableEntity, "This API is only available when the backend is EDCB")
		return
	}
	if s.proxy == nil {
		writeError(w, http.StatusBadGateway, "Bad Gateway")
		return
	}
	s.proxy.ServeHTTP(w, r)
}

// handleProgramTimeTable は GET /api/programs/timetable (番組表 API) を処理する。
func (s *Server) handleProgramTimeTable(w http.ResponseWriter, r *http.Request) {
	now := time.Now().In(constants.JST)
	query := r.URL.Query()

	// 開始時刻のデフォルト値: 現在時刻
	startTime := now
	if value := query.Get("start_time"); value != "" {
		parsed, ok := parseQueryDatetime(value)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "Invalid start_time")
			return
		}
		startTime = parsed
	}

	// チャンネル種別とピン留めチャンネル ID をパースする
	var channelType *string
	if value := query.Get("channel_type"); value != "" {
		channelType = &value
	}
	var pinnedChannelIDs []string
	if value := query.Get("pinned_channel_ids"); value != "" && strings.TrimSpace(value) != "" {
		for _, channelID := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(channelID); trimmed != "" {
				pinnedChannelIDs = append(pinnedChannelIDs, trimmed)
			}
		}
	}

	// 番組データの日付範囲を取得 (日付セレクター用)
	dateRange, err := database.GetProgramDateRange(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to get program date range", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	earliest := now
	if dateRange.Earliest != nil {
		earliest = *dateRange.Earliest
	}
	latest := now.Add(7 * 24 * time.Hour)
	if dateRange.Latest != nil {
		latest = *dateRange.Latest
	}

	// 終了時刻のデフォルト値: DB に存在する最終日時
	endTime := latest
	if value := query.Get("end_time"); value != "" {
		parsed, ok := parseQueryDatetime(value)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "Invalid end_time")
			return
		}
		endTime = parsed
	}

	// チャンネル情報を取得する
	channels, err := database.ListWatchableChannelsForTimeTable(r.Context(), s.db, channelType, pinnedChannelIDs)
	if err != nil {
		s.logger.Error("failed to list channels", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if len(channels) == 0 {
		writeJSON(w, http.StatusOK, timeTableResponse{
			Channels: []timeTableChannelEntry{},
			DateRange: timeTableDateRangeResponse{
				Earliest: database.FormatJSONTime(earliest),
				Latest:   database.FormatJSONTime(latest),
			},
		})
		return
	}

	// ピン留め指定がない通常番組表では、枝番つき地デジ局のサブチャンネルが同じ局の近くに並ぶ順序に直す
	if pinnedChannelIDs == nil {
		sort.SliceStable(channels, func(i int, j int) bool {
			return compareTimeTableChannelSortKey(channels[i], channels[j]) < 0
		})
	} else {
		// 指定された順序でソートする
		channelIDOrder := map[string]int{}
		for index, channelID := range pinnedChannelIDs {
			channelIDOrder[channelID] = index
		}
		sort.SliceStable(channels, func(i int, j int) bool {
			left, leftOK := channelIDOrder[channels[i].ID]
			right, rightOK := channelIDOrder[channels[j].ID]
			if !leftOK {
				left = int(^uint(0) >> 1)
			}
			if !rightOK {
				right = int(^uint(0) >> 1)
			}
			return left < right
		})
	}

	// サブチャンネル結合用キーごとにチャンネルをグループ化する
	groupedChannels := map[timeTableSubchannelGroupKey][]database.Channel{}
	for _, channel := range channels {
		groupKey := getTimeTableSubchannelGroupKey(channel)
		if groupKey == nil {
			continue
		}
		groupedChannels[*groupKey] = append(groupedChannels[*groupKey], channel)
	}

	// サブチャンネル放送時間の集計を取得する
	durations, err := database.GetSubchannelDurations(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to get subchannel durations", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	durationsByGroup := map[timeTableSubchannelGroupKey]map[int]map[string]float64{}
	for _, duration := range durations {
		groupKey := getTimeTableSubchannelDurationGroupKey(duration)
		if groupKey == nil {
			continue
		}
		if durationsByGroup[*groupKey] == nil {
			durationsByGroup[*groupKey] = map[int]map[string]float64{}
		}
		if durationsByGroup[*groupKey][duration.ServiceID] == nil {
			durationsByGroup[*groupKey][duration.ServiceID] = map[string]float64{}
		}
		durationsByGroup[*groupKey][duration.ServiceID][duration.BroadcastDate] = duration.TotalDuration
	}

	// 8時間ルールに基づいて独立サブチャンネルを判定する (閾値: 8時間 = 28800秒)
	const independentSubchannelThreshold = 8 * 3600
	independentSubchannelsByGroup := map[timeTableSubchannelGroupKey]map[int]bool{}
	for groupKey, durationsByService := range durationsByGroup {
		independentSubchannels := map[int]bool{}
		for serviceID, dailyDurations := range durationsByService {
			// いずれかの日で閾値以上の放送時間があれば独立チャンネルとして判定する
			for _, duration := range dailyDurations {
				if duration >= independentSubchannelThreshold {
					independentSubchannels[serviceID] = true
					break
				}
			}
		}
		independentSubchannelsByGroup[groupKey] = independentSubchannels
	}

	// 番組情報を取得する
	channelIDs := make([]string, 0, len(channels))
	for _, channel := range channels {
		channelIDs = append(channelIDs, channel.ID)
	}
	programs, err := database.ListProgramsForTimeTable(
		r.Context(), s.db, channelIDs, database.FormatDBTime(startTime), database.FormatDBTime(endTime),
	)
	if err != nil {
		s.logger.Error("failed to list programs", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// チャンネルごとに番組をグループ化する
	programsByChannel := map[string][]timeTableProgramResponse{}
	for _, channelID := range channelIDs {
		programsByChannel[channelID] = []timeTableProgramResponse{}
	}
	for index := range programs {
		program := &programs[index]
		if _, ok := programsByChannel[program.ChannelID]; !ok {
			continue
		}
		response, err := buildProgramResponse(program)
		if err != nil {
			s.logger.Error("failed to build program response", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		programsByChannel[program.ChannelID] = append(programsByChannel[program.ChannelID], timeTableProgramResponse{programResponse: *response})
	}

	// レスポンスを構築する
	resultChannels := make([]timeTableChannelEntry, 0, len(channels))
	for _, channel := range channels {
		groupKey := getTimeTableSubchannelGroupKey(channel)
		independentSubchannels := map[int]bool{}
		if groupKey != nil {
			independentSubchannels = independentSubchannelsByGroup[*groupKey]
		}

		// このチャンネルがサブチャンネルかつ独立サブチャンネルでない場合はスキップする (メインチャンネルの subchannels に含める)
		if channel.IsSubchannel && !independentSubchannels[channel.ServiceID] {
			continue
		}

		programsList := programsByChannel[channel.ID]

		// サブチャンネルのリストを収集する (8時間未満のサブチャンネルのみ)
		var subchannels []timeTableSubchannelResponse
		if !channel.IsSubchannel && groupKey != nil {
			for _, subChannel := range groupedChannels[*groupKey] {
				if !subChannel.IsSubchannel || independentSubchannels[subChannel.ServiceID] {
					continue
				}
				// サブチャンネルの SID がメインチャンネルの SID より小さい場合はスキップする
				if subChannel.ServiceID < channel.ServiceID {
					continue
				}
				subPrograms := programsByChannel[subChannel.ID]
				if len(subPrograms) > 0 {
					subchannels = append(subchannels, timeTableSubchannelResponse{
						Channel:  buildTimeTableChannelResponse(subChannel),
						Programs: subPrograms,
					})
				}
			}
		}

		resultChannels = append(resultChannels, timeTableChannelEntry{
			Channel:     buildTimeTableChannelResponse(channel),
			Programs:    programsList,
			Subchannels: subchannels,
		})
	}

	writeJSON(w, http.StatusOK, timeTableResponse{
		Channels: resultChannels,
		DateRange: timeTableDateRangeResponse{
			Earliest: database.FormatJSONTime(earliest),
			Latest:   database.FormatJSONTime(latest),
		},
	})
}

// buildTimeTableChannelResponse は Channel から schemas.Channel 互換のレスポンスを構築する。
// 番組表では terrestrial_regions を設定しないため常に null になる。
func buildTimeTableChannelResponse(channel database.Channel) timeTableChannelResponse {
	return timeTableChannelResponse{
		ID:                channel.ID,
		DisplayChannelID:  channel.DisplayChannelID,
		NetworkID:         channel.NetworkID,
		ServiceID:         channel.ServiceID,
		TransportStreamID: channel.TransportStreamID,
		RemoconID:         channel.RemoconID,
		ChannelNumber:     channel.ChannelNumber,
		Type:              channel.Type,
		Name:              channel.Name,
		TerrestrialRegion: nil,
		JikkyoForce:       channel.JikkyoForce,
		IsSubchannel:      channel.IsSubchannel,
		IsRadiochannel:    channel.IsRadiochannel,
		IsWatchable:       channel.IsWatchable,
	}
}

// timeTableChannelSortKey は番組表で利用するチャンネルの並び替えキー。
// Python 版 GetTimeTableChannelSortKey() のタプル (int, int, int, int, str) に対応する。
type timeTableChannelSortKey struct {
	Numbers [4]int
	Text    string
}

// compareTimeTableChannelSortKey は番組表で利用するチャンネル並び替えキーを比較する。
// Python 版 GetTimeTableChannelSortKey() と同じ順序になるようにする。
func compareTimeTableChannelSortKey(left database.Channel, right database.Channel) int {
	leftKey := getTimeTableChannelSortKey(left)
	rightKey := getTimeTableChannelSortKey(right)
	for i := range leftKey.Numbers {
		if leftKey.Numbers[i] != rightKey.Numbers[i] {
			return leftKey.Numbers[i] - rightKey.Numbers[i]
		}
	}
	return strings.Compare(leftKey.Text, rightKey.Text)
}

// getTimeTableChannelSortKey はチャンネルの並び替えキーを返す。
func getTimeTableChannelSortKey(channel database.Channel) timeTableChannelSortKey {
	channelNumber := channel.ChannelNumber
	matched := channelNumberPattern.FindStringSubmatch(channelNumber)

	// 想定外のチャンネル番号は末尾に回し、DB に残っている文字列表現で順序を安定させる
	if matched == nil {
		return timeTableChannelSortKey{
			Numbers: [4]int{999999, 999999, 999999, channel.ServiceID},
			Text:    channelNumber,
		}
	}

	baseChannelNumber, _ := strconv.Atoi(matched[1])
	branchNumber := 0
	if matched[2] != "" {
		branchNumber, _ = strconv.Atoi(matched[2])
	}

	// 地上波では同一局にチャンネルが複数ある場合、枝番を優先して並び替える
	if channel.Type == "GR" {
		remoconID := baseChannelNumber / 10
		serviceNumber := baseChannelNumber % 10
		return timeTableChannelSortKey{
			Numbers: [4]int{remoconID, branchNumber, serviceNumber, channel.ServiceID},
			Text:    channelNumber,
		}
	}

	// 地デジ以外は従来通り3桁番号を主キーにしつつ、念のため枝番つき番号も自然な順序にする
	return timeTableChannelSortKey{
		Numbers: [4]int{baseChannelNumber, branchNumber, 0, channel.ServiceID},
		Text:    channelNumber,
	}
}

// getTimeTableSubchannelGroupKey は番組表でサブチャンネルを同じ列へ入れるためのグループキーを取得する。
func getTimeTableSubchannelGroupKey(channel database.Channel) *timeTableSubchannelGroupKey {
	// TSID がある場合は放送波の単位をそのまま使う
	if channel.TransportStreamID != nil {
		return &timeTableSubchannelGroupKey{Kind: "TS", NetworkID: channel.NetworkID, Value: *channel.TransportStreamID}
	}
	// TSID がない BS は、既知のマルチ編成だけサービス ID から親サービスへ寄せる
	if channel.Type == "BS" {
		if parent := tsinfo.CalculateSubchannelParentServiceID("BS", channel.ServiceID); parent != nil {
			return &timeTableSubchannelGroupKey{Kind: "BSService", NetworkID: channel.NetworkID, Value: *parent}
		}
		return &timeTableSubchannelGroupKey{Kind: "BSService", NetworkID: channel.NetworkID, Value: channel.ServiceID}
	}
	// TSID もサービス ID からの親子判定もないチャンネルは、誤結合を避けるため単独扱いにする
	return nil
}

// getTimeTableSubchannelDurationGroupKey は集計結果の行からグループキーを取得する。
func getTimeTableSubchannelDurationGroupKey(duration database.SubchannelDuration) *timeTableSubchannelGroupKey {
	if duration.TransportStreamID != nil {
		return &timeTableSubchannelGroupKey{Kind: "TS", NetworkID: duration.NetworkID, Value: *duration.TransportStreamID}
	}
	if duration.Type == "BS" {
		if parent := tsinfo.CalculateSubchannelParentServiceID("BS", duration.ServiceID); parent != nil {
			return &timeTableSubchannelGroupKey{Kind: "BSService", NetworkID: duration.NetworkID, Value: *parent}
		}
		return &timeTableSubchannelGroupKey{Kind: "BSService", NetworkID: duration.NetworkID, Value: duration.ServiceID}
	}
	return nil
}

// parseQueryDatetime はクエリパラメーターの日時文字列を JST の time.Time に変換する。
// タイムゾーンが指定されていない場合は JST として扱う (NormalizeToJSTDatetime と同じ) 。
func parseQueryDatetime(value string) (time.Time, bool) {
	parsed, err := database.ParseDBTime(value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.In(constants.JST), true
}
