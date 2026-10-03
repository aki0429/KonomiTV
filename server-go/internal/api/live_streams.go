package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// liveStreamStatusResponse はライブストリームの状態 (server/app/schemas.py の LiveStreamStatus 相当) 。
type liveStreamStatusResponse struct {
	Status      string          `json:"status"`
	Detail      string          `json:"detail"`
	StartedAt   pydanticFloat64 `json:"started_at"`
	UpdatedAt   pydanticFloat64 `json:"updated_at"`
	ClientCount int             `json:"client_count"`
}

// toLiveStreamStatusResponse は stream パッケージの状態を API レスポンスへ変換する。
func toLiveStreamStatusResponse(status stream.LiveStreamStatus) liveStreamStatusResponse {
	return liveStreamStatusResponse{
		Status:      status.Status,
		Detail:      status.Detail,
		StartedAt:   pydanticFloat64(status.StartedAt),
		UpdatedAt:   pydanticFloat64(status.UpdatedAt),
		ClientCount: status.ClientCount,
	}
}

// liveStreamsStatusesResponse はステータスごとに分類されたライブストリームの状態。
type liveStreamsStatusesResponse struct {
	Restart map[string]liveStreamStatusResponse `json:"Restart"`
	Idling  map[string]liveStreamStatusResponse `json:"Idling"`
	ONAir   map[string]liveStreamStatusResponse `json:"ONAir"`
	Standby map[string]liveStreamStatusResponse `json:"Standby"`
	Offline map[string]liveStreamStatusResponse `json:"Offline"`
}

