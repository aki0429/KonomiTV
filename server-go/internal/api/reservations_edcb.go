package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/routers/ReservationsRouter.py のうち EDCB 連携部分
// (CtrlCmdUtil を使う処理・EDCB の生データのデコード) を移植したもの。
//
// 実機の EDCB へ接続するテストは行わないため、EDCB クライアントは edcbClient
// インターフェースで抽象化し、テストでは edcbClientFactory を差し替える。

// edcbClient は録画予約系ルーターが使う EDCB クライアントの必要最小限のインターフェース。
type edcbClient interface {
	EnumReserve() ([]reservations.ReserveData, bool)
	AddReserve(reserveList []reservations.ReserveData) bool
	ChgReserve(reserveList []reservations.ReserveData) bool
	DelReserve(reserveIDList []int) bool
	EnumPgInfoEx(serviceTimeList []int64) ([]reservations.ServiceEventInfo, bool)
	FileCopy(name string) ([]byte, bool)
	FileCopy2(nameList []string) ([]reservations.FileData, bool)
	GetRecFilePath(reserveID int) *string
	EnumAutoAdd() ([]reservations.AutoAddData, bool)
	AddAutoAdd(dataList []reservations.AutoAddData) bool
	ChgAutoAdd(dataList []reservations.AutoAddData) bool
	DelAutoAdd(idList []int) bool
}

// edcbClientFactory は EDCB クライアントを生成する。
// テストで実機へ接続しないよう差し替えられるようにグローバル変数にしている。
var edcbClientFactory = func(cfg *config.Config) (edcbClient, error) {
	return reservations.NewClientFromURL(cfg.General.EDCBURL)
}

