package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// SetVideoSegmentEncoderFactory は録画視聴セッションのエンコーダー (VideoEncodingTask 相当) の工場を登録する。
// 未登録の間、GET /api/streams/video/{video_id}/{quality}/segment は 500 を返す。
func (s *Server) SetVideoSegmentEncoderFactory(factory videostream.SegmentEncoderFactory) {
	s.videoStreams.SetEncoderFactory(factory)
}

// videoStreamRequest は録画ストリーム API 共通のバリデーション結果。
type videoStreamRequest struct {
	Detail        *database.RecordedProgramDetail
	StreamQuality stream.StreamQualityWithOptions
	SessionID     string
}

// parseVideoStreamRequest はパスと session_id を検証する。
// 移植元: VideoStreamsRouter.ValidateVideoID() / ValidateQuality()
func (s *Server) parseVideoStreamRequest(w http.ResponseWriter, r *http.Request) (*videoStreamRequest, bool) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return nil, false
	}
	detail, err := database.GetRecordedProgramDetail(r.Context(), s.db, videoID)
	if err != nil {
		s.logger.Error("[VideoStreamsRouter][ValidateVideoID] Failed to get the recorded program.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, false
	}
	if detail == nil {
		s.logger.Error("[VideoStreamsRouter][ValidateVideoID] Specified video_id was not found.", "video_id", videoID)
		writeError(w, http.StatusUnprocessableEntity, "Specified video_id was not found")
		return nil, false
	}

	quality := r.PathValue("quality")
	streamQuality, ok := stream.SplitQualityAndEncodingOptions(quality, s.config.General.Encoder)
	if !ok {
		s.logger.Error("[VideoStreamsRouter][ValidateQuality] Specified quality was not found.", "quality", quality)
		writeError(w, http.StatusUnprocessableEntity, "Specified quality was not found")
		return nil, false
	}
	// HLS プレイリストではオリジナル画質で配信できない
	if streamQuality.Quality == stream.OriginalQuality {
		s.logger.Error("[VideoStreamsRouter][ValidateQuality] Original quality is not available for HLS playlist.", "quality", quality)
		writeError(w, http.StatusUnprocessableEntity, "Original quality is not available for HLS playlist")
		return nil, false
	}

	sessionID := r.URL.Query().Get("session_id")
	if !r.URL.Query().Has("session_id") {
		writeError(w, http.StatusUnprocessableEntity, "Field required: query.session_id")
		return nil, false
	}
	return &videoStreamRequest{Detail: detail, StreamQuality: streamQuality, SessionID: sessionID}, true
}

// getVideoSession は録画視聴セッションを取得する。allowNew のときだけ新規作成する。
func (s *Server) getVideoSession(w http.ResponseWriter, r *http.Request, request *videoStreamRequest, allowNew bool) (*videostream.Session, bool) {
	params := videostream.SessionParams{
		SessionID:       request.SessionID,
		Program:         request.Detail,
		Quality:         request.StreamQuality.Quality,
		EncodingOptions: request.StreamQuality.EncodingOptions,
		Encoder:         s.config.General.Encoder,
		WriteDB:         s.writeDB,
	}
	if allowNew {
		// 新規セッション用に DB 保存済みの segment_map を読み込む (読めなくても再生は続行する)
		segmentMap, err := videostream.LoadSegmentMap(r.Context(), s.db, request.Detail.Video.ID)
		if err != nil {
			s.logger.Warn("[VideoStreamsRouter] Failed to load segment_map.", "error", err)
		}
		params.SegmentMap = segmentMap
	}
	session, err := s.videoStreams.Get(params, allowNew)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
	}
	return session, true
}

