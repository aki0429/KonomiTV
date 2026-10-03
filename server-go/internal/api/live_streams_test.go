package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// setupLiveStreamTestServer はライブストリーム API のテスト用サーバーを生成する。
func setupLiveStreamTestServer(t *testing.T) *Server {
	t.Helper()
	server, _ := newTestServer(t, "")
	// テストでは実際のエンコードタスクを起動しないよう、ダミーのマネージャーに差し替える
	server.liveStreams = stream.NewManager(server.logger)
	return server
}

// TestLiveStreamValidation はチャンネル ID と画質のバリデーションを検証する。
func TestLiveStreamValidation(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})

	testCases := []struct {
		path   string
		status int
		detail string
	}{
		// 存在しないチャンネル ID
		{"/api/streams/live/gr099/720p", http.StatusUnprocessableEntity, "Specified display_channel_id was not found"},
		// 存在しない IPTV の疑似チャンネル
		{"/api/streams/live/iptv0123456789/720p", http.StatusUnprocessableEntity, "Specified display_channel_id was not found"},
		// 存在しない画質
		{"/api/streams/live/gr011/480p-hevc-10bit-24fps-extra", http.StatusUnprocessableEntity, "Specified quality was not found"},
		// オプションの順序が逆
		{"/api/streams/live/gr011/720p-hevc-24fps-10bit", http.StatusUnprocessableEntity, "Specified quality was not found"},
		// 正しい指定
		{"/api/streams/live/gr011/720p", http.StatusOK, ""},
		{"/api/streams/live/gr011/original", http.StatusOK, ""},
		{"/api/streams/live/gr011/720p-hevc-10bit-24fps", http.StatusOK, ""},
	}
	for _, testCase := range testCases {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if recorder.Code != testCase.status {
			t.Errorf("GET %s: status = %d, want %d (body: %s)", testCase.path, recorder.Code, testCase.status, recorder.Body.String())
			continue
		}
		if testCase.detail != "" {
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse the response: %v", err)
			}
			if body["detail"] != testCase.detail {
				t.Errorf("GET %s: detail = %v, want %v", testCase.path, body["detail"], testCase.detail)
			}
		}
	}
}

// TestLiveStreamValidationRadioChannel はラジオチャンネルで original 画質が拒否されることを検証する。
func TestLiveStreamValidationRadioChannel(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})
	// ラジオチャンネルに更新する
	if _, err := server.writeDB.Exec("UPDATE channels SET is_radiochannel = 1 WHERE display_channel_id = 'gr011'"); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/original", nil))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Original quality is not available for radio channels") {
		t.Errorf("body = %s", recorder.Body.String())
	}
	// original 以外の画質は受け付ける
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/720p", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
}

// TestLiveStreamStatusEndpoint はライブストリームの状態 API を検証する。
func TestLiveStreamStatusEndpoint(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/720p", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var status map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to parse the response: %v", err)
	}
	if status["status"] != "Offline" {
		t.Errorf("status = %v, want Offline", status["status"])
	}
	if status["detail"] != "ライブストリームは Offline です。" {
		t.Errorf("detail = %v", status["detail"])
	}
	if status["client_count"] != float64(0) {
		t.Errorf("client_count = %v, want 0", status["client_count"])
	}
	// Pydantic と同じく float は "0.0" として出力される
	if !strings.Contains(recorder.Body.String(), `"started_at":0.0`) || !strings.Contains(recorder.Body.String(), `"updated_at":0.0`) {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

// TestLiveStreamsListEndpoint はライブストリーム一覧 API を検証する。
func TestLiveStreamsListEndpoint(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})
	insertTestChannel(t, server.db, testChannel{
		ID: "gr012", DisplayChannelID: "gr012", NetworkID: 32736, ServiceID: 2,
		RemoconID: 2, ChannelNumber: "012", Type: "GR", Name: "NHK教育・東京", IsWatchable: true,
	})

	// 1つ目のストリームを ONAir にする
	onAir := server.liveStreams.GetLiveStream("gr011", "720p", stream.StreamEncodingOptions{})
	onAir.SetStatus("Standby", "エンコードタスクを起動しています…", false)
	onAir.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
	// 2つ目のストリームは Offline のまま生成する
	server.liveStreams.GetLiveStream("gr012", "1080p", stream.StreamEncodingOptions{})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/streams/live", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var result map[string]map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse the response: %v", err)
	}
	// すべてのキーが存在する (空でもオブジェクトが返る)
	for _, key := range []string{"Restart", "Idling", "ONAir", "Standby", "Offline"} {
		if _, ok := result[key]; !ok {
			t.Errorf("the key %q is missing", key)
		}
	}
	if len(result["ONAir"]) != 1 || result["ONAir"]["gr011-720p"] == nil {
		t.Errorf("ONAir = %+v", result["ONAir"])
	}
	if result["ONAir"]["gr011-720p"]["status"] != "ONAir" {
		t.Errorf("status = %v, want ONAir", result["ONAir"]["gr011-720p"]["status"])
	}
	if len(result["Offline"]) != 1 || result["Offline"]["gr012-1080p"] == nil {
		t.Errorf("Offline = %+v", result["Offline"])
	}
}