// getEDCBClient はバックエンドが EDCB かどうかを確認し、EDCB であればクライアントを返す。
//
// 移植元: ReservationsRouter.GetCtrlCmdUtil()
// バックエンドが EDCB でない場合は Python 版と同じ 422 を返す。
func (s *Server) getEDCBClient(w http.ResponseWriter) (edcbClient, bool) {
	if s.config.General.Backend != "EDCB" {
		s.logger.Warn("[ReservationsRouter][GetCtrlCmdUtil] This API is only available when the backend is EDCB.")
		writeError(w, http.StatusUnprocessableEntity, "This API is only available when the backend is EDCB")
		return nil, false
	}
	client, err := edcbClientFactory(s.config)
	if err != nil {
		s.logger.Error("[ReservationsRouter][GetCtrlCmdUtil] Failed to create the EDCB client.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, false
	}
	return client, true
}

// bitrateHardcoded は常時マルチチャンネル放送のため例外的に決め打ちの値を使うチャンネル。
// キー: (NID, SID) 、値: ビットレート (kbps) 。
var bitrateHardcoded = map[[2]int]int{
	{32391, 23608}: 12000, // TOKYO MX1(091ch) (12Mbps)
	{32391, 23609}: 12000, // TOKYO MX1(092ch) (12Mbps)
	{32391, 23610}: 4800,  // TOKYO MX2(093ch) (4.8Mbps)
	{32381, 24680}: 10000, // イッツコムch10(101ch) (10Mbps)
	{32381, 24681}: 10000, // イッツコムch10(102ch) (10Mbps)
	{32383, 24696}: 10000, // イッツコムch10(111ch) (10Mbps)
	{32383, 24697}: 10000, // イッツコムch10(112ch) (10Mbps)
}

// defaultBitrateKbps は Bitrate.ini から該当するビットレートが取得できなかった場合の既定値。
const defaultBitrateKbps = 19456

// bitrateCacheTTL は Bitrate.ini のキャッシュ保持時間 (900 秒 = 15 分) 。
const bitrateCacheTTL = 900 * time.Second

// bitrateCache は Bitrate.ini の内容を保持するプロセス内キャッシュ。
type bitrateIniCache struct {
	mu       sync.Mutex
	values   map[string]int
	loadedAt time.Time
	loaded   bool
}

var bitrateCache bitrateIniCache

// lookupBitrate はキャッシュから指定されたチャンネルのビットレートを段階的に探す。
// 全指定 -> SID=0xFFFF -> TSID=0xFFFF -> ONID=0xFFFF の順に探し、見つからなければ既定値を返す。
func lookupBitrate(values map[string]int, networkID int, transportStreamID int, serviceID int) int {
	for i := 0; i < 4; i++ {
		onid := networkID
		if i > 2 {
			onid = 0xFFFF
		}
		tsid := transportStreamID
		if i > 1 {
			tsid = 0xFFFF
		}
		sid := serviceID
		if i > 0 {
			sid = 0xFFFF
		}
		// EpgTimer の Create64Key ロジック: (onid << 32 | tsid << 16 | sid)
		key := fmt.Sprintf("%012X", onid<<32|tsid<<16|sid)
		if value, ok := values[key]; ok && value > 0 {
			return value
		}
	}
	return defaultBitrateKbps
}

// getBitrateFromEDCB は EDCB から Bitrate.ini を取得して指定されたチャンネルのビットレートを返す。
//
// 移植元: ReservationsRouter.DecodeEDCBReserveData() 内の GetBitrateFromEDCB()
func (s *Server) getBitrateFromEDCB(ctx context.Context, client edcbClient, networkID int, transportStreamID int, serviceID int) int {
	// 決め打ちの値が設定されているかチェック
	if value, ok := bitrateHardcoded[[2]int{networkID, serviceID}]; ok {
		return value
	}

	// キャッシュが存在し、かつ 15 分以内の場合はそれを使用
	bitrateCache.mu.Lock()
	defer bitrateCache.mu.Unlock()
	if bitrateCache.loaded && time.Since(bitrateCache.loadedAt) < bitrateCacheTTL {
		return lookupBitrate(bitrateCache.values, networkID, transportStreamID, serviceID)
	}

	// EDCB から Bitrate.ini を取得
	files, ok := client.FileCopy2([]string{"Bitrate.ini"})
	if !ok || len(files) == 0 {
		s.logger.Warn("[ReservationsRouter][GetBitrateFromEDCB] Failed to get Bitrate.ini from EDCB.")
		return defaultBitrateKbps
	}
	if len(files[0].Data) == 0 {
		s.logger.Warn("[ReservationsRouter][GetBitrateFromEDCB] Bitrate.ini is empty.")
		return defaultBitrateKbps
	}

	// バイナリデータを文字列に変換
	// (Linux 版 EDCB では UTF-8 (BOM なし) で返る場合があるため、BOM 判定 + UTF-8 優先の同じ変換を使う)
	iniText := reservations.ConvertEDCBBytesToString(files[0].Data)

	// EDCB 由来の ini を重複キーを許容して解析
	// (Bitrate.ini のキーは後段で大文字に変換してから参照するため、ここでは大文字小文字を保持しない)
	parsed, err := reservations.ParseEDCBIni(iniText, false)
	if err != nil {
		s.logger.Error("[ReservationsRouter][GetBitrateFromEDCB] Failed to parse Bitrate.ini.", "error", err)
		return defaultBitrateKbps
	}

	// BITRATE セクションからビットレート情報を取得してキャッシュに保存
	values := map[string]int{}
	if parsed.HasSection("BITRATE") {
		for key, value := range parsed.Items("BITRATE") {
			parsedValue, err := parseIntValue(value)
			if err != nil {
				continue
			}
			values[strings.ToUpper(key)] = parsedValue
		}
	}
	bitrateCache.values = values
	bitrateCache.loadedAt = time.Now()
	bitrateCache.loaded = true

	return lookupBitrate(values, networkID, transportStreamID, serviceID)
}

// parseIntValue は Python の int() と同じく前後の空白を許容して 10 進整数を解釈する。
// 解釈できない場合はエラーを返し、呼び出し元がその値を読み飛ばす。
func parseIntValue(value string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(value))
}

// calculateEstimatedFileSize は録画予定時間とビットレートから想定ファイルサイズを計算する。
//
// 移植元: ReservationsRouter.DecodeEDCBReserveData() 内の CalculateEstimatedFileSize()
func calculateEstimatedFileSize(durationSeconds float64, bitrateKbps int, recordingMode string) int {
	// 視聴モードの場合は想定サイズ 0 を返す (録画されないため)
	if recordingMode == "View" {
		return 0
	}

	// EpgTimer のロジック: bitrate / 8 * 1000 * duration (秒)
	estimatedSizeBytes := int(float64(bitrateKbps) / 8 * 1000 * durationSeconds)
	if estimatedSizeBytes < 0 {
		return 0
	}
	return estimatedSizeBytes
}

