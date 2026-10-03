package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// insertTestSeries はテスト用のシリーズ番組を作成し、ID を返す。
func insertTestSeries(t *testing.T, db *sql.DB, title string, description string, updatedAt time.Time) int64 {
	t.Helper()
	timestamp := formatTestDBTime(updatedAt)
	result, err := db.Exec(`
		INSERT INTO series (title, description, genres, created_at, updated_at)
		VALUES (?, ?, '[{"major": "ドラマ", "middle": "国内ドラマ"}]', ?, ?)
	`, title, description, timestamp, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertTestBroadcastPeriod はテスト用の放送期間を作成し、ID を返す。
func insertTestBroadcastPeriod(t *testing.T, db *sql.DB, seriesID int64, channelID string, startDate string, endDate string) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO series_broadcast_periods (series_id, channel_id, start_date, end_date)
		VALUES (?, ?, ?, ?)
	`, seriesID, channelID, startDate, endDate)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertTestRecordedProgram はテスト用の録画番組を作成し、ID を返す。
func insertTestRecordedProgram(
	t *testing.T, db *sql.DB, seriesID int64, periodID int64, channelID string, title string, startTime time.Time,
) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO recorded_programs (
			recording_start_margin, recording_end_margin, is_partially_recorded,
			channel_id, network_id, service_id, event_id, series_id, series_broadcast_period_id,
			title, series_title, episode_number, subtitle, description, detail,
			start_time, end_time, duration, is_free, genres,
			primary_audio_type, primary_audio_language, secondary_audio_type, secondary_audio_language,
			created_at, updated_at
		) VALUES (
			1.0, 1.0, 0, ?, 32736, 1024, 1, ?, ?, ?, 'テストシリーズ', '#1', 'サブタイトル', '録画番組の説明', '{"字幕": "あり"}',
			?, ?, 3600.0, 1, '[{"major": "ドラマ", "middle": "国内ドラマ"}]',
			'2/0モード(ステレオ)', '日本語', NULL, NULL, ?, ?
		)
	`, channelID, seriesID, periodID, title, formatTestDBTime(startTime), formatTestDBTime(startTime.Add(time.Hour)),
		formatTestDBTime(startTime), formatTestDBTime(startTime))
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertTestRecordedVideo はテスト用の録画ファイルを作成する。
func insertTestRecordedVideo(t *testing.T, db *sql.DB, recordedProgramID int64) {
	t.Helper()
	timestamp := formatTestDBTime(time.Now())
	_, err := db.Exec(`
		INSERT INTO recorded_videos (
			recorded_program_id, status, file_path, file_hash, file_size,
			file_created_at, file_modified_at, recording_start_time, recording_end_time, duration,
			container_format, video_codec, video_codec_profile, video_scan_type,
			video_frame_rate, video_resolution_width, video_resolution_height,
			primary_audio_codec, primary_audio_channel, primary_audio_sampling_rate,
			secondary_audio_codec, secondary_audio_channel, secondary_audio_sampling_rate,
			key_frames, cm_sections, created_at, updated_at, thumbnail_info, has_video_stream_changes
		) VALUES (
			?, 'Recorded', '/recorded/test.ts', 'hash123', 1024,
			?, ?, ?, ?, 3600.0,
			'MPEG-TS', 'H.264', 'High', 'Progressive',
			29.97, 1920, 1080,
			'AAC-LC', 'Stereo', 48000,
			NULL, NULL, NULL,
			'[]', '[{"start_time": 10.0, "end_time": 20.0}]', ?, ?, '{"version": 1}', 0
		)
	`, recordedProgramID, timestamp, timestamp, timestamp, timestamp, timestamp, timestamp)
	if err != nil {
		t.Fatal(err)
	}
}

// setupSeriesTestData はシリーズ番組テスト用のデータを作成する。
func setupSeriesTestData(t *testing.T, server *Server) (seriesID int64, channelID string) {
	t.Helper()
	channelID = "NID32736-SID1024"
	insertTestChannel(t, server.db, testChannel{
		ID: channelID, DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})

	now := time.Now()
	seriesID = insertTestSeries(t, server.db, "テストドラマ", "テストドラマの説明", now)
	periodID := insertTestBroadcastPeriod(t, server.db, seriesID, channelID, "2026-09-01", "2026-09-30")
	programID := insertTestRecordedProgram(t, server.db, seriesID, periodID, channelID, "テストドラマ #1", now.Add(-24*time.Hour))
	insertTestRecordedVideo(t, server.db, programID)

	// 検索用に別のシリーズも作成する
	otherSeriesID := insertTestSeries(t, server.db, "ABCニュース", "ニュースの説明", now.Add(-time.Hour))
	otherPeriodID := insertTestBroadcastPeriod(t, server.db, otherSeriesID, channelID, "2026-10-01", "2026-10-31")
	otherProgramID := insertTestRecordedProgram(t, server.db, otherSeriesID, otherPeriodID, channelID, "ABCニュース #1", now.Add(-48*time.Hour))
	insertTestRecordedVideo(t, server.db, otherProgramID)
	return seriesID, channelID
}

// TestSeriesList はシリーズ番組一覧 API の挙動を検証する。
func TestSeriesList(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	seriesID, channelID := setupSeriesTestData(t, server)

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/series", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response seriesListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 || len(response.SeriesList) != 2 {
		t.Fatalf("total/list = %d/%d, want 2/2", response.Total, len(response.SeriesList))
	}
	// デフォルトは updated_at の降順
	if response.SeriesList[0].ID != seriesID {
		t.Errorf("first series = %d, want %d", response.SeriesList[0].ID, seriesID)
	}

	// 放送期間・録画番組・録画ファイルが展開される
	series := response.SeriesList[0]
	if series.Title != "テストドラマ" || len(series.BroadcastPeriods) != 1 {
		t.Fatalf("series = %+v", series)
	}
	period := series.BroadcastPeriods[0]
	if period.Channel == nil || period.Channel.ID != channelID {
		t.Errorf("period channel = %+v", period.Channel)
	}
	if period.StartDate != "2026-09-01" || period.EndDate != "2026-09-30" {
		t.Errorf("period dates = %q / %q", period.StartDate, period.EndDate)
	}
	if len(period.RecordedPrograms) != 1 {
		t.Fatalf("recorded programs = %d, want 1", len(period.RecordedPrograms))
	}
	program := period.RecordedPrograms[0]
	if program.Title != "テストドラマ #1" || program.SeriesTitle == nil || *program.SeriesTitle != "テストシリーズ" {
		t.Errorf("program = %+v", program)
	}
	if program.RecordedVideo == nil || program.RecordedVideo.FileHash != "hash123" {
		t.Fatalf("recorded video = %+v", program.RecordedVideo)
	}
	if program.RecordedVideo.Duration != 3600.0 || program.RecordedVideo.VideoFrameRate != 29.97 {
		t.Errorf("video duration/frame rate = %v / %v", program.RecordedVideo.Duration, program.RecordedVideo.VideoFrameRate)
	}
	// JSON は Pydantic v2 と同じくコンパクトに再直列化される
	if string(program.RecordedVideo.CMSections) != `[{"start_time":10.0,"end_time":20.0}]` {
		t.Errorf("cm_sections = %s", program.RecordedVideo.CMSections)
	}
	if string(program.RecordedVideo.ThumbnailInfo) != `{"version":1}` {
		t.Errorf("thumbnail_info = %s", program.RecordedVideo.ThumbnailInfo)
	}

	// ***** 昇順ソート *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series?order=asc", "", "", "")
	var ascending seriesListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &ascending); err != nil {
		t.Fatal(err)
	}
	if ascending.SeriesList[0].ID == seriesID {
		t.Error("order=asc should return the oldest series first")
	}

	// ***** 不正な order は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series?order=invalid", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("order=invalid: status = %d, want 422", recorder.Code)
	}

	// ***** 2ページ目は空 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series?page=2", "", "", "")
	var secondPage seriesListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.SeriesList) != 0 || secondPage.Total != 2 {
		t.Errorf("page 2 = %+v", secondPage)
	}
}

