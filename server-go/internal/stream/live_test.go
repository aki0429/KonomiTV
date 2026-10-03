package stream

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newTestManager はテスト用のマネージャーを生成する。
func newTestManager() *Manager {
	return NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestLiveStreamIDSingleton はライブストリーム ID ごとに1つのインスタンスになることを検証する。
func TestLiveStreamIDSingleton(t *testing.T) {
	manager := newTestManager()

	streamA := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	streamB := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	if streamA != streamB {
		t.Errorf("the same live stream ID should return the same instance")
	}
	if streamA.ID != "gr011-720p" {
		t.Errorf("live stream ID = %q, want gr011-720p", streamA.ID)
	}

	// 画質が異なれば別のインスタンスになる
	if manager.GetLiveStream("gr011", "1080p", StreamEncodingOptions{}) == streamA {
		t.Errorf("a different quality should return a different instance")
	}

	// エンコードオプションのサフィックスが ID に含まれる
	withOptions := manager.GetLiveStream("gr011", "720p-hevc", StreamEncodingOptions{
		IsHEVC10bitEnabled: true,
		Is24fpsModeEnabled: true,
	})
	if withOptions.ID != "gr011-720p-hevc-10bit-24fps" {
		t.Errorf("live stream ID = %q, want gr011-720p-hevc-10bit-24fps", withOptions.ID)
	}
	only24fps := manager.GetLiveStream("gr011", "720p-hevc", StreamEncodingOptions{Is24fpsModeEnabled: true})
	if only24fps.ID != "gr011-720p-hevc-24fps" {
		t.Errorf("live stream ID = %q, want gr011-720p-hevc-24fps", only24fps.ID)
	}

	// 生成順に取得できる
	ids := []string{}
	for _, liveStream := range manager.GetAllLiveStreams() {
		ids = append(ids, liveStream.ID)
	}
	want := []string{"gr011-720p", "gr011-1080p", "gr011-720p-hevc-10bit-24fps", "gr011-720p-hevc-24fps"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("live stream IDs = %v, want %v", ids, want)
	}
}

// TestLiveStreamInitialStatus は初期状態が Offline であることを検証する。
func TestLiveStreamInitialStatus(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	status := liveStream.GetStatus()
	if status.Status != "Offline" {
		t.Errorf("status = %q, want Offline", status.Status)
	}
	if status.Detail != "ライブストリームは Offline です。" {
		t.Errorf("detail = %q", status.Detail)
	}
	if status.ClientCount != 0 || status.StartedAt != 0 || status.UpdatedAt != 0 {
		t.Errorf("unexpected initial status: %+v", status)
	}
}

// TestSetStatusRules はステータス更新のルールが Python 版と一致することを検証する。
func TestSetStatusRules(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})

	// 同じステータス・同じ詳細は更新されない
	if liveStream.SetStatus("Offline", "ライブストリームは Offline です。", false) {
		t.Errorf("the same status should not be updated")
	}
	// Offline から Restart には移行しない
	if liveStream.SetStatus("Restart", "再起動しています…", false) {
		t.Errorf("Offline -> Restart should not be allowed")
	}
	// Offline から Standby には移行し、started_at が設定される
	if !liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false) {
		t.Fatalf("Offline -> Standby should be allowed")
	}
	status := liveStream.GetStatus()
	if status.Status != "Standby" || status.StartedAt == 0 || status.UpdatedAt == 0 {
		t.Errorf("unexpected status after Standby: %+v", status)
	}
	if status.StartedAt != status.UpdatedAt {
		t.Errorf("started_at (%v) should equal updated_at (%v) on standby", status.StartedAt, status.UpdatedAt)
	}
	// Standby から ONAir には移行する
	if !liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false) {
		t.Errorf("Standby -> ONAir should be allowed")
	}
	// ONAir から Idling には移行する
	if !liveStream.SetStatus("Idling", "ライブストリームは Idling です。", false) {
		t.Errorf("ONAir -> Idling should be allowed")
	}
	// Idling から Restart には移行する
	if !liveStream.SetStatus("Restart", "エンコードタスクを再起動しています…", false) {
		t.Errorf("Idling -> Restart should be allowed")
	}
	// Restart から Standby には移行し、started_at が更新される
	previousStartedAt := liveStream.GetStatus().StartedAt
	time.Sleep(10 * time.Millisecond)
	if !liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false) {
		t.Errorf("Restart -> Standby should be allowed")
	}
	if liveStream.GetStatus().StartedAt <= previousStartedAt {
		t.Errorf("started_at should be updated on Restart -> Standby")
	}
}