// reservationLookup は DecodeEDCBReserveData() にあらかじめ取得済みのチャンネル・番組情報を渡すための入れ物。
//
// Python 版は channels / programs に None を渡すと都度 DB から取得し、
// リスト・辞書を渡すとそれを検索する。Go では useChannels / usePrograms で区別する。
type reservationLookup struct {
	channels    []database.Channel
	useChannels bool
	programs    map[reservations.ProgramKey]database.Program
	usePrograms bool
}

// decodeEDCBReserveData は EDCB の ReserveData を schemas.Reservation 互換のレスポンスに変換する。
//
// 移植元: ReservationsRouter.DecodeEDCBReserveData()
func (s *Server) decodeEDCBReserveData(
	ctx context.Context,
	client edcbClient,
	reserveData reservations.ReserveData,
	lookup *reservationLookup,
	isRecordingInProgress bool,
) (*reservationResponse, error) {
	// 録画予約 ID
	reserveID := reserveData.ReserveID

	// 録画対象チャンネルのネットワーク ID / サービス ID / トランスポートストリーム ID
	networkID := reserveData.Onid
	serviceID := reserveData.Sid
	transportStreamID := reserveData.Tsid

	// 録画対象チャンネルのサービス名 (基本全角なので半角に変換する)
	serviceName := reservations.FormatString(reserveData.StationName)

	// ここで ONID・SID・TSID が一致するチャンネルをデータベースから取得する
	var channel *database.Channel
	if lookup != nil && lookup.useChannels {
		for index := range lookup.channels {
			candidate := &lookup.channels[index]
			if candidate.NetworkID == networkID && candidate.ServiceID == serviceID &&
				candidate.TransportStreamID != nil && *candidate.TransportStreamID == transportStreamID {
				channel = candidate
				break
			}
		}
	} else {
		found, err := reservations.GetChannelByKey(ctx, s.db, reservations.ChannelKey{
			NetworkID: networkID, TransportStreamID: transportStreamID, ServiceID: serviceID,
		})
		if err != nil && !errors.Is(err, reservations.ErrNotFound) {
			return nil, err
		}
		channel = found
	}

	var channelResp *channelResponse
	if channel != nil {
		channelResp = buildChannelResponse(channel)
	} else {
		// 取得できなかった場合のみ、上記の限定的な情報を使って間に合わせのチャンネル情報を作成する
		// (通常ここでチャンネル情報が取得できないのはワンセグやデータ放送など KonomiTV ではサポートしていないサービスを予約している場合だけ)
		channelType := reservations.GetNetworkType(networkID)
		channelID := fmt.Sprintf("NID%d-SID%d", networkID, serviceID)
		remoconID := 0
		if channelType != "GR" {
			remoconID = reservations.CalculateRemoconID(channelType, serviceID)
		}
		channelNumber, err := reservations.CalculateChannelNumber(ctx, s.db, channelType, networkID, serviceID, remoconID)
		if err != nil {
			return nil, err
		}
		jikkyoForce := 0 // Python 版は False を渡しており、Pydantic が 0 に変換する
		channelResp = &channelResponse{
			ID:                channelID,
			DisplayChannelID:  strings.ToLower(channelType) + channelNumber,
			NetworkID:         networkID,
			ServiceID:         serviceID,
			TransportStreamID: &transportStreamID,
			RemoconID:         remoconID,
			ChannelNumber:     channelNumber,
			Type:              channelType,
			Name:              serviceName,
			TerrestrialRegion: nil,
			JikkyoForce:       &jikkyoForce,
			IsSubchannel:      reservations.CalculateIsSubchannel(channelType, serviceID),
			IsRadiochannel:    false,
			IsWatchable:       false,
		}
		// 以降の番組検索はデータベース上のチャンネル情報を使うため、間に合わせの値を入れておく
		channel = &database.Channel{
			ID:                channelResp.ID,
			DisplayChannelID:  channelResp.DisplayChannelID,
			NetworkID:         channelResp.NetworkID,
			ServiceID:         channelResp.ServiceID,
			TransportStreamID: channelResp.TransportStreamID,
			RemoconID:         channelResp.RemoconID,
			ChannelNumber:     channelResp.ChannelNumber,
			Type:              channelResp.Type,
			Name:              channelResp.Name,
		}
	}

	// 録画予約番組のイベント ID
	eventID := reserveData.Eid

	// 録画予約番組のタイトル (基本全角なので半角に変換する)
	title := reservations.FormatString(reserveData.Title)

	// 録画予約番組の番組開始時刻・終了時刻・番組長 (秒)
	startTime := normalizeToJSTDatetime(reserveData.StartTime)
	endTime := startTime.Add(time.Duration(reserveData.DurationSecond) * time.Second)
	duration := float64(reserveData.DurationSecond)

	// ここで ONID・SID・EID が一致する番組をデータベースから取得する
	var program *database.Program
	programKey := reservations.ProgramKey{NetworkID: channel.NetworkID, ServiceID: channel.ServiceID, EventID: eventID}
	if lookup != nil && lookup.usePrograms {
		if found, ok := lookup.programs[programKey]; ok {
			copied := found
			program = &copied
		}
	} else {
		found, err := reservations.GetProgramByKey(ctx, s.db, programKey)
		if err != nil && !errors.Is(err, reservations.ErrNotFound) {
			return nil, err
		}
		program = found
	}

	var programResp *programResponse
	if program != nil {
		// 番組情報をデータベースから取得できた場合でも、番組タイトル・番組開始時刻・番組終了時刻・番組長は
		// EDCB から返される情報の方が正確な可能性があるため (特に追従時など) 、それらの情報を上書きする
		built, err := buildProgramResponse(program)
		if err != nil {
			return nil, err
		}
		built.Title = title
		built.StartTime = database.FormatJSONTime(startTime)
		built.EndTime = database.FormatJSONTime(endTime)
		built.Duration = pydanticFloat64(duration)
		programResp = built
	} else {
		// 取得できなかった場合のみ、上記の限定的な情報を使って間に合わせの番組情報を作成する
		videoType := "映像1080i(1125i)、アスペクト比16:9 パンベクトルなし"
		videoCodec := "mpeg2"
		videoResolution := "1080i"
		programResp = &programResponse{
			ID:                       fmt.Sprintf("NID%d-SID%d-EID%d", channel.NetworkID, channel.ServiceID, eventID),
			ChannelID:                channel.ID,
			NetworkID:                channel.NetworkID,
			ServiceID:                channel.ServiceID,
			EventID:                  eventID,
			Title:                    title,
			Description:              "",
			Detail:                   emptyJSONObject,
			StartTime:                database.FormatJSONTime(startTime),
			EndTime:                  database.FormatJSONTime(endTime),
			Duration:                 pydanticFloat64(duration),
			IsFree:                   true,
			Genres:                   emptyJSONArray,
			VideoType:                &videoType,
			VideoCodec:               &videoCodec,
			VideoResolution:          &videoResolution,
			PrimaryAudioType:         "1/0モード(シングルモノ)",
			PrimaryAudioLanguage:     "日本語",
			PrimaryAudioSamplingRate: "48kHz",
		}
	}

	// 実際に録画可能かどうか: 全編録画可能 / チューナー不足のため部分的にのみ録画可能 / チューナー不足のため全編録画不可能
	recordingAvailability := "Full"
	if reserveData.OverlapMode == 1 {
		recordingAvailability = "Partial"
	} else if reserveData.OverlapMode == 2 {
		recordingAvailability = "Unavailable"
	}

	// 録画予定のファイル名 (EDCB からのレスポンスでは配列になっているが、大半の場合は 1 つしかないため単一の値としている)
	scheduledRecordingFileName := ""
	if len(reserveData.RecFileNameList) > 0 {
		scheduledRecordingFileName = reserveData.RecFileNameList[0]
	}

	// 録画設定
	recordSettings := DecodeEDCBRecSettingData(reserveData.RecSetting)

	// 想定録画ファイルサイズを計算 (失敗した場合は 0 とする)
	estimatedRecordingFileSize := 0
	bitrateKbps := s.getBitrateFromEDCB(ctx, client, networkID, transportStreamID, serviceID)
	estimatedRecordingFileSize = calculateEstimatedFileSize(duration, bitrateKbps, recordSettings.RecordingMode)

	return &reservationResponse{
		ID:                         reserveID,
		Channel:                    channelResp,
		Program:                    programResp,
		IsRecordingInProgress:      isRecordingInProgress,
		RecordingAvailability:      recordingAvailability,
		Comment:                    reserveData.Comment,
		ScheduledRecordingFileName: scheduledRecordingFileName,
		EstimatedRecordingFileSize: estimatedRecordingFileSize,
		RecordSettings:             recordSettings,
	}, nil
}

