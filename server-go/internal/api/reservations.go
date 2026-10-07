package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/routers/ReservationsRouter.py の 5 つのエンドポイントを移植したもの。
//
// - GET    /api/recording/reservations
// - POST   /api/recording/reservations
// - GET    /api/recording/reservations/{reservation_id}
// - PUT    /api/recording/reservations/{reservation_id}
// - DELETE /api/recording/reservations/{reservation_id}

// registerReservationRoutes は録画予約系のルートを mux に登録する。
// 移植元: server/app/routers/ReservationsRouter.py
func (s *Server) registerReservationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/recording/reservations", s.handleReservations)
	mux.HandleFunc("POST /api/recording/reservations", s.handleReservationAdd)
	mux.HandleFunc("GET /api/recording/reservations/{reservation_id}", s.handleReservation)
	mux.HandleFunc("PUT /api/recording/reservations/{reservation_id}", s.handleReservationUpdate)
	mux.HandleFunc("DELETE /api/recording/reservations/{reservation_id}", s.handleReservationDelete)
}

// handleReservations は録画予約情報一覧 API を処理する。
// 移植元: ReservationsRouter.ReservationsAPI()
func (s *Server) handleReservations(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}

	// EDCB から現在のすべての録画予約の情報を取得
	reserveDataList, ok := client.EnumReserve()
	if !ok {
		// 取得できなかった場合は空のリストを返す
		writeJSON(w, http.StatusOK, reservationsResponse{Total: 0, Reservations: []reservationResponse{}})
		return
	}

	// 録画中判定が必要な予約のみ EDCB へ問い合わせる
	isRecordingInProgressByReserveID := map[int]bool{}
	for _, reserveData := range reserveDataList {
		isRecordingInProgressByReserveID[reserveData.ReserveID] = getIsRecordingInProgress(reserveData, client)
	}

	// 高速化のため、あらかじめ全てのチャンネル情報を取得しておく
	channels, err := reservations.ListAllChannels(r.Context(), s.db)
	if err != nil {
		s.logger.Error("[ReservationsRouter][ReservationsAPI] Failed to get channels.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 高速化のため、録画予約に必要な番組情報を一括取得しておく
	programs, err := s.getRequiredProgramsForReservations(r.Context(), reserveDataList)
	if err != nil {
		s.logger.Error("[ReservationsRouter][ReservationsAPI] Failed to get programs.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	lookup := &reservationLookup{channels: channels, useChannels: true, programs: programs, usePrograms: true}

	// EDCB の ReserveData を schemas.Reservation 互換のレスポンスに変換
	type decodedReservation struct {
		response  reservationResponse
		startTime time.Time
	}
	decoded := make([]decodedReservation, 0, len(reserveDataList))
	for _, reserveData := range reserveDataList {
		response, err := s.decodeEDCBReserveData(
			r.Context(), client, reserveData, lookup,
			isRecordingInProgressByReserveID[reserveData.ReserveID],
		)
		if err != nil {
			s.logger.Error("[ReservationsRouter][ReservationsAPI] Failed to decode reserve data.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		startTime := normalizeToJSTDatetime(reserveData.StartTime)
		decoded = append(decoded, decodedReservation{response: *response, startTime: startTime})
	}

	// 録画予約番組の番組開始時刻でソート (Python の list.sort と同じく安定ソート)
	sort.SliceStable(decoded, func(i int, j int) bool {
		return decoded[i].startTime.Before(decoded[j].startTime)
	})
	responses := make([]reservationResponse, 0, len(decoded))
	for _, item := range decoded {
		responses = append(responses, item.response)
	}

	writeJSON(w, http.StatusOK, reservationsResponse{
		Total:        len(reserveDataList),
		Reservations: responses,
	})
}

// handleReservationAdd は録画予約追加 API を処理する。
// 移植元: ReservationsRouter.AddReservationAPI()
func (s *Server) handleReservationAdd(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}

	var request reservationAddRequest
	if !decodeJSONBody(r, &request) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	if request.ProgramID == nil || request.RecordSettings == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	recordSettings, err := request.RecordSettings.toRecordSettings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	programID := *request.ProgramID

	// 録画予約対象のチャンネル情報・番組情報 (EDCB に投入する値)
	var (
		channel          *database.Channel
		title            string
		startTime        time.Time
		durationSecond   int
		stationName      string
		eventID          int
		requestedEventID int
		reserveDataList  []reservations.ReserveData
	)

	// 指定された番組 ID の番組が DB にある場合は、従来通り EDCB の EPG 情報で補正する
	program, programErr := reservations.GetProgramByID(r.Context(), s.db, programID)
	if programErr != nil && !isNotFound(programErr) {
		s.logger.Error("[ReservationsRouter][AddReservationAPI] Failed to get program.", "error", programErr)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	if program != nil {
		// 指定された番組 ID に関連付けられたチャンネルがあるかを確認
		found, err := reservations.GetChannelByID(r.Context(), s.db, program.ChannelID)
		if isNotFound(err) {
			s.logger.Error("[ReserveRouter][AddReserveAPI] Specified channel was not found.", "channel_id", program.ChannelID)
			writeError(w, http.StatusUnprocessableEntity, "Specified channel was not found")
			return
		}
		if err != nil {
			s.logger.Error("[ReservationsRouter][AddReservationAPI] Failed to get channel.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		channel = found

		// EDCB に予約を投入するには TSID が必須
		if channel.TransportStreamID == nil {
			s.logger.Error("[ReservationsRouter][AddReserveAPI] Specified channel does not have transport_stream_id.", "channel_id", channel.ID)
			writeError(w, http.StatusUnprocessableEntity, "Specified channel does not have transport_stream_id")
			return
		}

		// すでに同じ番組 ID の録画予約が存在するかを確認
		reserveDataList, ok = s.getReserveDataList(w, client)
		if !ok {
			return
		}
		if hasSameReservation(reserveDataList, channel.NetworkID, *channel.TransportStreamID, channel.ServiceID, program.EventID) {
			s.logger.Error("[ReservationsRouter][AddReserveAPI] The same program_id is already reserved.", "program_id", programID)
			writeError(w, http.StatusUnprocessableEntity, "The same program_id is already reserved")
			return
		}

		// EDCB から録画予約対象の番組に一致する ServiceEventInfo を取得
		serviceEventInfo, eventInfo, ok := s.getServiceEventInfo(w, client, channel, program)
		if !ok {
			return
		}

		// ReserveData に設定するチャンネル情報・番組情報を取得
		// (放送時間未定運用などでごく稀に取得できないことも考えられるため、その場合は KonomiTV 側が持っている情報にフォールバックする)
		title = program.Title
		startTime = normalizeToJSTDatetime(program.StartTime)
		durationSecond = int(program.Duration)
		if durationSecond < 1 {
			durationSecond = 1
		}
		stationName = serviceEventInfo.ServiceInfo.ServiceName
		eventID = program.EventID
		requestedEventID = program.EventID
		if eventInfo.ShortInfo != nil {
			// Python 版は 'event_name' キーの有無で判定しているが、EDCB のプロトコル上
			// short_info がある場合は event_name も必ず含まれるため、ポインタの有無で判定する
			title = eventInfo.ShortInfo.EventName
		}
		if eventInfo.StartTime != nil {
			startTime = normalizeToJSTDatetime(*eventInfo.StartTime)
		}
		if eventInfo.DurationSec != nil {
			durationSecond = *eventInfo.DurationSec
			if durationSecond < 1 {
				durationSecond = 1
			}
		}
		eventID = eventInfo.Eid
	} else {
		// DB に未反映の EIT[p/f] 由来番組は、クライアントから渡された最小情報で ReserveData を作る
		payload := request.Program
		if payload == nil {
			s.logger.Error("[ReserveRouter][AddReserveAPI] Specified program was not found.", "program_id", programID)
			writeError(w, http.StatusUnprocessableEntity, "Specified program was not found")
			return
		}
		if payload.ID == nil || payload.ChannelID == nil || payload.NetworkID == nil ||
			payload.ServiceID == nil || payload.EventID == nil || payload.Title == nil ||
			payload.StartTime == nil || payload.Duration == nil {
			writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
			return
		}

		// TSID は IProgram には含まれないため、必ず DB の Channel から取得する
		found, err := reservations.GetChannelByID(r.Context(), s.db, *payload.ChannelID)
		if isNotFound(err) {
			s.logger.Error("[ReserveRouter][AddReserveAPI] Specified channel was not found.", "channel_id", *payload.ChannelID)
			writeError(w, http.StatusUnprocessableEntity, "Specified channel was not found")
			return
		}
		if err != nil {
			s.logger.Error("[ReservationsRouter][AddReservationAPI] Failed to get channel.", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		channel = found

		// EDCB に予約を投入するには TSID が必須
		if channel.TransportStreamID == nil {
			s.logger.Error("[ReservationsRouter][AddReserveAPI] Specified channel does not have transport_stream_id.", "channel_id", channel.ID)
			writeError(w, http.StatusUnprocessableEntity, "Specified channel does not have transport_stream_id")
			return
		}

		// クライアントから送られたペイロードと DB 上の Channel が食い違う場合は、別チャンネルの番組を誤予約するため拒否する
		if *payload.ID != programID || *payload.ChannelID != channel.ID ||
			*payload.NetworkID != channel.NetworkID || *payload.ServiceID != channel.ServiceID {
			s.logger.Error(
				"[ReservationsRouter][AddReserveAPI] Program payload does not match channel.",
				"program_id", programID, "payload_channel_id", *payload.ChannelID, "channel_id", channel.ID,
				"payload_network_id", *payload.NetworkID, "channel_network_id", channel.NetworkID,
				"payload_service_id", *payload.ServiceID, "channel_service_id", channel.ServiceID,
			)
			writeError(w, http.StatusUnprocessableEntity, "Program payload does not match channel")
			return
		}

		// duration が未定の EIT[p/f] は EDCB に投入する録画時間を決められないため、サーバー側でも拒否する
		if !isFinite(*payload.Duration) || *payload.Duration <= 0 {
			s.logger.Error(
				"[ReservationsRouter][AddReserveAPI] Specified program duration is unknown.",
				"program_id", programID, "duration", *payload.Duration,
			)
			writeError(w, http.StatusUnprocessableEntity, "Specified program duration is unknown")
			return
		}

		payloadStartTime, err := parseDatetimeString(*payload.StartTime)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
			return
		}
		title = *payload.Title
		startTime = payloadStartTime
		durationSecond = int(*payload.Duration)
		stationName = channel.Name
		eventID = *payload.EventID
		requestedEventID = *payload.EventID
		reserveEndTime := startTime.Add(time.Duration(durationSecond) * time.Second)

		// 放送終了後の ReserveData は EpgTimerSrv 側で拒否されるため、送る前に明示的に止める
		if !jstNow().Before(reserveEndTime) {
			s.logger.Error(
				"[ReservationsRouter][AddReserveAPI] Specified program has already ended.",
				"program_id", programID, "event_id", eventID,
				"start_time", startTime.Format(time.RFC3339), "duration_second", durationSecond,
			)
			writeError(w, http.StatusUnprocessableEntity, "Specified program has already ended")
			return
		}

		// すでに同じ ONID/TSID/SID/EID の録画予約が存在するかを確認
		reserveDataList, ok = s.getReserveDataList(w, client)
		if !ok {
			return
		}
		if hasSameReservation(reserveDataList, channel.NetworkID, *channel.TransportStreamID, channel.ServiceID, eventID) {
			s.logger.Error("[ReservationsRouter][AddReserveAPI] The same program_id is already reserved.", "program_id", programID)
			writeError(w, http.StatusUnprocessableEntity, "The same program_id is already reserved")
			return
		}
	}

	// 実際に予約するイベント ID がリクエストされたイベント ID と異なる場合は重複チェックをやり直す
	if eventID != requestedEventID {
		if hasSameReservation(reserveDataList, channel.NetworkID, *channel.TransportStreamID, channel.ServiceID, eventID) {
			s.logger.Error(
				"[ReservationsRouter][AddReserveAPI] The fallback event is already reserved.",
				"program_id", programID, "requested_event_id", requestedEventID, "resolved_event_id", eventID,
			)
			writeError(w, http.StatusUnprocessableEntity, "The fallback event is already reserved")
			return
		}
	}

	// EDCB の ReserveData を組み立てる
	// (reserve_id / overlap_mode / rec_file_name_list は EDCB 側で自動設定されるため省略している)
	addReserveData := reservations.ReserveData{
		Title:          title,
		StartTime:      startTime,
		StartTimeEPG:   startTime,
		DurationSecond: durationSecond,
		StationName:    stationName,
		Onid:           channel.NetworkID,
		Tsid:           *channel.TransportStreamID,
		Sid:            channel.ServiceID,
		Eid:            eventID,
		Comment:        "", // 単発予約の場合は空文字列で問題ないはず
		RecSetting:     EncodeEDCBRecSettingData(recordSettings),
	}

	// EDCB に録画予約を追加するように指示
	if client.AddReserve([]reservations.ReserveData{addReserveData}) {
		writeJSON(w, http.StatusCreated, nil)
		return
	}

	// EDCB が「現在時刻で既に放送終了扱い」と判断した可能性がある場合のみ、時刻補正して 1 回だけ再試行する
	currentTime := jstNow()
	reserveEndTime := startTime.Add(time.Duration(maxInt(durationSecond, 1)) * time.Second)
	if !currentTime.Before(reserveEndTime) {
		retryDurationSecond := maxInt(int(currentTime.Sub(startTime).Seconds())+120, 120)
		retryReserveData := addReserveData
		retryReserveData.DurationSecond = retryDurationSecond
		s.logger.Warn(
			"[ReservationsRouter][AddReserveAPI] Retrying with adjusted duration because reservation window looks expired.",
			"program_id", programID, "event_id", eventID,
			"original_duration_second", durationSecond, "retry_duration_second", retryDurationSecond,
		)

		// 再試行前に重複予約を再確認する
		latestReserveDataList, ok := s.getReserveDataList(w, client)
		if !ok {
			return
		}
		if hasSameReservation(latestReserveDataList, channel.NetworkID, *channel.TransportStreamID, channel.ServiceID, eventID) {
			s.logger.Error(
				"[ReservationsRouter][AddReserveAPI] Reservation already exists before retry.",
				"program_id", programID, "event_id", eventID,
			)
			writeError(w, http.StatusUnprocessableEntity, "The same program_id is already reserved")
			return
		}

		if client.AddReserve([]reservations.ReserveData{retryReserveData}) {
			s.logger.Info(
				"[ReservationsRouter][AddReserveAPI] Added reservation with adjusted duration fallback.",
				"program_id", programID, "event_id", eventID, "retry_duration_second", retryDurationSecond,
			)
			writeJSON(w, http.StatusCreated, nil)
			return
		}
	}

	// それでも失敗した場合は、重複・イベント不整合・通信系を判別しやすいログを残したうえでエラーを返す
	latestReserveDataList, ok := s.getReserveDataList(w, client)
	if !ok {
		return
	}
	if hasSameReservation(latestReserveDataList, channel.NetworkID, *channel.TransportStreamID, channel.ServiceID, eventID) {
		s.logger.Error(
			"[ReservationsRouter][AddReserveAPI] Reservation was added by another process concurrently.",
			"program_id", programID, "event_id", eventID,
		)
		writeError(w, http.StatusUnprocessableEntity, "The same program_id is already reserved")
		return
	}

	s.logger.Error(
		"[ReservationsRouter][AddReserveAPI] Failed to add a recording reservation.",
		"program_id", programID, "requested_event_id", requestedEventID, "resolved_event_id", eventID,
		"start_time", startTime.Format(time.RFC3339), "duration_second", durationSecond,
	)
	writeError(w, http.StatusInternalServerError, "Failed to add a recording reservation due to EDCB rejection or event mismatch")
}

// handleReservation は録画予約情報取得 API を処理する。
// 移植元: ReservationsRouter.ReservationAPI()
func (s *Server) handleReservation(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	reservationID, ok := parsePathInt(w, r, "reservation_id")
	if !ok {
		return
	}
	reserveData, ok := s.getReserveData(w, client, int(reservationID))
	if !ok {
		return
	}

	// EDCB の ReserveData を schemas.Reservation 互換のレスポンスに変換して返す
	response, err := s.decodeEDCBReserveData(
		r.Context(), client, reserveData, nil,
		getIsRecordingInProgress(reserveData, client),
	)
	if err != nil {
		s.logger.Error("[ReservationsRouter][ReservationAPI] Failed to decode reserve data.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleReservationUpdate は録画予約設定更新 API を処理する。
// 移植元: ReservationsRouter.UpdateReservationAPI()
func (s *Server) handleReservationUpdate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	reservationID, ok := parsePathInt(w, r, "reservation_id")
	if !ok {
		return
	}

	var request reservationUpdateRequest
	if !decodeJSONBody(r, &request) || request.RecordSettings == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	recordSettings, err := request.RecordSettings.toRecordSettings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	reserveData, ok := s.getReserveData(w, client, int(reservationID))
	if !ok {
		return
	}

	// 現在の録画予約の ReserveData に新しい録画設定を上書きマージする形で EDCB に送信する
	reserveData.RecSetting = EncodeEDCBRecSettingData(recordSettings)

	// EDCB に指定された録画予約を更新するように指示
	if !client.ChgReserve([]reservations.ReserveData{reserveData}) {
		s.logger.Error("[ReservationsRouter][UpdateReserveAPI] Failed to update the specified recording reservation.")
		writeError(w, http.StatusInternalServerError, "Failed to update the specified recording reservation")
		return
	}

	// 更新された録画予約の情報を schemas.Reservation 互換のレスポンスに変換して返す
	updatedReserveData, ok := s.getReserveData(w, client, reserveData.ReserveID)
	if !ok {
		return
	}
	response, err := s.decodeEDCBReserveData(
		r.Context(), client, updatedReserveData, nil,
		getIsRecordingInProgress(updatedReserveData, client),
	)
	if err != nil {
		s.logger.Error("[ReservationsRouter][UpdateReservationAPI] Failed to decode reserve data.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleReservationDelete は録画予約削除 API を処理する。
// 移植元: ReservationsRouter.DeleteReservationAPI()
func (s *Server) handleReservationDelete(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	reservationID, ok := parsePathInt(w, r, "reservation_id")
	if !ok {
		return
	}
	reserveData, ok := s.getReserveData(w, client, int(reservationID))
	if !ok {
		return
	}

	// EDCB に指定された録画予約を削除するように指示
	if !client.DelReserve([]int{reserveData.ReserveID}) {
		s.logger.Error("[ReservationsRouter][DeleteReserveAPI] Failed to delete the specified recording reservation.")
		writeError(w, http.StatusInternalServerError, "Failed to delete the specified recording reservation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