// TestSeriesSearch はシリーズ番組検索 API の挙動を検証する。
func TestSeriesSearch(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	setupSeriesTestData(t, server)

	// ***** 日本語の部分一致 *****
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/series/search?query="+urlEncode("ドラマ"), "", "", "")
	var response seriesListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || response.SeriesList[0].Title != "テストドラマ" {
		t.Errorf("search ドラマ = %+v", response)
	}

	// ***** ASCII の大文字小文字を区別しない検索 (UPPER LIKE) *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series/search?query=abc", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || response.SeriesList[0].Title != "ABCニュース" {
		t.Errorf("search abc = %+v", response)
	}

	// ***** description でも検索できる *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series/search?query="+urlEncode("ニュースの説明"), "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 {
		t.Errorf("search description = %+v", response)
	}

	// ***** LIKE のワイルドカードはエスケープされる *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series/search?query=%25", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 0 {
		t.Errorf("search %% = %+v, want 0 results", response)
	}

	// ***** クエリが空の場合は全件 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series/search", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 {
		t.Errorf("empty query = %+v", response)
	}
}

// TestSeriesDetail はシリーズ番組 API (単体取得) の挙動を検証する。
func TestSeriesDetail(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()
	seriesID, _ := setupSeriesTestData(t, server)

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/series/"+itoa(seriesID), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response seriesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != seriesID || response.Title != "テストドラマ" {
		t.Errorf("series = %+v", response)
	}
	if len(response.BroadcastPeriods) != 1 || len(response.BroadcastPeriods[0].RecordedPrograms) != 1 {
		t.Errorf("broadcast periods = %+v", response.BroadcastPeriods)
	}

	// ***** 存在しない ID は 422 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/series/99999", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Specified series_id was not found") {
		t.Errorf("unknown series: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// urlEncode はクエリパラメーター用に文字列をエンコードする。
func urlEncode(value string) string {
	encoded := ""
	for _, character := range []byte(value) {
		encoded += "%" + strings.ToUpper(hexByte(character))
	}
	return encoded
}

// hexByte は 1 バイトを 16 進数 2 桁の文字列にする。
func hexByte(value byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[value>>4], digits[value&0x0F]})
}

// itoa は int64 を文字列にする。
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(rune('0'+value%10)) + result
		value /= 10
	}
	return result
}
