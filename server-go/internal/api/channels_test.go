package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// testChannel はテスト用のチャンネル定義。
type testChannel struct {
	ID                string
	DisplayChannelID  string
	NetworkID         int
	ServiceID         int
	TransportStreamID *int
	RemoconID         int
	ChannelNumber     string
	Type              string
	Name              string
	IsSubchannel      bool
	IsRadiochannel    bool
	IsWatchable       bool
}

// insertTestChannel はテスト用のチャンネルを挿入する。
func insertTestChannel(t *testing.T, db *sql.DB, channel testChannel) {
	t.Helper()
	isWatchable := 0
	if channel.IsWatchable {
		isWatchable = 1
	}
	isSubchannel := 0
	if channel.IsSubchannel {
		isSubchannel = 1
	}
	isRadiochannel := 0
	if channel.IsRadiochannel {
		isRadiochannel = 1
	}
	_, err := db.Exec(`
		INSERT INTO channels (
			id, display_channel_id, network_id, service_id, transport_stream_id,
			remocon_id, channel_number, type, name, jikkyo_force,
			is_subchannel, is_radiochannel, is_watchable
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
	`,
		channel.ID, channel.DisplayChannelID, channel.NetworkID, channel.ServiceID, channel.TransportStreamID,
		channel.RemoconID, channel.ChannelNumber, channel.Type, channel.Name,
		isSubchannel, isRadiochannel, isWatchable,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// insertTestProgram はテスト用の番組を挿入する。
func insertTestProgram(t *testing.T, db *sql.DB, id string, channelID string, title string, startTime time.Time, endTime time.Time, duration float64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO programs (
			id, channel_id, network_id, service_id, event_id, title, description,
			detail, start_time, end_time, duration, is_free, genres,
			video_type, video_codec, video_resolution,
			primary_audio_type, primary_audio_language, primary_audio_sampling_rate,
			secondary_audio_type, secondary_audio_language, secondary_audio_sampling_rate
		) VALUES (?, ?, 32736, 1024, 1, ?, 'テスト番組の説明', '{"テスト": "値"}', ?, ?, ?, 1,
			'[{"major": "ニュース／報道", "middle": "国内"}]',
			'映像', 'H.264', '1080i',
			'音声', '日本語', '48kHz', NULL, NULL, NULL
		)
	`, id, channelID, title, formatTestDBTime(startTime), formatTestDBTime(endTime), duration)
	if err != nil {
		t.Fatal(err)
	}
}

// formatTestDBTime は Tortoise ORM 互換の日時文字列に変換する。
func formatTestDBTime(value time.Time) string {
	return value.In(constants.JST).Format("2006-01-02 15:04:05.000000-07:00")
}

// TestChannelDetail はチャンネル情報 API の挙動を検証する。
func TestChannelDetail(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	now := time.Now()
	// 現在放送中の番組を2つ挿入し、より開始時刻が遅い方が選ばれることを確認する
	insertTestProgram(t, server.db, "NID32736-SID1024-E1", "NID32736-SID1024", "古い番組", now.Add(-50*time.Minute), now.Add(10*time.Minute), 3600.0)
	insertTestProgram(t, server.db, "NID32736-SID1024-E2", "NID32736-SID1024", "現在の番組", now.Add(-20*time.Minute), now.Add(30*time.Minute), 3000.0)
	insertTestProgram(t, server.db, "NID32736-SID1024-E3", "NID32736-SID1024", "次の番組", now.Add(5*time.Minute), now.Add(65*time.Minute), 3600.0)
	insertTestProgram(t, server.db, "NID32736-SID1024-E4", "NID32736-SID1024", "その次の番組", now.Add(70*time.Minute), now.Add(130*time.Minute), 3600.0)

	// ***** display_channel_id で取得できる *****
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr011", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "NID32736-SID1024" || response["display_channel_id"] != "gr011" {
		t.Errorf("unexpected channel: %v", response)
	}
	// 地デジチャンネルは地域名が入る
	regions, ok := response["terrestrial_regions"].([]any)
	if !ok || len(regions) != 7 || regions[0] != "茨城県" {
		t.Errorf("terrestrial_regions = %v", response["terrestrial_regions"])
	}
	if response["is_display"] != true || response["viewer_count"] != float64(0) {
		t.Errorf("is_display/viewer_count = %v/%v", response["is_display"], response["viewer_count"])
	}

	// 現在の番組は start_time が一番遅いもの
	present, ok := response["program_present"].(map[string]any)
	if !ok || present["title"] != "現在の番組" {
		t.Errorf("program_present = %v", response["program_present"])
	}
	// 次の番組は start_time が一番早いもの
	following, ok := response["program_following"].(map[string]any)
	if !ok || following["title"] != "次の番組" {
		t.Errorf("program_following = %v", response["program_following"])
	}
	// detail / genres は JSON としてデコードされ、duration は Pydantic v2 互換の小数表記になる
	if detail, ok := present["detail"].(map[string]any); !ok || detail["テスト"] != "値" {
		t.Errorf("detail = %v", present["detail"])
	}
	if genres, ok := present["genres"].([]any); !ok || len(genres) != 1 {
		t.Errorf("genres = %v", present["genres"])
	}
	if present["duration"] != 3000.0 {
		t.Errorf("duration = %v (%T)", present["duration"], present["duration"])
	}
	if !strings.Contains(recorder.Body.String(), `"duration":3000.0`) {
		t.Errorf("duration should be serialized as 3000.0: %s", recorder.Body.String())
	}
	// 日時は Pydantic v2 と同じ ISO8601 形式 (マイクロ秒 6 桁 + オフセット) になる
	startTime, _ := present["start_time"].(string)
	if len(startTime) != len("2026-09-22T15:11:50.725472+09:00") || startTime[10] != 'T' {
		t.Errorf("start_time = %q", startTime)
	}
	if present["is_free"] != true {
		t.Errorf("is_free = %v", present["is_free"])
	}

	// ***** id でも取得できる *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/NID32736-SID1024", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Errorf("get by id: status = %d", recorder.Code)
	}

	// ***** 存在しないチャンネルは 422 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr999", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified display_channel_id was not found") {
		t.Errorf("unknown channel: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestChannelDetailSubchannel はサブチャンネルの is_display 判定を検証する。
func TestChannelDetailSubchannel(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1025", DisplayChannelID: "gr012", NetworkID: 32736, ServiceID: 1025,
		RemoconID: 1, ChannelNumber: "012", Type: "GR", Name: "NHK総合1・東京 (サブ)", IsSubchannel: true, IsWatchable: true,
	})

	// 現在の番組がないサブチャンネルは is_display が false になる
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr012", "", "", "")
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["is_display"] != false {
		t.Errorf("is_display = %v, want false", response["is_display"])
	}
	if response["program_present"] != nil || response["program_following"] != nil {
		t.Errorf("program = %v / %v, want null", response["program_present"], response["program_following"])
	}

	// 現在の番組があるサブチャンネルは is_display が true になる
	now := time.Now()
	insertTestProgram(t, server.db, "NID32736-SID1025-E1", "NID32736-SID1025", "サブチャンネルの番組", now.Add(-10*time.Minute), now.Add(10*time.Minute), 1200.0)
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr012", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["is_display"] != true {
		t.Errorf("is_display = %v, want true", response["is_display"])
	}
}

// TestChannelLogo はチャンネルロゴ API の挙動を検証する。
func TestChannelLogo(t *testing.T) {
	server, paths := newTestServer(t, "")
	handler := server.Handler()

	// デフォルトのロゴを配置する
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "default.png"), "default-logo-bytes")
	// 同梱ロゴを配置する
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "NID32736-SID1024.png"), "nhk-logo-bytes")
	// コミュニティチャンネルのロゴを配置する
	communityDir := filepath.Join(paths.StaticDir, "logos", "community-channels")
	if err := os.MkdirAll(communityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(communityDir, "J：COMテレビ.png"), "jcom-logo-bytes")

	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	// ロゴが存在しないチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32737-SID1032", DisplayChannelID: "gr021", NetworkID: 32737, ServiceID: 1032,
		RemoconID: 2, ChannelNumber: "021", Type: "GR", Name: "NHK Eテレ1・東京", IsWatchable: true,
	})
	// コミュニティチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32740-SID1050", DisplayChannelID: "gr091", NetworkID: 32740, ServiceID: 1050,
		RemoconID: 9, ChannelNumber: "091", Type: "GR", Name: "J:COMテレビ", IsWatchable: true,
	})

	// ***** NID0-SID0 / gr000 はデフォルトのロゴ *****
	for _, channelID := range []string{"NID0-SID0", "gr000"} {
		recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/"+channelID+"/logo", "", "", "")
		if recorder.Code != http.StatusOK || recorder.Body.String() != "default-logo-bytes" {
			t.Errorf("%s: status = %d, body = %q", channelID, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("ETag") != sha256HexString("default") {
			t.Errorf("%s: ETag = %q", channelID, recorder.Header().Get("ETag"))
		}
		if recorder.Header().Get("Cache-Control") != "public, no-transform, immutable, max-age=2592000" {
			t.Errorf("%s: Cache-Control = %q", channelID, recorder.Header().Get("Cache-Control"))
		}
	}

	// ***** 同梱のロゴが返る *****
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr011/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "nhk-logo-bytes" {
		t.Fatalf("bundled logo: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	logoPath := filepath.Join(paths.StaticDir, "logos", "NID32736-SID1024.png")
	expectedETag := sha256HexString(logoPath + constants.Version)
	if recorder.Header().Get("ETag") != expectedETag {
		t.Errorf("ETag = %q, want %q", recorder.Header().Get("ETag"), expectedETag)
	}
	if recorder.Header().Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}

	// ***** If-None-Match が一致すると 304 *****
	request := httptest.NewRequest(http.MethodGet, "/api/channels/gr011/logo", nil)
	request.Header.Set("If-None-Match", expectedETag)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Errorf("304: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	// ***** ロゴがないチャンネルはデフォルトのロゴ *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr021/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "default-logo-bytes" {
		t.Errorf("fallback: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("ETag") != sha256HexString("default") {
		t.Errorf("fallback: ETag = %q", recorder.Header().Get("ETag"))
	}

	// ***** コミュニティチャンネルはチャンネル名で決め打ち *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr091/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "jcom-logo-bytes" {
		t.Errorf("community: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	// ***** IPTV 疑似チャンネルはプロキシが無効な場合は既定のロゴ (IPTV 用の ETag) *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/IPTV-gr999/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "default-logo-bytes" {
		t.Errorf("iptv: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("ETag") != sha256HexString("iptv-default"+constants.Version) {
		t.Errorf("iptv: ETag = %q", recorder.Header().Get("ETag"))
	}

	// ***** 存在しないチャンネルは 422 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr999/logo", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown channel: status = %d", recorder.Code)
	}
}

// TestChannelLogoSubchannel はサブチャンネルのロゴがメインチャンネルのロゴになることを検証する。
func TestChannelLogoSubchannel(t *testing.T) {
	server, paths := newTestServer(t, "")
	handler := server.Handler()
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "default.png"), "default-logo-bytes")
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "NID32736-SID1024.png"), "nhk-logo-bytes")

	// メインチャンネル (サービス ID が一番若い)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	// サブチャンネル (メインチャンネルのロゴを利用する)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1025", DisplayChannelID: "gr012", NetworkID: 32736, ServiceID: 1025,
		RemoconID: 1, ChannelNumber: "012", Type: "GR", Name: "NHK総合2・東京", IsSubchannel: true, IsWatchable: true,
	})

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/gr012/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "nhk-logo-bytes" {
		t.Errorf("gr subchannel: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	// BS サブチャンネル (service_id 102 → 101)
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "NID4-SID101.png"), "bs-logo-bytes")
	insertTestChannel(t, server.db, testChannel{
		ID: "NID4-SID101", DisplayChannelID: "bs101", NetworkID: 4, ServiceID: 101,
		RemoconID: 1, ChannelNumber: "101", Type: "BS", Name: "NHK BS1", IsWatchable: true,
	})
	insertTestChannel(t, server.db, testChannel{
		ID: "NID4-SID102", DisplayChannelID: "bs102", NetworkID: 4, ServiceID: 102,
		RemoconID: 1, ChannelNumber: "102", Type: "BS", Name: "NHK BS1 (サブ)", IsSubchannel: true, IsWatchable: true,
	})
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/bs102/logo", "", "", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "bs-logo-bytes" {
		t.Errorf("bs subchannel: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestBuildProgramResponseInvalidJSON は programs.detail / genres が壊れている場合にエラーになることを検証する。
func TestBuildProgramResponseInvalidJSON(t *testing.T) {
	_, err := buildProgramResponse(&database.Program{ID: "broken", Detail: "{", Genres: "[]"})
	if err == nil {
		t.Error("invalid detail should be an error")
	}
	_, err = buildProgramResponse(&database.Program{ID: "broken", Detail: "{}", Genres: "["})
	if err == nil {
		t.Error("invalid genres should be an error")
	}
}