// handleLiveStreams はライブストリーム一覧 API (GET /api/streams/live) を処理する。
// すべてのライブストリームの状態を Offline・Standby・ONAir・Idling・Restart の各ステータスごとに取得する。
func (s *Server) handleLiveStreams(w http.ResponseWriter, r *http.Request) {
	result := liveStreamsStatusesResponse{
		Restart: map[string]liveStreamStatusResponse{},
		Idling:  map[string]liveStreamStatusResponse{},
		ONAir:   map[string]liveStreamStatusResponse{},
		Standby: map[string]liveStreamStatusResponse{},
		Offline: map[string]liveStreamStatusResponse{},
	}
	// 逆順になっているのは、デバッグ時に全体の大半を占める Offline なストリームが邪魔なため (Python 版と同じ) 。
	for _, liveStream := range s.liveStreams.GetAllLiveStreams() {
		status := toLiveStreamStatusResponse(liveStream.GetStatus())
		switch status.Status {
		case "Restart":
			result.Restart[liveStream.ID] = status
		case "Idling":
			result.Idling[liveStream.ID] = status
		case "ONAir":
			result.ONAir[liveStream.ID] = status
		case "Standby":
			result.Standby[liveStream.ID] = status
		default:
			result.Offline[liveStream.ID] = status
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// validateLiveStreamChannelID はチャンネル ID のバリデーションを行う。
// 移植元: LiveStreamsRouter.ValidateChannelID()
func (s *Server) validateLiveStreamChannelID(ctx context.Context, displayChannelID string) (int, string) {
	// IPTV ページからテレビ視聴 UI に登録された IPTV の疑似チャンネルの場合。
	if iptv.IsDisplayChannelID(displayChannelID) {
		if s.iptv.GetChannelByDisplayChannelID(displayChannelID) == nil {
			return http.StatusUnprocessableEntity, "Specified display_channel_id was not found"
		}
		return 0, ""
	}

	// チャンネル ID が存在するか確認する。
	if _, err := database.GetChannelByIDOrDisplayChannelID(ctx, s.db, displayChannelID); err != nil {
		return http.StatusUnprocessableEntity, "Specified display_channel_id was not found"
	}
	return 0, ""
}

// validateLiveStreamQuality は映像の品質のバリデーションを行い、ベース画質とエンコードオプションに分解する。
// 移植元: LiveStreamsRouter.ValidateQuality()
func (s *Server) validateLiveStreamQuality(ctx context.Context, displayChannelID string, quality string) (stream.StreamQualityWithOptions, int, string) {
	// ラジオチャンネルには MPEG-2 映像がなく、mpeg2toh264 では再生できないため受け付けない。
	if quality == stream.OriginalQuality && !iptv.IsDisplayChannelID(displayChannelID) {
		channel, err := database.GetChannelByIDOrDisplayChannelID(ctx, s.db, displayChannelID)
		if err == nil && channel.IsRadiochannel {
			return stream.StreamQualityWithOptions{}, http.StatusUnprocessableEntity, "Original quality is not available for radio channels"
		}
	}

	// 指定されたエンコード済み品質が存在するか確認する。
	streamQuality, ok := stream.SplitQualityAndEncodingOptions(quality, s.config.General.Encoder)
	if !ok {
		return stream.StreamQualityWithOptions{}, http.StatusUnprocessableEntity, "Specified quality was not found"
	}
	return streamQuality, 0, ""
}

// liveStreamRequest はライブストリーム API の共通のバリデーション結果。
type liveStreamRequest struct {
	// DisplayChannelID はチャンネル ID。
	DisplayChannelID string
	// StreamQuality はベース画質とエンコードオプション。
	StreamQuality stream.StreamQualityWithOptions
}

// parseLiveStreamRequest はパスパラメーターを検証してライブストリームの指定を返す。
func (s *Server) parseLiveStreamRequest(r *http.Request) (liveStreamRequest, int, string) {
	displayChannelID := r.PathValue("display_channel_id")
	quality := r.PathValue("quality")
	if status, detail := s.validateLiveStreamChannelID(r.Context(), displayChannelID); status != 0 {
		return liveStreamRequest{}, status, detail
	}
	streamQuality, status, detail := s.validateLiveStreamQuality(r.Context(), displayChannelID, quality)
	if status != 0 {
		return liveStreamRequest{}, status, detail
	}
	return liveStreamRequest{DisplayChannelID: displayChannelID, StreamQuality: streamQuality}, 0, ""
}

// handleLiveStream はライブストリーム API (GET /api/streams/live/{display_channel_id}/{quality}) を処理する。
// ライブストリームの状態を取得する (ステータスを取得したいだけなので、接続はしない) 。
func (s *Server) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	request, status, detail := s.parseLiveStreamRequest(r)
	if status != 0 {
		writeError(w, status, detail)
		return
	}
	liveStream := s.liveStreams.GetLiveStream(request.DisplayChannelID, request.StreamQuality.Quality, request.StreamQuality.EncodingOptions)
	writeJSON(w, http.StatusOK, toLiveStreamStatusResponse(liveStream.GetStatus()))
}

// handleLiveStreamEvents はライブストリーム イベント API
// (GET /api/streams/live/{display_channel_id}/{quality}/events) を処理する。
// ライブストリームのイベントを Server-Sent Events で随時配信する。
func (s *Server) handleLiveStreamEvents(w http.ResponseWriter, r *http.Request) {
	request, status, detail := s.parseLiveStreamRequest(r)
	if status != 0 {
		writeError(w, status, detail)
		return
	}
	liveStream := s.liveStreams.GetLiveStream(request.DisplayChannelID, request.StreamQuality.Quality, request.StreamQuality.EncodingOptions)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// 初期値
	previousStatus := liveStream.GetStatus()
	// 取得できたクライアント数は同じチャンネル+同じ画質のものなので、同じチャンネルのすべての画質の合計で上書きする。
	previousStatus.ClientCount = s.liveStreams.GetViewerCount(request.DisplayChannelID)
	writeSSEEvent(w, "initial_update", toLiveStreamStatusResponse(previousStatus))
	flusher.Flush()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	// sse_starlette と同じく 15 秒間隔で ping を送信する。
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-pingTicker.C:
			_, _ = io.WriteString(w, ": ping - "+time.Now().Format(time.RFC3339Nano)+"\r\n\r\n")
			flusher.Flush()
		case <-ticker.C:
		}

		// 現在のライブストリームのステータスを取得する。
		status := liveStream.GetStatus()
		status.ClientCount = s.liveStreams.GetViewerCount(request.DisplayChannelID)

		// 以前の結果と異なっている場合のみレスポンスを返す。
		if status == previousStatus {
			continue
		}
		event := "clients_update"
		if previousStatus.Status != status.Status {
			event = "status_update"
		} else if previousStatus.Detail != status.Detail {
			event = "detail_update"
		}
		writeSSEEvent(w, event, toLiveStreamStatusResponse(status))
		flusher.Flush()
		previousStatus = status
	}
}