// getReserveDataList はすべての録画予約の情報を取得する。
//
// 移植元: ReservationsRouter.GetReserveDataList()
func (s *Server) getReserveDataList(w http.ResponseWriter, client edcbClient) ([]reservations.ReserveData, bool) {
	reserveDataList, ok := client.EnumReserve()
	if !ok {
		s.logger.Error("[ReservationsRouter][GetReserveDataList] Failed to get the list of recording reservations.")
		writeError(w, http.StatusInternalServerError, "Failed to get the list of recording reservations")
		return nil, false
	}
	return reserveDataList, true
}

// getRequiredProgramsForReservations は録画予約に必要な番組情報を一括取得する。
//
// 移植元: ReservationsRouter.GetRequiredProgramsForReservations()
func (s *Server) getRequiredProgramsForReservations(ctx context.Context, reserveDataList []reservations.ReserveData) (map[reservations.ProgramKey]database.Program, error) {
	if len(reserveDataList) == 0 {
		return map[reservations.ProgramKey]database.Program{}, nil
	}

	// 録画予約から必要な番組の (ONID, SID, EID) の組み合わせを抽出
	keys := make([]reservations.ProgramKey, 0, len(reserveDataList))
	seen := map[reservations.ProgramKey]bool{}
	for _, reserveData := range reserveDataList {
		key := reservations.ProgramKey{NetworkID: reserveData.Onid, ServiceID: reserveData.Sid, EventID: reserveData.Eid}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return map[reservations.ProgramKey]database.Program{}, nil
	}
	return reservations.GetProgramsByKeys(ctx, s.db, keys)
}

