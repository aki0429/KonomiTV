package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// assertPG4Error は実際のルーター応答を本文全体・Content-Type・副作用で固定する。
// status だけでは unknown-API へのフォールスルーや 501 による偽の成功を見逃す。
func assertPG4Error(t *testing.T, server *Server, method, path string, status int, detail string) {
	t.Helper()
	recorder := videoStreamRequest_(t, server, method, path)
	want := fmt.Sprintf("{\"detail\":%q}\n", detail)
	if recorder.Code != status || recorder.Body.String() != want {
		t.Fatalf("%s %s = %d %q; want %d %q", method, path, recorder.Code, recorder.Body.String(), status, want)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if count := server.videoStreams.Count(); count != 0 {
		t.Errorf("検証だけで HLS セッションを作成した: %d", count)
	}
}

// TestPG4OfflineNativeValidation は session_id なしで検証がネイティブに完結する契約を固定する。
// unknown ID の 404 はこの slice 固有の契約であり、Python ValidateVideoID の 422 とは異なる。
func TestPG4OfflineNativeValidation(t *testing.T) {
	server, id := setupVideoStreamTest(t)
	if server.proxy != nil {
		t.Fatal("no-proxy fixture に proxy が設定されている")
	}
	recordingID := insertTestRecordedProgramWithChannel(t, server.db, nil, "録画中", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, recordingID, "/synthetic/recording.ts", "synthetic", "Recording")
	cases := []struct {
		name, videoID, quality, query string
		status                        int
		detail                        string
	}{
		{"整数でない ID", "abc", "1080p", "", 422, "Invalid video_id"},
		{"小数 ID", "1.5", "1080p", "", 422, "Invalid video_id"},
		{"int64 上限超過", "9223372036854775808", "1080p", "", 422, "Invalid video_id"},
		{"未知 ID", "9999", "1080p", "", 404, "Specified video_id was not found"},
		{"ゼロ ID", "0", "1080p", "", 404, "Specified video_id was not found"},
		{"負の ID", "-1", "1080p", "", 404, "Specified video_id was not found"},
		{"未知 ID は quality より先", "9999", "invalid", "", 404, "Specified video_id was not found"},
		{"不正 ID は quality より先", "abc", "original", "", 422, "Invalid video_id"},
		{"未知 quality", fmt.Sprint(id), "9999p", "", 422, "Specified quality was not found"},
		{"逆順オプション", fmt.Sprint(id), "1080p-hevc-24fps-10bit", "", 422, "Specified quality was not found"},
		{"重複 10bit", fmt.Sprint(id), "1080p-hevc-10bit-10bit", "", 422, "Specified quality was not found"},
		{"重複 24fps", fmt.Sprint(id), "1080p-hevc-24fps-24fps", "", 422, "Specified quality was not found"},
		{"quality 大文字は不正", fmt.Sprint(id), "1080P", "", 422, "Specified quality was not found"},
		{"original", fmt.Sprint(id), "original", "", 422, "Original quality is not available for HLS playlist"},
		{"録画中", fmt.Sprint(recordingID), "1080p", "", 409, "Recording video cannot be saved for offline playback"},
		{"録画中でも quality が先", fmt.Sprint(recordingID), "invalid", "", 422, "Specified quality was not found"},
		{"録画中でも original が先", fmt.Sprint(recordingID), "original", "", 422, "Original quality is not available for HLS playlist"},
		{"session を付けても録画中は拒否", fmt.Sprint(recordingID), "1080p", "?session_id=unrelated", 409, "Recording video cannot be saved for offline playback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertPG4Error(t, server, http.MethodGet, "/api/streams/video/"+c.videoID+"/"+c.quality+"/offline-stream"+c.query, c.status, c.detail)
		})
	}
}