// TestConnectAndDisconnect はクライアントの接続と切断を検証する。
func TestConnectAndDisconnect(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})

	// エンコードタスクを起動しないようにする (startTask を設定しない)
	client := manager.Connect(liveStream, "mpegts")
	if client.ID == "" || !strings.HasPrefix(client.ID, "MPEGTS-") {
		t.Errorf("client ID = %q, want the MPEGTS- prefix", client.ID)
	}
	if liveStream.GetStatus().ClientCount != 1 {
		t.Errorf("client count = %d, want 1", liveStream.GetStatus().ClientCount)
	}
	// Offline から接続すると Standby に移行する (エンコードタスクは起動しない)
	if liveStream.GetStatus().Status != "Standby" {
		t.Errorf("status = %q, want Standby", liveStream.GetStatus().Status)
	}

	// 視聴者数が集計される
	if count := manager.GetViewerCount("gr011"); count != 1 {
		t.Errorf("viewer count = %d, want 1", count)
	}
	if count := manager.GetViewerCount("gr012"); count != 0 {
		t.Errorf("viewer count for another channel = %d, want 0", count)
	}

	// ストリームデータがクライアントに届く
	liveStream.WriteStreamData([]byte("stream-data"))
	select {
	case data, ok := <-client.Queue():
		if !ok || string(data) != "stream-data" {
			t.Errorf("stream data = %q (ok=%v), want stream-data", data, ok)
		}
	case <-time.After(time.Second):
		t.Fatalf("stream data was not delivered")
	}

	// 切断するとクライアント数が減り、キューが閉じられる
	liveStream.Disconnect(client)
	if liveStream.GetStatus().ClientCount != 0 {
		t.Errorf("client count = %d, want 0", liveStream.GetStatus().ClientCount)
	}
	if _, ok := <-client.Queue(); ok {
		t.Errorf("the queue should be closed after disconnecting")
	}
	// 二重に切断しても何も起きない
	liveStream.Disconnect(client)
}

// TestIdlingToONAirOnConnect は Idling 状態で接続すると ONAir に復帰することを検証する。
func TestIdlingToONAirOnConnect(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	liveStream.SetStatus("Standby", "エンコードタスクを起動しています…", false)
	liveStream.SetStatus("ONAir", "ライブストリームは ONAir です。", false)
	liveStream.SetStatus("Idling", "ライブストリームは Idling です。", false)

	client := manager.Connect(liveStream, "mpegts")
	defer liveStream.Disconnect(client)
	status := liveStream.GetStatus()
	if status.Status != "ONAir" {
		t.Errorf("status = %q, want ONAir", status.Status)
	}
	if status.Detail != "ライブストリームは ONAir です。" {
		t.Errorf("detail = %q", status.Detail)
	}
}

// TestWriteStreamDataTimeout は読み取りが滞ったクライアントが削除されることを検証する。
func TestWriteStreamDataTimeout(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	client := manager.Connect(liveStream, "mpegts")

	// 最終読み取り時刻を 11 秒前にする
	client.mu.Lock()
	client.streamDataReadAt = unixNow() - 11
	client.mu.Unlock()

	liveStream.WriteStreamData([]byte("stream-data"))
	if liveStream.GetStatus().ClientCount != 0 {
		t.Errorf("client count = %d, want 0 (the client should be removed by the timeout)", liveStream.GetStatus().ClientCount)
	}
}

// TestDisconnectAll はすべてのクライアントの切断を検証する。
func TestDisconnectAll(t *testing.T) {
	manager := newTestManager()
	liveStream := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	clientA := manager.Connect(liveStream, "mpegts")
	clientB := manager.Connect(liveStream, "mpegts")
	if liveStream.GetStatus().ClientCount != 2 {
		t.Fatalf("client count = %d, want 2", liveStream.GetStatus().ClientCount)
	}

	liveStream.DisconnectAll()
	if liveStream.GetStatus().ClientCount != 0 {
		t.Errorf("client count = %d, want 0", liveStream.GetStatus().ClientCount)
	}
	for _, client := range []*LiveStreamClient{clientA, clientB} {
		if _, ok := <-client.Queue(); ok {
			t.Errorf("the queue should be closed after DisconnectAll")
		}
	}
}

// TestStatusesByStatus はステータスごとの分類を検証する。
func TestStatusesByStatus(t *testing.T) {
	manager := newTestManager()
	offline := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	onAir := manager.GetLiveStream("gr012", "720p", StreamEncodingOptions{})
	onAir.SetStatus("Standby", "エンコードタスクを起動しています…", false)
	onAir.SetStatus("ONAir", "ライブストリームは ONAir です。", false)

	result := manager.StatusesByStatus()
	if len(result["Offline"]) != 1 || result["Offline"][offline.ID].Status != "Offline" {
		t.Errorf("Offline = %+v", result["Offline"])
	}
	if len(result["ONAir"]) != 1 || result["ONAir"][onAir.ID].Status != "ONAir" {
		t.Errorf("ONAir = %+v", result["ONAir"])
	}
	for _, status := range []string{"Restart", "Idling", "Standby"} {
		if len(result[status]) != 0 {
			t.Errorf("%s = %+v, want empty", status, result[status])
		}
	}
}