// getReserveData は指定された録画予約の情報を取得する。
//
// 移植元: ReservationsRouter.GetReserveData()
func (s *Server) getReserveData(w http.ResponseWriter, client edcbClient, reservationID int) (reservations.ReserveData, bool) {
	reserveDataList, ok := s.getReserveDataList(w, client)
	if !ok {
		return reservations.ReserveData{}, false
	}
	for _, reserveData := range reserveDataList {
		if reserveData.ReserveID == reservationID {
			return reserveData, true
		}
	}

	// 指定された録画予約が見つからなかった場合はエラーを返す
	s.logger.Error("[ReserveRouter][GetReserveData] Specified reservation_id was not found.", "reservation_id", reservationID)
	writeError(w, http.StatusUnprocessableEntity, "Specified reservation_id was not found")
	return reservations.ReserveData{}, false
}

// getServiceEventInfo は EDCB から指定されたチャンネル情報・番組情報に合致する ServiceEventInfo と EventInfo を取得する。
//
// 移植元: ReservationsRouter.GetServiceEventInfo()
func (s *Server) getServiceEventInfo(
	w http.ResponseWriter,
	client edcbClient,
	channel *database.Channel,
	program *database.Program,
) (*reservations.ServiceEventInfo, *reservations.EventInfo, bool) {
	if channel.TransportStreamID == nil {
		s.logger.Error("[ReservationsRouter][GetServiceEventInfo] transport_stream_id is missing.", "channel_id", channel.ID)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, nil, false
	}

	// EDCB からサービスと当該番組の開始時刻を指定して番組情報を取得
	// (EIT[p/f] と EIT[schedule] の更新タイミング差で番組開始時刻がずれることがあるため、
	//  まずは番組開始時刻近傍で探索し、それで見つからなければ現在時刻近傍の広い範囲で再探索する)
	currentTime := jstNow()
	programStartTime := normalizeToJSTDatetime(program.StartTime)

	type searchRange struct {
		startTime   time.Time
		endTime     time.Time
		searchLabel string
	}
	searchRanges := []searchRange{
		{programStartTime, programStartTime.Add(1 * time.Minute), "program_time_window"},
		{currentTime.Add(-6 * time.Hour), currentTime.Add(6 * time.Hour), "current_time_wide_window"},
	}

	var latestServiceEventInfoList []reservations.ServiceEventInfo
	for _, sr := range searchRanges {
		serviceEventInfoList, ok := client.EnumPgInfoEx([]int64{
			// 絞り込み対象の ONID・TSID・SID に掛けるビットマスク (今回はビットマスクは使用しないので 0)
			0,
			// 絞り込み対象の ONID・TSID・SID ((ONID << 32 | TSID << 16 | SID) の形式)
			int64(channel.NetworkID)<<32 | int64(*channel.TransportStreamID)<<16 | int64(channel.ServiceID),
			// 絞り込み対象の番組開始時刻の最小値・最大値 (FILETIME)
			reservations.DateTimeToFileTime(sr.startTime, 9*60*60),
			reservations.DateTimeToFileTime(sr.endTime, 9*60*60),
		})
		if !ok || len(serviceEventInfoList) == 0 {
			s.logger.Warn(
				"[ReservationsRouter][GetServiceEventInfo] No program information found in search range.",
				"channel_id", channel.ID, "program_id", program.ID, "search_label", sr.searchLabel,
			)
			continue
		}
		latestServiceEventInfoList = serviceEventInfoList

		// まずイベント ID 一致を最優先で探す
		matchedEvent := findEventByEventID(serviceEventInfoList, program.EventID)
		if matchedEvent != nil {
			// 一致したイベントが既に終了していて、かつ現在放送中イベントが別に存在する場合は現在放送中イベントへ切り替える
			// (スポーツ延長などで番組詳細パネルの EID が古いまま残ったケースを救済する)
			onAirEvent := findOnAirEvent(serviceEventInfoList, currentTime)
			if matchedEvent.Event.StartTime != nil && matchedEvent.Event.DurationSec != nil &&
				onAirEvent != nil && onAirEvent.Event.Eid != matchedEvent.Event.Eid {
				duration := *matchedEvent.Event.DurationSec
				if duration < 1 {
					duration = 1
				}
				matchedEventEndTime := matchedEvent.Event.StartTime.Add(time.Duration(duration) * time.Second)
				if !currentTime.Before(matchedEventEndTime) {
					s.logger.Warn(
						"[ReservationsRouter][GetServiceEventInfo] Matched event is already ended, switched to on-air event.",
						"channel_id", channel.ID, "program_id", program.ID,
						"requested_event_id", program.EventID, "matched_event_id", matchedEvent.Event.Eid,
						"resolved_event_id", onAirEvent.Event.Eid,
					)
					return &onAirEvent.Service, &onAirEvent.Event, true
				}
			}
			return &matchedEvent.Service, &matchedEvent.Event, true
		}
	}

	// イベント ID が一致しないとき、番組が既に終了扱いなら現在放送中イベントへフェイルオーバーする
	// (長時間延長などで EPG の切り替わりが遅延したケースでは、指定 EID が実態とずれていることがある)
	programDuration := int(program.Duration)
	if programDuration < 1 {
		programDuration = 1
	}
	requestedProgramEndTime := programStartTime.Add(time.Duration(programDuration) * time.Second)
	if len(latestServiceEventInfoList) > 0 && !currentTime.Before(requestedProgramEndTime) {
		onAirEvent := findOnAirEvent(latestServiceEventInfoList, currentTime)
		if onAirEvent != nil {
			s.logger.Warn(
				"[ReservationsRouter][GetServiceEventInfo] Falling back to on-air event because event_id mismatch.",
				"channel_id", channel.ID, "program_id", program.ID,
				"requested_event_id", program.EventID, "resolved_event_id", onAirEvent.Event.Eid,
			)
			return &onAirEvent.Service, &onAirEvent.Event, true
		}
	}

	// 最終的に番組情報が取得できなかった場合はエラーを返す
	s.logger.Error(
		"[ReservationsRouter][GetServiceEventInfo] Failed to resolve program information from EDCB.",
		"channel_id", channel.ID, "program_id", program.ID, "requested_event_id", program.EventID,
	)
	writeError(w, http.StatusInternalServerError, "Failed to resolve program information from EDCB")
	return nil, nil, false
}