// TestPG4HLSValidationOrderUnchanged は共通化後も既存 HLS の検証順と 422 を保護する。
func TestPG4HLSValidationOrderUnchanged(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	cases := []struct{ method, endpoint string }{
		{http.MethodGet, "playlist"}, {http.MethodGet, "segment"},
		{http.MethodGet, "buffer"}, {http.MethodPut, "keep-alive"},
	}
	for _, c := range cases {
		t.Run(c.endpoint, func(t *testing.T) {
			base := "/api/streams/video/"
			assertPG4Error(t, server, c.method, base+"9999/invalid/"+c.endpoint, 422, "Specified video_id was not found")
			assertPG4Error(t, server, c.method, base+"abc/original/"+c.endpoint, 422, "Invalid video_id")
			assertPG4Error(t, server, c.method, base+"1/invalid/"+c.endpoint, 422, "Specified quality was not found")
			assertPG4Error(t, server, c.method, base+"1/1080p-hevc-24fps-10bit/"+c.endpoint, 422, "Specified quality was not found")
			assertPG4Error(t, server, c.method, base+"1/original/"+c.endpoint, 422, "Original quality is not available for HLS playlist")
			assertPG4Error(t, server, c.method, base+"1/720p-hevc-10bit-24fps/"+c.endpoint, 422, "Field required: query.session_id")
		})
	}
}

// TestPG4OfflineNoProxyFallback は proxy が存在しても検証の全結果が転送されないことを示す。
func TestPG4OfflineNoProxyFallback(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	calls := 0
	server.proxy = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unexpected proxy", http.StatusBadGateway)
	})
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/9999/1080p/offline-stream", 404, "Specified video_id was not found")
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/1/invalid/offline-stream", 422, "Specified quality was not found")
	if calls != 0 {
		t.Fatalf("proxy calls = %d", calls)
	}
}

// TestPG4OfflineNoSessionRequired は有効 Recorded が session_id の有無によらず未実装 gate に達することを確認する。
// 501 は保存成功ではない。本文が KTVODLP になったと偽って扱わない。
func TestPG4OfflineNoSessionRequired(t *testing.T) {
	server, id := setupVideoStreamTest(t)
	qualities := []string{"1080p", "720p", "240p", "720p-hevc-10bit-24fps", "1080p-60fps-hevc-10bit-24fps"}
	for _, quality := range qualities {
		for _, query := range []string{"", "?session_id=", "?session_id=unrelated"} {
			assertPG4Error(t, server, http.MethodGet, fmt.Sprintf("/api/streams/video/%d/%s/offline-stream%s", id, quality, query), 501, "Offline stream generation is not implemented")
		}
	}
}

// TestPG4OfflineRecordingCaseSensitive は Python の status == 'Recording' と同じ厳密比較を固定する。
func TestPG4OfflineRecordingCaseSensitive(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	for _, status := range []string{"recording", "RECORDING", "Recorded"} {
		id := insertTestRecordedProgramWithChannel(t, server.db, nil, status, time.Now())
		insertTestRecordedVideoWithFile(t, server.db, id, "/synthetic/missing.ts", status, status)
		assertPG4Error(t, server, http.MethodGet, fmt.Sprintf("/api/streams/video/%d/720p/offline-stream", id), 501, "Offline stream generation is not implemented")
	}
}

// TestPG4OfflineDBFailure は DB の失敗を unknown ID や未実装として隠さないことを保護する。
func TestPG4OfflineDBFailure(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/1/720p/offline-stream", 500, "Internal Server Error")
}

// TestPG4OfflineWrongMethod は GET 以外で検証層を呼び出さないことを保護する。
func TestPG4OfflineWrongMethod(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, "/api/streams/video/1/1080p/offline-stream", nil))
		if strings.Contains(recorder.Body.String(), "Offline stream generation") || server.videoStreams.Count() != 0 {
			t.Fatalf("%s が GET handler を呼び出した: %d %s", method, recorder.Code, recorder.Body.String())
		}
	}
}