// writeSSEEvent は Server-Sent Events のイベントを書き出す。
func writeSSEEvent(w io.Writer, event string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "event: "+event+"\r\n")
	_, _ = io.WriteString(w, "data: "+string(data)+"\r\n\r\n")
}

// handleLiveStreamPSIArchivedData はライブ PSI/SI アーカイブデータストリーミング API
// (GET /api/streams/live/{display_channel_id}/{quality}/psi-archived-data) を処理する。
func (s *Server) handleLiveStreamPSIArchivedData(w http.ResponseWriter, r *http.Request) {
	request, status, detail := s.parseLiveStreamRequest(r)
	if status != 0 {
		writeError(w, status, detail)
		return
	}
	liveStream := s.liveStreams.GetLiveStream(request.DisplayChannelID, request.StreamQuality.Quality, request.StreamQuality.EncodingOptions)

	// PSI/SI データアーカイバーがまだ初期化されていない場合は、起動するまで最大 10 秒待つ。
	// (LivePSIDataArchiver はエンコードタスクが起動次第自動的に初期化されるので、ここでは待つだけ)
	var archiver *stream.LivePSIDataArchiver
	for attempt := 0; attempt < 20; attempt++ {
		if archiver = liveStream.PSIDataArchiver(); archiver != nil {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if archiver == nil {
		s.logger.Error("PSI/SI Data Archiver is not running.", "live_stream_id", liveStream.ID)
		writeError(w, http.StatusInternalServerError, "PSI/SI Data Archiver is not running")
		return
	}

	chunks, err := archiver.StartPSIArchivedData(r.Context())
	if err != nil {
		s.logger.Error("Failed to start the PSI/SI Data Archiver. "+err.Error(), "live_stream_id", liveStream.ID)
		writeError(w, http.StatusInternalServerError, "PSI/SI Data Archiver is not running")
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	for chunk := range chunks {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// handleLiveStreamMPEGTS はライブ MPEG-TS ストリーム API
// (GET /api/streams/live/{display_channel_id}/{quality}/mpegts) を処理する。
// 同じチャンネル ID・同じ画質のライブストリームが Offline 状態のときは、新たにエンコードタスクを立ち上げて、
// ONAir 状態になるのを待機してからストリームデータを配信する。
func (s *Server) handleLiveStreamMPEGTS(w http.ResponseWriter, r *http.Request) {
	request, status, detail := s.parseLiveStreamRequest(r)
	if status != 0 {
		writeError(w, status, detail)
		return
	}

	// ライブストリームに接続し、ライブストリームクライアントを取得する。
	// (接続時に Offline だった場合は自動的にエンコードタスクが起動される)
	liveStream := s.liveStreams.GetLiveStream(request.DisplayChannelID, request.StreamQuality.Quality, request.StreamQuality.EncodingOptions)
	client := s.liveStreams.Connect(liveStream, "mpegts")

	// 必ず接続を切断する (さもなければ誰も見てないのに視聴中扱いでエンコードタスクが実行され続けてしまう) 。
	defer func() {
		liveStream.Disconnect(client)
		s.logger.Debug(liveStream.LogPrefix()+" Request is disconnected.", "live_stream_id", liveStream.ID)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)

	for {
		select {
		case <-r.Context().Done():
			// リクエストがキャンセル (切断) された場合は、ライブストリームへの接続を切断してループを終了する。
			return
		case data, ok := <-client.Queue():
			if !ok {
				// エンコードタスクが終了した場合は接続を切断してループを終了する。
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