// eventMatch は ServiceEventInfo と EventInfo の組。
type eventMatch struct {
	Service reservations.ServiceEventInfo
	Event   reservations.EventInfo
}

// findEventByEventID は ServiceEventInfo リストから、イベント ID が一致する EventInfo を探す。
func findEventByEventID(serviceEventInfoList []reservations.ServiceEventInfo, eventID int) *eventMatch {
	for _, serviceEventInfo := range serviceEventInfoList {
		for _, eventInfo := range serviceEventInfo.EventList {
			if eventInfo.Eid == eventID {
				return &eventMatch{Service: serviceEventInfo, Event: eventInfo}
			}
		}
	}
	return nil
}

// findOnAirEvent は ServiceEventInfo リストから、現在放送中の EventInfo を探す。
func findOnAirEvent(serviceEventInfoList []reservations.ServiceEventInfo, currentTime time.Time) *eventMatch {
	for _, serviceEventInfo := range serviceEventInfoList {
		for _, eventInfo := range serviceEventInfo.EventList {
			if eventInfo.StartTime == nil || eventInfo.DurationSec == nil {
				continue
			}
			duration := *eventInfo.DurationSec
			if duration < 1 {
				duration = 1
			}
			eventEndTime := eventInfo.StartTime.Add(time.Duration(duration) * time.Second)
			if !eventInfo.StartTime.After(currentTime) && currentTime.Before(eventEndTime) {
				return &eventMatch{Service: serviceEventInfo, Event: eventInfo}
			}
		}
	}
	return nil
}

