package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// setupVideoStreamTest は録画番組 (3600 秒・29.97fps) を 1 件登録したテスト用サーバーを返す。
func setupVideoStreamTest(t *testing.T) (*Server, int64) {
	t.Helper()
	server, _ := newTestServer(t, "")
	channelID := "NID32736-SID1024"
	insertTestChannel(t, server.db, testChannel{
		ID: channelID, DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	programID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "番組", time.Now().Add(-time.Hour))
	insertTestRecordedVideoWithFile(t, server.db, programID, "/recorded/not-exist.ts", "hash", "Recorded")
	return server, programID
}

func videoStreamRequest_(t *testing.T, server *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func TestVideoStreamValidation(t *testing.T) {
	server, id := setupVideoStreamTest(t)
	cases := []struct {
		path   string
		status int
		detail string
	}{
		{"/api/streams/video/9999/1080p/playlist?session_id=a", 422, "Specified video_id was not found"},
		{"/api/streams/video/1/9999p/playlist?session_id=a", 422, "Specified quality was not found"},
		{"/api/streams/video/1/1080p-hevc-24fps-10bit/playlist?session_id=a", 422, "Specified quality was not found"},
		{"/api/streams/video/1/original/playlist?session_id=a", 422, "Original quality is not available for HLS playlist"},
		{"/api/streams/video/1/1080p/keep-alive?session_id=nothing", 422, "Session does not exist"},
		{"/api/streams/video/1/1080p/segment?session_id=nothing&sequence=0&cache_key=k", 422, "Session does not exist"},
	}
	_ = id
	for _, c := range cases {
		method := http.MethodGet
		if strings.Contains(c.path, "keep-alive") {
			method = http.MethodPut
		}
		recorder := videoStreamRequest_(t, server, method, c.path)
		var body map[string]string
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != c.status || body["detail"] != c.detail {
			t.Errorf("%s: %d %q, want %d %q", c.path, recorder.Code, body["detail"], c.status, c.detail)
		}
	}
}

func TestVideoStreamMasterPlaylist(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	// 1080p: video_bitrate_max 13000K, audio 256K → round((13000000 + 512000) * 1.1) = 14863200
	recorder := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/1080p-hevc-10bit/playlist?session_id=m1&type=master")
	if recorder.Code != 200 {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "max-age=0" {
		t.Errorf("Cache-Control = %q", got)
	}
	// 1080p-hevc: 4500K / 192K → (4500000 + 384000) * 1.1 = 5372400
	want := "#EXTM3U\n#EXT-X-VERSION:6\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"主音声\",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE=\"jpn\"\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"副音声\",DEFAULT=NO,AUTOSELECT=YES,LANGUAGE=\"jpn\",URI=\"playlist?session_id=m1&type=secondary-audio\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5372400,AUDIO=\"audio\"\n" +
		"playlist?session_id=m1&type=primary-audio\n"
	if recorder.Body.String() != want {
		t.Errorf("master playlist:\n%s\nwant:\n%s", recorder.Body.String(), want)
	}
	// 1080p-60fps: (13000000 + 2*256000) * 1.1 = 14863200
	if got := buildVideoMasterPlaylist("x", "1080p-60fps"); !strings.Contains(got, "BANDWIDTH=14863200,") {
		t.Errorf("1080p-60fps bandwidth: %s", got)
	}
	// 240p: (650000 + 256000) * 1.1 = 996600
	if got := buildVideoMasterPlaylist("x", "240p"); !strings.Contains(got, "BANDWIDTH=996600,") {
		t.Errorf("240p bandwidth: %s", got)
	}
}

func TestVideoStreamMediaPlaylistAndKeepAlive(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	recorder := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=p1&cache_key=abcd1234")
	if recorder.Code != 200 {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	want := videostream.BuildVirtualPlaylist("p1", "abcd1234", 29.97, 3600, "primary")
	if recorder.Body.String() != want {
		t.Error("プレイリスト本文が BuildVirtualPlaylist と一致しません")
	}
	secondary := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=p1&cache_key=abcd1234&type=secondary-audio")
	if !strings.Contains(secondary.Body.String(), "&audio=secondary") {
		t.Error("副音声プレイリストに audio=secondary がありません")
	}
	// cache_key 省略時は 8 桁の乱数
	generated := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=p1")
	if !strings.Contains(generated.Body.String(), "&cache_key=") {
		t.Error("cache_key が生成されていません")
	}
	if bad := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=p1&type=bogus"); bad.Code != 422 {
		t.Errorf("不正な type = %d", bad.Code)
	}
	if bad := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist"); bad.Code != 422 {
		t.Errorf("session_id 欠落 = %d", bad.Code)
	}
	// 同じ session_id で画質違いは 422
	mismatch := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/480p/playlist?session_id=p1")
	if mismatch.Code != 422 || !strings.Contains(mismatch.Body.String(), "mismatch") {
		t.Errorf("mismatch = %d %s", mismatch.Code, mismatch.Body.String())
	}
	// keep-alive は 204
	if keep := videoStreamRequest_(t, server, http.MethodPut, "/api/streams/video/1/720p/keep-alive?session_id=p1"); keep.Code != 204 {
		t.Errorf("keep-alive = %d", keep.Code)
	}
	if server.videoStreams.Count() != 1 {
		t.Errorf("セッション数 = %d", server.videoStreams.Count())
	}
}

// 実在しない録画ファイルでも、DB の segment_map に載っているセグメントはファイル I/O なしでエンコーダーへ渡る。
type apiFakeEncoder struct {
	session *videostream.Session
	mode    string
}

func (e *apiFakeEncoder) Run(ctx context.Context, start int) error {
	switch e.mode {
	case "ok":
		e.session.CompleteAndAdvance(start, []byte{0x47, 1, 2, 3})
		return nil
	case "fail":
		return errors.New("encoder crashed")
	default:
		<-ctx.Done()
		return ctx.Err()
	}
}
func (e *apiFakeEncoder) Cancel() {}

func TestVideoStreamSegment(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	if _, err := server.db.Exec(`UPDATE recorded_videos SET segment_map = '[{"sequence_index":0,"source_file_position":0,"source_start_dts":900000},{"sequence_index":1,"source_file_position":10,"source_start_dts":1440540}]'`); err != nil {
		t.Fatal(err)
	}
	const playlist = "/api/streams/video/1/1080p/playlist?session_id=sg&cache_key=k"
	const segment = "/api/streams/video/1/1080p/segment?session_id=sg&cache_key=k&sequence="

	videoStreamRequest_(t, server, http.MethodGet, playlist)
	// エンコーダー未登録は 500
	recorder := videoStreamRequest_(t, server, http.MethodGet, segment+"0")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), "Failed to generate segment. [audio: primary]") {
		t.Errorf("エンコーダー未登録 = %d %s", recorder.Code, recorder.Body.String())
	}

	server.SetVideoSegmentEncoderFactory(func(s *videostream.Session) videostream.SegmentEncoder {
		return &apiFakeEncoder{session: s, mode: "ok"}
	})
	recorder = videoStreamRequest_(t, server, http.MethodGet, segment+"0")
	if recorder.Code != 200 || recorder.Header().Get("Content-Type") != "video/mp2t" ||
		recorder.Header().Get("Cache-Control") != "max-age=10800" || recorder.Body.Len() != 4 {
		t.Errorf("segment 0 = %d %v %q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	// 範囲外・負のシーケンスは 422
	for _, sequence := range []string{"-1", "100000"} {
		recorder = videoStreamRequest_(t, server, http.MethodGet, segment+sequence)
		if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "Specified sequence segment was not found") {
			t.Errorf("sequence %s = %d %s", sequence, recorder.Code, recorder.Body.String())
		}
	}
	// 副音声の抽出失敗 (ダミーデータ) は ValueError 相当の 422
	recorder = videoStreamRequest_(t, server, http.MethodGet, segment+"0&audio=secondary")
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "Failed to get segment. [audio: secondary]") {
		t.Errorf("secondary = %d %s", recorder.Code, recorder.Body.String())
	}
	if bad := videoStreamRequest_(t, server, http.MethodGet, segment+"x"); bad.Code != 422 {
		t.Errorf("不正な sequence = %d", bad.Code)
	}
	if bad := videoStreamRequest_(t, server, http.MethodGet, "/api/streams/video/1/1080p/segment?session_id=sg&sequence=0"); bad.Code != 422 {
		t.Errorf("cache_key 欠落 = %d", bad.Code)
	}

	// エンコーダー異常終了 (RuntimeError 相当) は 500
	server.SetVideoSegmentEncoderFactory(func(s *videostream.Session) videostream.SegmentEncoder {
		return &apiFakeEncoder{session: s, mode: "fail"}
	})
	recorder = videoStreamRequest_(t, server, http.MethodGet, segment+"1")
	if recorder.Code != 500 {
		t.Errorf("encoder fail = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestVideoStreamBufferEvents(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	if resp, err := http.Get(httpServer.URL + "/api/streams/video/1/1080p/playlist?session_id=bf&cache_key=k"); err != nil {
		t.Fatal(err)
	} else {
		_ = resp.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/streams/video/1/1080p/buffer?session_id=bf", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	readEvent := func() (string, string) {
		var event, data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE 読み取り失敗: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && event != "":
				return event, data
			}
		}
	}
	// 初回は必ず送信される (まだエンコード済みが無いので 0, 0)
	if event, data := readEvent(); event != "buffer_range_update" || data != `{"begin": 0, "end": 0}` {
		t.Errorf("初回 = %q %q", event, data)
	}
	// セグメント 0 を完了させると変化として送信される
	found := server.videoStreams.Lookup("bf")
	if found == nil {
		t.Fatal("セッションが見つかりません")
	}
	found.CompleteAndAdvance(0, []byte("x"))
	event, data := readEvent()
	var payload struct{ Begin, End float64 }
	if err := json.Unmarshal([]byte(strings.ToLower(data)), &payload); err != nil || event != "buffer_range_update" || payload.Begin != 0 || payload.End < 6 || payload.End > 6.01 {
		t.Errorf("更新 = %q %q (%v)", event, data, err)
	}
	if !strings.HasPrefix(data, `{"begin": 0.0, "end": 6.006`) {
		t.Errorf("Python 形式の float ではありません: %s", data)
	}
}

func TestFormatPythonFloat(t *testing.T) {
	for in, want := range map[float64]string{0: "0.0", 12: "12.0", 6.006: "6.006", 1.5e-7: "1.5e-07"} {
		if got := formatPythonFloat(in); got != want && in != 1.5e-7 {
			t.Errorf("formatPythonFloat(%v) = %q, want %q", in, got, want)
		}
	}
}