// handleVideoStreamPlaylist は録画番組 HLS M3U8 プレイリスト API
// (GET /api/streams/video/{video_id}/{quality}/playlist) を処理する。
func (s *Server) handleVideoStreamPlaylist(w http.ResponseWriter, r *http.Request) {
	request, ok := s.parseVideoStreamRequest(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	playlistType := query.Get("type")
	if playlistType == "" {
		playlistType = "primary-audio"
	}
	if playlistType != "master" && playlistType != "primary-audio" && playlistType != "secondary-audio" {
		writeError(w, http.StatusUnprocessableEntity, "Input should be 'master', 'primary-audio' or 'secondary-audio'")
		return
	}
	session, ok := s.getVideoSession(w, r, request, true)
	if !ok {
		return
	}

	var playlist string
	if playlistType == "master" {
		playlist = buildVideoMasterPlaylist(request.SessionID, request.StreamQuality.Quality)
	} else {
		audio := "primary"
		if playlistType == "secondary-audio" {
			audio = "secondary"
		}
		// Python は cache_key が未指定 (None) のときだけ乱数を使う。空文字指定は空のまま使うが、Go 版は空も乱数扱い。
		playlist = session.GetVirtualPlaylist(query.Get("cache_key"), audio)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=0")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, playlist)
}

// buildVideoMasterPlaylist は主音声・副音声を公開するマスタープレイリストを作る。
// MPEG-TS の多重化オーバーヘッドを 10% 見込み、最大映像と 2 本分の音声を収容できる帯域幅を宣言する。
func buildVideoMasterPlaylist(sessionID string, quality string) string {
	qualityInfo := stream.Qualities[quality]
	videoBitrate := parseBitrateK(qualityInfo.VideoBitrateMax) * 1000
	audioBitrate := parseBitrateK(qualityInfo.AudioBitrate) * 1000
	bandwidth := int64(math.RoundToEven(float64(videoBitrate+audioBitrate*2) * 1.1))
	playlistURI := "playlist?session_id=" + sessionID

	// tsreadex は副音声のない区間も無音 AAC で補完するため、常に 2 本の音声トラックを公開する
	playlist := "#EXTM3U\n#EXT-X-VERSION:6\n"
	playlist += "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"主音声\",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE=\"jpn\"\n"
	playlist += fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"副音声\",DEFAULT=NO,AUTOSELECT=YES,LANGUAGE=\"jpn\",URI=\"%s&type=secondary-audio\"\n", playlistURI)
	playlist += fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AUDIO=\"audio\"\n", bandwidth)
	playlist += playlistURI + "&type=primary-audio\n"
	return playlist
}

// parseBitrateK は "13000K" 形式のビットレートを数値 (K 単位) にする。
func parseBitrateK(value string) int64 {
	number, _ := strconv.ParseInt(value[:len(value)-1], 10, 64)
	return number
}

// handleVideoStreamKeepAlive は録画番組 HLS Keep-Alive API
// (PUT /api/streams/video/{video_id}/{quality}/keep-alive) を処理する。
func (s *Server) handleVideoStreamKeepAlive(w http.ResponseWriter, r *http.Request) {
	request, ok := s.parseVideoStreamRequest(w, r)
	if !ok {
		return
	}
	session, ok := s.getVideoSession(w, r, request, false)
	if !ok {
		return
	}
	session.KeepAlive()
	w.WriteHeader(http.StatusNoContent)
}

// formatPythonFloat は Python の json.dumps が float を出力する形式 (整数値でも 1.0 のように小数部を付ける) にする。
func formatPythonFloat(value float64) string {
	text := strconv.FormatFloat(value, 'g', -1, 64)
	for _, character := range text {
		if character == '.' || character == 'e' || character == 'E' {
			return text
		}
	}
	return text + ".0"
}

// writeBufferRangeEvent は buffer_range_update イベントを書き出す。
// 録画がまだ 1 セグメントもエンコードされていない (0, 0) のときだけ、Python の int 0 と同じく "0" を出力する。
func writeBufferRangeEvent(w io.Writer, begin float64, end float64) {
	beginText, endText := formatPythonFloat(begin), formatPythonFloat(end)
	if begin == 0 && end == 0 {
		beginText, endText = "0", "0"
	}
	_, _ = io.WriteString(w, "event: buffer_range_update\r\n")
	_, _ = io.WriteString(w, `data: {"begin": `+beginText+`, "end": `+endText+"}\r\n\r\n")
}