// shouldCheckRecordingInProgress は録画中判定のために EDCB へ追加問い合わせを行うべきかを判定する。
//
// 移植元: ReservationsRouter.ShouldCheckRecordingInProgress()
func shouldCheckRecordingInProgress(reserveData reservations.ReserveData) bool {
	// 無効予約・視聴予約は録画ファイルパスが存在しないため、判定 API を呼ばない
	recMode := reserveData.RecSetting.RecMode
	if recMode >= 5 || recMode == 4 {
		return false
	}

	// 録画中判定を行う時間範囲 (現在時刻の 2 時間前〜2 時間後)
	currentTime := jstNow()
	recordingCheckStart := currentTime.Add(-2 * time.Hour)
	recordingCheckEnd := currentTime.Add(2 * time.Hour)

	reserveStartTime := normalizeToJSTDatetime(reserveData.StartTime)
	reserveEndTime := reserveStartTime.Add(time.Duration(reserveData.DurationSecond) * time.Second)

	return !reserveStartTime.After(recordingCheckEnd) && !reserveEndTime.Before(recordingCheckStart)
}

// getIsRecordingInProgress は指定された予約が現在録画中かどうかを判定する。
//
// 移植元: ReservationsRouter.GetIsRecordingInProgress()
func getIsRecordingInProgress(reserveData reservations.ReserveData, client edcbClient) bool {
	// 録画中判定が不要な予約では追加問い合わせを行わない
	if !shouldCheckRecordingInProgress(reserveData) {
		return false
	}

	// sendGetRecFilePath() で「録画中かつ視聴予約でない予約の録画ファイルパス」が返ってくる場合は True、それ以外は False
	return client.GetRecFilePath(reserveData.ReserveID) != nil
}

// hasSameReservation は同一 ONID/TSID/SID/EID の録画予約が既に存在するかを判定する。
//
// 移植元: ReservationsRouter.AddReservationAPI() 内の HasSameReservation()
func hasSameReservation(reserveDataList []reservations.ReserveData, networkID int, transportStreamID int, serviceID int, eventID int) bool {
	for _, reserveData := range reserveDataList {
		if reserveData.Onid == networkID && reserveData.Tsid == transportStreamID &&
			reserveData.Sid == serviceID && reserveData.Eid == eventID {
			return true
		}
	}
	return false
}

// normalizeToJSTDatetime は datetime を JST aware な time.Time に正規化する。
//
// 移植元: app.utils.NormalizeToJSTDatetime()
// EDCB から読み取った時刻は既に JST だが、DB 由来の naive な時刻も同じ扱いに揃える。
func normalizeToJSTDatetime(value time.Time) time.Time {
	return value.In(constants.JST)
}

// isFinite は float64 が有限値かどうかを返す (Python の math.isfinite() 相当) 。
func isFinite(value float64) bool {
	return !math.IsInf(value, 0) && !math.IsNaN(value)
}

// isNotFound は reservations パッケージの「レコードなし」エラーかどうかを返す。
func isNotFound(err error) bool {
	return errors.Is(err, reservations.ErrNotFound)
}

// maxInt は 2 つの int のうち大きい方を返す。
func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

// parseDatetimeString は ISO8601 互換の日時文字列を JST の time.Time に変換する。
//
// 移植元: app.utils.ParseDatetimeStringToJST() (Pydantic の datetime バリデーション相当)
func parseDatetimeString(value string) (time.Time, error) {
	return database.ParseDBTime(value)
}
