package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// testChannelWithTSID は TSID 付きのテスト用チャンネル定義を生成するヘルパー。
func testChannelWithTSID(channel testChannel, transportStreamID int) testChannel {
	channel.TransportStreamID = &transportStreamID
	return channel
}

// timeTableTestSetup は番組表テスト用のチャンネルと番組を作成する。
func timeTableTestSetup(t *testing.T, server *Server) (now time.Time) {
	t.Helper()
	now = time.Now()
	// 地デジ (メインチャンネルとサブチャンネルは同一 TS)
	insertTestChannel(t, server.db, testChannelWithTSID(testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	}, 32736))
	insertTestChannel(t, server.db, testChannelWithTSID(testChannel{
		ID: "NID32736-SID1025", DisplayChannelID: "gr012", NetworkID: 32736, ServiceID: 1025,
		RemoconID: 1, ChannelNumber: "012", Type: "GR", Name: "NHK総合2・東京", IsSubchannel: true, IsWatchable: true,
	}, 32736))
	// BS (TSID なし)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID4-SID101", DisplayChannelID: "bs101", NetworkID: 4, ServiceID: 101,
		RemoconID: 2, ChannelNumber: "101", Type: "BS", Name: "NHK BS1", IsWatchable: true,
	})
	// 視聴不可チャンネルは番組表に出ない
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1099", DisplayChannelID: "gr099", NetworkID: 32736, ServiceID: 1099,
		RemoconID: 9, ChannelNumber: "099", Type: "GR", Name: "視聴不可チャンネル", IsWatchable: false,
	})

	insertTestProgram(t, server.db, "NID32736-SID1024-E1", "NID32736-SID1024", "メインの番組", now.Add(10*time.Minute), now.Add(40*time.Minute), 1800.0)
	insertTestProgram(t, server.db, "NID32736-SID1025-E1", "NID32736-SID1025", "サブの番組", now.Add(50*time.Minute), now.Add(80*time.Minute), 1800.0)
	insertTestProgram(t, server.db, "NID4-SID101-E1", "NID4-SID101", "BS の番組", now.Add(5*time.Minute), now.Add(35*time.Minute), 1800.0)
	return now
}

// TestProgramTimeTable は番組表 API の基本的な挙動を検証する。
func TestProgramTimeTable(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	timeTableTestSetup(t, server)

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/programs/timetable", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response timeTableResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}

	// 地デジ2チャンネルはサブチャンネルがメインに統合されるため、GR 1列 + BS 1列の2列になる
	if len(response.Channels) != 2 {
		t.Fatalf("channels = %d, want 2 (body: %s)", len(response.Channels), recorder.Body.String())
	}
	gr := response.Channels[0]
	if gr.Channel.ID != "NID32736-SID1024" {
		t.Errorf("first channel = %q, want the GR main channel", gr.Channel.ID)
	}
	if len(gr.Programs) != 1 || gr.Programs[0].Title != "メインの番組" {
		t.Errorf("GR programs = %+v", gr.Programs)
	}
	if gr.Programs[0].Duration != 1800.0 {
		t.Errorf("duration = %v", gr.Programs[0].Duration)
	}
	if !strings.Contains(recorder.Body.String(), `"duration":1800.0`) {
		t.Errorf("duration should be serialized as 1800.0")
	}
	if gr.Programs[0].Reservation != nil {
		t.Errorf("reservation should be null for non-EDCB backends")
	}
	// サブチャンネルは subchannels に含まれる
	if len(gr.Subchannels) != 1 {
		t.Fatalf("subchannels = %d, want 1", len(gr.Subchannels))
	}
	if gr.Subchannels[0].Channel.ID != "NID32736-SID1025" {
		t.Errorf("subchannel = %q", gr.Subchannels[0].Channel.ID)
	}
	if len(gr.Subchannels[0].Programs) != 1 || gr.Subchannels[0].Programs[0].Title != "サブの番組" {
		t.Errorf("subchannel programs = %+v", gr.Subchannels[0].Programs)
	}
	// BS チャンネル
	if response.Channels[1].Channel.ID != "NID4-SID101" || len(response.Channels[1].Programs) != 1 {
		t.Errorf("BS channel = %+v", response.Channels[1])
	}
	// 視聴不可チャンネルは含まれない
	for _, entry := range response.Channels {
		if entry.Channel.ID == "NID32736-SID1099" {
			t.Error("unwatchable channel should not be included")
		}
	}
	// date_range は番組データの範囲になる
	if response.DateRange.Earliest == "" || response.DateRange.Latest == "" {
		t.Errorf("date_range = %+v", response.DateRange)
	}
}

// TestProgramTimeTableChannelType は channel_type による絞り込みを検証する。
func TestProgramTimeTableChannelType(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	timeTableTestSetup(t, server)

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/programs/timetable?channel_type=BS", "", "", "")
	var response timeTableResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Channels) != 1 || response.Channels[0].Channel.Type != "BS" {
		t.Errorf("channels = %+v", response.Channels)
	}
}