// TestGetChannelViewerCounts は視聴者数の一覧を検証する。
func TestGetChannelViewerCounts(t *testing.T) {
	manager := newTestManager()
	liveStream720p := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	liveStream1080p := manager.GetLiveStream("gr011", "1080p", StreamEncodingOptions{})
	manager.Connect(liveStream720p, "mpegts")
	manager.Connect(liveStream1080p, "mpegts")
	manager.Connect(liveStream1080p, "mpegts")

	counts := manager.GetChannelViewerCounts()
	if counts["gr011"] != 3 {
		t.Errorf("viewer count = %d, want 3", counts["gr011"])
	}
	if len(counts) != 1 {
		t.Errorf("viewer counts = %+v, want only gr011", counts)
	}
}

// TestIsFullHDChannelID はフル HD チャンネルの判定が Python 版と一致することを検証する。
func TestIsFullHDChannelID(t *testing.T) {
	testCases := []struct {
		networkID int
		serviceID int
		want      bool
	}{
		{31811, 1, true},     // テレビ宮崎
		{32466, 1, true},     // ABS秋田放送
		{0x0004, 103, true},  // NHK BSプレミアム
		{0x0004, 211, true},  // BS11
		{0x0004, 101, false}, // NHK BS1
		{0x000B, 1, true},    // BS4K
		{0x000C, 1, true},    // CS4K
		{32736, 1, false},    // NHK総合 (東京)
	}
	for _, testCase := range testCases {
		if actual := isFullHDChannelID(testCase.networkID, testCase.serviceID); actual != testCase.want {
			t.Errorf("isFullHDChannelID(%d, %d) = %v, want %v", testCase.networkID, testCase.serviceID, actual, testCase.want)
		}
	}
}

// TestIsOffTheAirProgram は停波中の番組の判定が Python 版と一致することを検証する。
func TestIsOffTheAirProgram(t *testing.T) {
	testCases := []struct {
		title string
		want  bool
	}{
		{"番組情報がありません", true},
		{"放送休止", true},
		{"放送終了", true},
		{"休止中", true},
		{"停波のお知らせ", true},
		{"NHKニュース7", false},
		{"", false},
	}
	for _, testCase := range testCases {
		if actual := isOffTheAirProgram(testCase.title); actual != testCase.want {
			t.Errorf("isOffTheAirProgram(%q) = %v, want %v", testCase.title, actual, testCase.want)
		}
	}
}

// TestIsEncoderNoise はエンコーダーのログの除外判定を検証する。
func TestIsEncoderNoise(t *testing.T) {
	noise := []string{
		"",
		"removing 2 bytes from input bitstream not read by decoder.",
		"Delay between the first packet and last packet in the muxing queue is 1000000 > 1: forcing output",
		"[h264_metadata @ 0000000000000000] some message",
		"packet in the muxing queue",
		"ing output",
		"output",
		"t",
	}
	for _, line := range noise {
		if !isEncoderNoise(line) {
			t.Errorf("isEncoderNoise(%q) = false, want true", line)
		}
	}
	keep := []string{
		"frame=    1 fps=0.0 q=0.0",
		"Stream #0:0: Video: h264",
		"frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2519.8kbits/s speed=1x",
	}
	for _, line := range keep {
		if isEncoderNoise(line) {
			t.Errorf("isEncoderNoise(%q) = true, want false", line)
		}
	}
}

// TestEncoderProgressPattern はエンコード進捗のログからゴミを取り除けることを検証する。
func TestEncoderProgressPattern(t *testing.T) {
	testCases := []struct {
		line string
		want string
	}{
		{
			"frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2519.8kbits/s speed=1x",
			"frame=  100 fps= 30 q=28.0 size=    1024kB time=00:00:03.33 bitrate=2519.8kbits/s speed=1x",
		},
		{
			"[NVEncC] garbage 123 frames: 30.0 fps, 2500 kb/s, GPU 5%, VE 10%, VD 20%",
			"123 frames: 30.0 fps, 2500 kb/s, GPU 5%, VE 10%, VD 20%",
		},
		{
			"garbage 123 frames: 30.0 fps, 2500 kb/s, GPU 5%, VD 20%",
			"123 frames: 30.0 fps, 2500 kb/s, GPU 5%, VD 20%",
		},
		{
			"garbage 123 frames: 30.0 fps, 2500 kb/s",
			"123 frames: 30.0 fps, 2500 kb/s",
		},
		{
			"no progress information",
			"no progress information",
		},
	}
	for _, testCase := range testCases {
		line := testCase.line
		if match := encoderProgressPattern.FindStringSubmatch(line); match != nil {
			line = match[1]
		}
		if line != testCase.want {
			t.Errorf("progress line = %q, want %q", line, testCase.want)
		}
	}
}