// bufferPollInterval は buffer API がバッファ範囲を確認する間隔。
const bufferPollInterval = 100 * time.Millisecond

// handleVideoStreamBuffer は録画番組 HLS バッファ範囲 API
// (GET /api/streams/video/{video_id}/{quality}/buffer) を処理する (Server-Sent Events) 。
func (s *Server) handleVideoStreamBuffer(w http.ResponseWriter, r *http.Request) {
	request, ok := s.parseVideoStreamRequest(w, r)
	if !ok {
		return
	}
	session, ok := s.getVideoSession(w, r, request, false)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// 初回接続時に必ず現在のバッファ範囲を返す
	previousBegin, previousEnd := session.GetBufferRange()
	writeBufferRangeEvent(w, previousBegin, previousEnd)
	flusher.Flush()

	ticker := time.NewTicker(bufferPollInterval)
	defer ticker.Stop()
	// sse_starlette と同じく 15 秒間隔で ping を送信する
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-pingTicker.C:
			_, _ = io.WriteString(w, ": ping - "+time.Now().Format(time.RFC3339Nano)+"\r\n\r\n")
			flusher.Flush()
			continue
		case <-ticker.C:
		}
		begin, end := session.GetBufferRange()
		// 以前の結果と異なっている場合のみレスポンスを返す
		if begin != previousBegin || end != previousEnd {
			s.logger.Info(fmt.Sprintf("%s Buffer range updated. [begin: %s, end: %s]", session.LogPrefix(), formatPythonFloat(begin), formatPythonFloat(end)))
			writeBufferRangeEvent(w, begin, end)
			flusher.Flush()
			previousBegin, previousEnd = begin, end
		}
	}
}

// handleVideoStreamSegment は録画番組 HLS セグメント API
// (GET /api/streams/video/{video_id}/{quality}/segment) を処理する。
func (s *Server) handleVideoStreamSegment(w http.ResponseWriter, r *http.Request) {
	request, ok := s.parseVideoStreamRequest(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	sequence, err := strconv.Atoi(query.Get("sequence"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Input should be a valid integer: query.sequence")
		return
	}
	if !query.Has("cache_key") {
		writeError(w, http.StatusUnprocessableEntity, "Field required: query.cache_key")
		return
	}
	audio := query.Get("audio")
	if audio == "" {
		audio = "primary"
	}
	if audio != "primary" && audio != "secondary" {
		writeError(w, http.StatusUnprocessableEntity, "Input should be 'primary' or 'secondary'")
		return
	}
	session, ok := s.getVideoSession(w, r, request, false)
	if !ok {
		return
	}

	data, err := session.GetSegment(r.Context(), sequence, audio)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// クライアントが切断した
			return
		case errors.Is(err, videostream.ErrValue):
			s.logger.Error(fmt.Sprintf("%s Failed to get segment. [sequence: %d, audio: %s]", session.LogPrefix(), sequence, audio), "error", err)
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Failed to get segment. [audio: %s]", audio))
		default:
			// ErrRuntime (エンコーダー未登録・入力位置の解決失敗・空データ) とその他の予期しないエラー
			s.logger.Error(fmt.Sprintf("%s Failed to generate segment. [sequence: %d, audio: %s]", session.LogPrefix(), sequence, audio), "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to generate segment. [audio: %s]", audio))
		}
		return
	}
	if data == nil {
		s.logger.Error(fmt.Sprintf("%s Specified sequence segment was not found. [sequence: %d]", session.LogPrefix(), sequence))
		writeError(w, http.StatusUnprocessableEntity, "Specified sequence segment was not found")
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	// キャッシュ有効期間を 3 時間に設定
	w.Header().Set("Cache-Control", "max-age=10800")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