// TestProgramTimeTablePinned はピン留めチャンネルの指定順序を検証する。
func TestProgramTimeTablePinned(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	timeTableTestSetup(t, server)

	// BS → GR の順で指定する (指定順が優先される)
	recorder := doJSONRequest(
		t, handler, http.MethodGet,
		"/api/programs/timetable?pinned_channel_ids=NID4-SID101,NID32736-SID1024", "", "", "",
	)
	var response timeTableResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(response.Channels))
	}
	if response.Channels[0].Channel.ID != "NID4-SID101" || response.Channels[1].Channel.ID != "NID32736-SID1024" {
		t.Errorf("pinned order = %q / %q", response.Channels[0].Channel.ID, response.Channels[1].Channel.ID)
	}
	// ピン留め指定時はサブチャンネル指定がなければサブチャンネルは表示されない
	if len(response.Channels[0].Subchannels) != 0 && response.Channels[0].Subchannels != nil {
		t.Errorf("BS subchannels = %+v", response.Channels[0].Subchannels)
	}
}

// TestProgramTimeTableIndependentSubchannel は8時間ルールで独立するサブチャンネルを検証する。
func TestProgramTimeTableIndependentSubchannel(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	now := timeTableTestSetup(t, server)

	// サブチャンネルに8時間以上の番組を追加する (独立サブチャンネル扱いになる)
	insertTestProgram(t, server.db, "NID32736-SID1025-E2", "NID32736-SID1025", "長時間の番組", now.Add(2*time.Hour), now.Add(11*time.Hour), 30000.0)

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/programs/timetable?channel_type=GR", "", "", "")
	var response timeTableResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Channels) != 2 {
		t.Fatalf("channels = %d, want 2 (independent subchannel should be a separate column)", len(response.Channels))
	}
	if response.Channels[0].Subchannels != nil {
		t.Errorf("subchannels = %+v, want null", response.Channels[0].Subchannels)
	}
	// 独立サブチャンネルは service_id 順でメインチャンネルの次に並ぶ
	if response.Channels[1].Channel.ID != "NID32736-SID1025" {
		t.Errorf("second channel = %q, want the independent subchannel", response.Channels[1].Channel.ID)
	}
	if len(response.Channels[1].Programs) != 2 {
		t.Errorf("independent subchannel programs = %d, want 2", len(response.Channels[1].Programs))
	}
}

// TestProgramTimeTableEmpty はチャンネルがない場合の挙動を検証する。
func TestProgramTimeTableEmpty(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/programs/timetable", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response timeTableResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Channels) != 0 {
		t.Errorf("channels = %+v, want []", response.Channels)
	}
	if response.DateRange.Earliest == "" || response.DateRange.Latest == "" {
		t.Errorf("date_range should have default values: %+v", response.DateRange)
	}
}

// TestProgramTimeTableInvalidDatetime は不正な日時パラメーターを検証する。
func TestProgramTimeTableInvalidDatetime(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/programs/timetable?start_time=invalid", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", recorder.Code)
	}
}

// TestProgramSearchBackendCheck は番組検索 API が EDCB バックエンド専用であることを検証する。
func TestProgramSearchBackendCheck(t *testing.T) {
	server, _ := newTestServer(t, "")
	// ユーザー環境と同じ IPTV バックエンドにする
	server.config.General.Backend = "IPTV"
	handler := server.Handler()

	// テスト用の設定は IPTV バックエンドなので 422 になる
	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/programs/search", `{}`, "", "application/json")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "This API is only available when the backend is EDCB") {
		t.Errorf("detail = %s", recorder.Body.String())
	}
}

// TestProgramSearchProxiedForEDCB は EDCB バックエンド時に Python 版へプロキシされることを検証する。
func TestProgramSearchProxiedForEDCB(t *testing.T) {
	proxied := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total": 0, "programs": []}`))
	}))
	defer backend.Close()

	server, _ := newTestServer(t, backend.URL)
	server.config.General.Backend = "EDCB"

	recorder := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{}`, "", "application/json")
	if !proxied {
		t.Fatal("the search request should be proxied to the Python backend")
	}
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestTimeTableChannelSortMatchesPython は番組表のチャンネル並び替えが Python 版と一致することを検証する。
func TestTimeTableChannelSortMatchesPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "timetable_sort.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []struct {
			ID                string `json:"id"`
			Type              string `json:"type"`
			ChannelNumber     string `json:"channel_number"`
			ServiceID         int    `json:"service_id"`
			NetworkID         int    `json:"network_id"`
			TransportStreamID *int   `json:"transport_stream_id"`
		} `json:"rows"`
		SortedIDs []string `json:"sorted_ids"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	channels := make([]database.Channel, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		channels = append(channels, database.Channel{
			ID:                row.ID,
			Type:              row.Type,
			ChannelNumber:     row.ChannelNumber,
			ServiceID:         row.ServiceID,
			NetworkID:         row.NetworkID,
			TransportStreamID: row.TransportStreamID,
		})
	}
	sort.SliceStable(channels, func(i int, j int) bool {
		return compareTimeTableChannelSortKey(channels[i], channels[j]) < 0
	})

	actualIDs := make([]string, 0, len(channels))
	for _, channel := range channels {
		actualIDs = append(actualIDs, channel.ID)
	}
	if len(actualIDs) != len(fixture.SortedIDs) {
		t.Fatalf("sorted %d channels, want %d", len(actualIDs), len(fixture.SortedIDs))
	}
	for i := range fixture.SortedIDs {
		if actualIDs[i] != fixture.SortedIDs[i] {
			t.Errorf("sorted[%d] = %q, want %q\nactual:   %v\npython:   %v", i, actualIDs[i], fixture.SortedIDs[i], actualIDs, fixture.SortedIDs)
			break
		}
	}
}
