package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestVideoStreamOfflineStreamValidation は offline-stream のネイティブ検証層を検証する。
// 移植元: VideoStreamsRouter.ValidateVideoID() / ValidateQuality() / VideoOfflineStreamAPI()
func TestVideoStreamOfflineStreamValidation(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	// 録画中の番組 (status = Recording) を追加する
	recordingProgramID := insertTestRecordedProgramWithChannel(t, server.db, nil, "録画中の番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, recordingProgramID, "/recorded/recording.ts", "recording-hash", "Recording")

	cases := []struct {
		name   string
		path   string
		status int
		detail string
	}{
		// 不明 video は 404 (unknown-API の {"detail":"Not Found"} へフォールスルーしない)
		{"unknown video", "/api/streams/video/9999/1080p/offline-stream", http.StatusNotFound, "Specified video_id was not found"},
		// 不正 quality は Python 版 ValidateQuality() と同じ 422
		{"invalid quality", "/api/streams/video/1/9999p/offline-stream", http.StatusUnprocessableEntity, "Specified quality was not found"},
		{"reversed quality options", "/api/streams/video/1/1080p-hevc-24fps-10bit/offline-stream", http.StatusUnprocessableEntity, "Specified quality was not found"},
		// original は HLS 配信不可のため 422
		{"original quality", "/api/streams/video/1/original/offline-stream", http.StatusUnprocessableEntity, "Original quality is not available for HLS playlist"},
		// 録画中は Python 版と同じ 409
		{"recording video", "/api/streams/video/" + strconv.FormatInt(recordingProgramID, 10) + "/1080p/offline-stream", http.StatusConflict, "Recording video cannot be saved for offline playback"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// offline-stream は session_id を要求しない (パスと検証のみ)
			recorder := videoStreamRequest_(t, server, http.MethodGet, testCase.path)
			var body map[string]string
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			if recorder.Code != testCase.status || body["detail"] != testCase.detail {
				t.Errorf("%s: %d %q, want %d %q", testCase.path, recorder.Code, body["detail"], testCase.status, testCase.detail)
			}
		})
	}
}

// TestVideoStreamOfflineStreamBodyNotImplemented は、ストリーム本体を実装するまでの間、
// 検証を通過したリクエストが未実装 (501) を返すことを検証する。
// TODO(pg4-offline-stream): KTVODLP ストリーム本体の後続スライス実装時にこのテストを差し替える。
func TestVideoStreamOfflineStreamBodyNotImplemented(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	recorder := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/offline-stream")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if body["detail"] != "Offline stream generation is not implemented" {
		t.Errorf("detail = %q", body["detail"])
	}
}