// TestLiveStreamEventsEndpoint はライブストリーム イベント API (SSE) を検証する。
func TestLiveStreamEventsEndpoint(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})

	handler := server.Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/720p/events", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	writer := newStreamingRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, request)
	}()

	// 初期イベント (initial_update) を受信する
	event := writer.NextEvent(t)
	if event.Event != "initial_update" {
		t.Fatalf("event = %q, want initial_update", event.Event)
	}
	if !strings.Contains(event.Data, `"status":"Offline"`) {
		t.Errorf("data = %s", event.Data)
	}

	// ステータスを変更すると status_update イベントが配信される
	liveStream := server.liveStreams.GetLiveStream("gr011", "720p", stream.StreamEncodingOptions{})
	liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false)
	liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)

	event = writer.NextEvent(t)
	if event.Event != "status_update" {
		t.Errorf("event = %q, want status_update", event.Event)
	}
	if !strings.Contains(event.Data, `"status":"ONAir"`) {
		t.Errorf("data = %s", event.Data)
	}

	// クライアント数を変更すると clients_update イベントが配信される
	server.liveStreams.Connect(liveStream, "mpegts")
	event = writer.NextEvent(t)
	if event.Event != "clients_update" {
		t.Errorf("event = %q, want clients_update", event.Event)
	}
	if !strings.Contains(event.Data, `"client_count":1`) {
		t.Errorf("data = %s", event.Data)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the SSE handler did not finish")
	}
}

// TestLiveStreamPSIArchivedDataEndpoint は PSI/SI アーカイブデータ API を検証する。
// PSI/SI データアーカイバーが起動していない場合は 500 が返る。
func TestLiveStreamPSIArchivedDataEndpoint(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})

	// アーカイバーの起動を待つ実装のため、10 秒 + 余裕を持ったタイムアウトを設定する
	handler := server.Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/720p/psi-archived-data", nil)
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "PSI/SI Data Archiver is not running") {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

// TestLiveStreamMPEGTSEndpointWithoutEncoder はエンコーダーが起動できない場合の MPEG-TS API を検証する。
// エンコードタスクは tsreadex の起動に失敗して Offline になり、クライアントは即座に切断される。
func TestLiveStreamMPEGTSEndpointWithoutEncoder(t *testing.T) {
	server := setupLiveStreamTestServer(t)
	insertTestChannel(t, server.db, testChannel{
		ID: "gr011", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合・東京", IsWatchable: true,
	})

	// エンコードタスクを起動するように差し替える (tsreadex は存在しないため Offline になる)
	server.liveStreams.EnableEncodingTasks(stream.TaskOptions{
		Config: server.config,
		Paths:  server.paths,
		Logger: server.logger,
		IPTV:   server.iptv,
		DB:     server.db,
	})

	handler := server.Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/streams/live/gr011/720p/mpegts", nil)
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("Content-Type = %q, want video/mp2t", recorder.Header().Get("Content-Type"))
	}
	// エンコードタスクが起動できないため、ステータスは Offline になる
	liveStream := server.liveStreams.GetLiveStream("gr011", "720p", stream.StreamEncodingOptions{})
	status := liveStream.GetStatus()
	if status.Status != "Offline" {
		t.Errorf("status = %q, want Offline", status.Status)
	}
	if status.ClientCount != 0 {
		t.Errorf("client count = %d, want 0", status.ClientCount)
	}
}

// streamingRecorder はストリーミングのテスト用レスポンスライター (http.Flusher 対応) 。
type streamingRecorder struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buffer []byte
	header http.Header
	status int
}

// newStreamingRecorder はテスト用レスポンスライターを生成する。
func newStreamingRecorder() *streamingRecorder {
	recorder := &streamingRecorder{header: http.Header{}}
	recorder.cond = sync.NewCond(&recorder.mu)
	return recorder
}

// Header はヘッダーを返す。
func (r *streamingRecorder) Header() http.Header {
	return r.header
}

// WriteHeader はステータスコードを記録する。
func (r *streamingRecorder) WriteHeader(status int) {
	r.status = status
}

// Write はレスポンスボディに書き込む。
func (r *streamingRecorder) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buffer = append(r.buffer, data...)
	r.cond.Broadcast()
	return len(data), nil
}

// Flush は何もしない (http.Flusher の実装) 。
func (r *streamingRecorder) Flush() {}

// NextEvent は次の Server-Sent Events のイベントを待機して返す。
func (r *streamingRecorder) NextEvent(t *testing.T) sseEvent {
	t.Helper()
	event := sseEvent{}
	deadline := time.Now().Add(10 * time.Second)
	for {
		line := r.nextLine(t, deadline)
		switch {
		case strings.HasPrefix(line, "event: "):
			event.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.Data = strings.TrimPrefix(line, "data: ")
			return event
		}
	}
}

// nextLine は次の行を待機して返す (空行と ping は読み飛ばす) 。
func (r *streamingRecorder) nextLine(t *testing.T, deadline time.Time) string {
	t.Helper()
	for {
		r.mu.Lock()
		if index := indexByte(r.buffer, '\n'); index >= 0 {
			line := strings.TrimRight(string(r.buffer[:index]), "\r\n")
			r.buffer = r.buffer[index+1:]
			r.mu.Unlock()
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			return line
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out while waiting for the SSE events")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sseEvent は受信した Server-Sent Events のイベント。
type sseEvent struct {
	Event string
	Data  string
}

// indexByte は指定されたバイトの位置を返す。
func indexByte(data []byte, target byte) int {
	for index, value := range data {
		if value == target {
			return index
		}
	}
	return -1
}

var _ io.Writer = (*streamingRecorder)(nil)
