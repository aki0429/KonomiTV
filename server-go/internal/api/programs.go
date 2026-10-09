package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
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
	Channel  channelResponse            `json:"channel"`
	Programs []timeTableProgramResponse `json:"programs"`
}

// timeTableChannelEntry は schemas.TimeTableChannel 互換のレスポンス。
type timeTableChannelEntry struct {
	Channel     channelResponse               `json:"channel"`
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
// EDCB バックエンドではプロキシ有効時の既存転送を維持し、無効時だけ SearchPg を直接送信する。
func (s *Server) handleProgramSearch(w http.ResponseWriter, r *http.Request) {
	if s.config.General.Backend != "EDCB" {
		s.logger.Warn("[ReservationsRouter][GetCtrlCmdUtil] This API is only available when the backend is EDCB.")
		writeError(w, http.StatusUnprocessableEntity, "This API is only available when the backend is EDCB")
		return
	}
	// ストラングラー構成では body の検証も含め Python に委ね、既存の転送契約を変えない。
	// -no-proxy (s.proxy == nil) の場合だけ下の Go native 経路へ進む。
	if s.proxy != nil {
		s.proxy.ServeHTTP(w, r)
		return
	}
	var request programSearchConditionRequest
	if !decodeJSONBody(r, &request) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	condition, err := request.toProgramSearchCondition()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	client, err := reservations.NewClientFromURLContext(r.Context(), s.config.General.EDCBURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	key, err := s.encodeEDCBSearchKeyInfo(r.Context(), condition, client, nil)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	// Python 版の検索失敗時はエラーではなく空の検索結果を返す。
	events, _ := client.SearchPg([]reservations.SearchKeyInfo{key})
	programs := []programResponse{}
	if len(events) > 0 {
		channels, err := database.ListWatchableChannels(r.Context(), s.db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		// ONID/SID だけでは別 TS の番組を誤って参照するため、必ず三つ組で照合する。
		channelIDs := map[[3]int]string{}
		for _, channel := range channels {
			if channel.TransportStreamID != nil {
				channelIDs[[3]int{channel.NetworkID, *channel.TransportStreamID, channel.ServiceID}] = channel.ID
			}
		}
		now := time.Now()
		for _, event := range events {
			channelID, exists := channelIDs[[3]int{event.Onid, event.Tsid, event.Sid}]
			if !exists {
				continue
			}
			// イベント共有の副側は Python 版同様に結果から除く。
			if group := event.EventGroupInfo; group != nil && len(group.EventDataList) == 1 {
				primary := group.EventDataList[0]
				if primary.Onid != event.Onid || primary.Tsid != event.Tsid || primary.Sid != event.Sid || primary.Eid != event.Eid {
					continue
				}
			}
			program := decodeSearchEvent(event)
			end, _ := time.Parse(time.RFC3339, program.EndTime)
			if !end.After(now) {
				continue
			}
			program.ChannelID = channelID
			programs = append(programs, program)
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Total    int               `json:"total"`
		Programs []programResponse `json:"programs"`
	}{Total: len(programs), Programs: programs})
}

// decodeSearchEvent は ProgramsRouter.DecodeEDCBEventInfo の Program 表現を作る。
func decodeSearchEvent(event reservations.EventInfo) programResponse {
	start := time.Date(1970, 1, 1, 9, 0, 0, 0, constants.JST)
	if event.StartTime != nil {
		start = event.StartTime.In(constants.JST)
	}
	duration := 300
	if event.DurationSec != nil {
		duration = *event.DurationSec
	}
	p := programResponse{ID: fmt.Sprintf("NID%d-SID%03d-EID%d", event.Onid, event.Sid, event.Eid), ChannelID: fmt.Sprintf("NID%d-SID%03d", event.Onid, event.Sid), NetworkID: event.Onid, ServiceID: event.Sid, EventID: event.Eid, StartTime: database.FormatJSONTime(start), EndTime: database.FormatJSONTime(start.Add(time.Duration(duration) * time.Second)), Duration: pydanticFloat64(duration), IsFree: event.FreeCaFlag == 0, Genres: json.RawMessage("[]")}
	if event.ShortInfo != nil {
		p.Title = strings.TrimSpace(reservations.FormatString(event.ShortInfo.EventName))
		p.Description = strings.TrimSpace(reservations.FormatString(event.ShortInfo.TextChar))
	}
	// 見出し重複はタブを足して保持し、概要が空の時だけ最初の本文で補う。
	var detail reservations.ProgramDetail
	if event.ExtInfo != nil {
		detail = reservations.ParseProgramExtendedText(event.ExtInfo.TextChar).Normalized()
		for _, entry := range detail.Entries() {
			if strings.TrimSpace(p.Description) == "" {
				p.Description = entry.Body
			}
		}
	}
	// 空テキストは Python 版でも項目を作らない。明示的な空見出しは保持する。
	p.Detail, _ = json.Marshal(detail)
	genres := []genreSchema{}
	if event.ContentInfo != nil {
		for _, content := range event.ContentInfo.NibbleList {
			major, ok := reservations.FindGenreMajor(content.ContentNibble >> 8)
			if !ok {
				continue
			}
			genre := genreSchema{Major: strings.ReplaceAll(major.Major, "／", "・"), Middle: "未定義"}
			for _, middle := range major.Middle {
				if middle.Key == content.ContentNibble&0xf {
					genre.Middle = strings.ReplaceAll(middle.Name, "／", "・")
					break
				}
			}
			if genre.Major == "拡張" {
				if genre.Middle != "BS/地上デジタル放送用番組付属情報" {
					continue
				}
				genre.Middle = "未定義"
				user := content.UserNibble>>8<<4 | content.UserNibble&0xf
				for _, entry := range reservations.UserTypes() {
					if entry.Key == user {
						genre.Middle = entry.Name
						break
					}
				}
			}
			genres = append(genres, genre)
		}
	}
	p.Genres, _ = json.Marshal(genres)
	if video := event.ComponentInfo; video != nil {
		if value, ok := reservations.ComponentTypes[video.StreamContent][video.ComponentType]; ok {
			p.VideoType = &value
		}
		if value, ok := reservations.VideoCodecs[video.StreamContent]; ok {
			p.VideoCodec = &value
		}
		if value, ok := reservations.VideoResolutions[video.ComponentType]; ok {
			p.VideoResolution = &value
		}
	}
	if audio := event.AudioInfo; audio != nil {
		for index, item := range audio.ComponentList {
			if index > 1 {
				break
			}
			typeName := reservations.ComponentTypes[2][item.ComponentType]
			rate := reservations.SamplingRates[item.SamplingRate]
			language := "日本語"
			if index == 1 {
				language = "副音声"
			}
			if typeName == "1/0+1/0モード(デュアルモノ)" {
				if item.EsMultiLingualFlag != 0 {
					language += "+英語"
				} else {
					language += "+副音声"
				}
			}
			if index == 0 {
				p.PrimaryAudioType = typeName
				p.PrimaryAudioLanguage = language
				p.PrimaryAudioSamplingRate = rate
			} else {
				p.SecondaryAudioType = &typeName
				p.SecondaryAudioLanguage = &language
				p.SecondaryAudioSamplingRate = &rate
			}
		}
	}
	return p
}

// handleProgramTimeTable は GET /api/programs/timetable (番組表 API) を処理する。
func (s *Server) handleProgramTimeTable(w http.ResponseWriter, r *http.Request) {
	now := time.Now().In(constants.JST)
	query := r.URL.Query()
	v := newFastAPIValidation(r)

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

	// チャンネル種別は Literal['GR', 'BS', 'CS', 'CATV', 'SKY', 'BS4K'] で、無効値は literal_error にする。
	// 空文字も「未指定」ではなく許容外の入力として扱う (Python の str は空文字も有効値のため欠落と区別される) 。
	channelTypeValue := v.queryLiteral(
		"channel_type",
		[]string{"GR", "BS", "CS", "CATV", "SKY", "BS4K"},
		"",
		"'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'",
	)
	var channelType *string
	if channelTypeValue != "" {
		channelType = &channelTypeValue
	}

	// ピン留めチャンネル ID をパースする
	var pinnedChannelIDs []string
	if value := query.Get("pinned_channel_ids"); value != "" && strings.TrimSpace(value) != "" {
		for _, channelID := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(channelID); trimmed != "" {
				pinnedChannelIDs = append(pinnedChannelIDs, trimmed)
			}
		}
	}

	// クエリパラメータの検証エラー (channel_type の literal_error など) があれば
	// FastAPI 互換の 422 を返し、DB アクセスへは進まない
	if v.writeIfInvalid(w) {
		return
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
						Channel:  dereferenceChannelResponse(buildChannelResponse(&subChannel)),
						Programs: subPrograms,
					})
				}
			}
		}

		resultChannels = append(resultChannels, timeTableChannelEntry{
			Channel:     dereferenceChannelResponse(buildChannelResponse(&channel)),
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
