package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// insertTestRecordedProgramWithChannel はテスト用の録画番組を作成する (チャンネルなし) 。
func insertTestRecordedProgramWithChannel(
	t *testing.T, db *sql.DB, channelID *string, title string, startTime time.Time,
) int64 {
	t.Helper()
	var channel any
	if channelID != nil {
		channel = *channelID
	}
	result, err := db.Exec(`
		INSERT INTO recorded_programs (
			recording_start_margin, recording_end_margin, is_partially_recorded,
			channel_id, network_id, service_id, event_id, series_id, series_broadcast_period_id,
			title, series_title, episode_number, subtitle, description, detail,
			start_time, end_time, duration, is_free, genres,
			primary_audio_type, primary_audio_language, secondary_audio_type, secondary_audio_language,
			created_at, updated_at
		) VALUES (
			1.0, 1.0, 0, ?, 32736, 1024, 1, NULL, NULL,
			?, NULL, NULL, ?, '録画番組の説明', '{"字幕": "あり"}',
			?, ?, 3600.0, 1, '[{"major": "ドラマ", "middle": "国内ドラマ"}]',
			'2/0モード(ステレオ)', '日本語', NULL, NULL, ?, ?
		)
	`, channel, title, title+"のサブタイトル", formatTestDBTime(startTime), formatTestDBTime(startTime.Add(time.Hour)),
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

// insertTestRecordedVideoWithFile はテスト用の録画ファイルを作成する。
func insertTestRecordedVideoWithFile(
	t *testing.T, db *sql.DB, recordedProgramID int64, filePath string, fileHash string, status string,
) int64 {
	t.Helper()
	timestamp := formatTestDBTime(time.Now())
	result, err := db.Exec(`
		INSERT INTO recorded_videos (
			recorded_program_id, status, file_path, file_hash, file_size,
			file_created_at, file_modified_at, recording_start_time, recording_end_time, duration,
			container_format, video_codec, video_codec_profile, video_scan_type,
			video_frame_rate, video_resolution_width, video_resolution_height,
			primary_audio_codec, primary_audio_channel, primary_audio_sampling_rate,
			secondary_audio_codec, secondary_audio_channel, secondary_audio_sampling_rate,
			key_frames, cm_sections, created_at, updated_at, thumbnail_info, has_video_stream_changes
		) VALUES (
			?, ?, ?, ?, 1024,
			?, ?, ?, ?, 3600.0,
			'MPEG-TS', 'H.264', 'High', 'Progressive',
			29.97, 1920, 1080,
			'AAC-LC', 'Stereo', 48000,
			NULL, NULL, NULL,
			'[]', '[{"start_time": 10.0, "end_time": 20.0}]', ?, ?, '{"version": 1}', 0
		)
	`, recordedProgramID, status, filePath, fileHash, timestamp, timestamp, timestamp, timestamp, timestamp, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestVideosList は録画番組一覧 API の挙動を検証する。
func TestVideosList(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	// データがない場合は空のリストが返る
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/videos", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response recordedProgramsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 0 || len(response.RecordedPrograms) != 0 {
		t.Errorf("response = %+v", response)
	}

	// 録画番組を 2 件作成する
	channelID := "NID32736-SID1024"
	insertTestChannel(t, server.db, testChannel{
		ID: channelID, DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	now := time.Now()
	firstID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "古い番組", now.Add(-48*time.Hour))
	insertTestRecordedVideoWithFile(t, server.db, firstID, "/recorded/old.ts", "hash_old", "Recorded")
	secondID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "新しい番組", now.Add(-24*time.Hour))
	insertTestRecordedVideoWithFile(t, server.db, secondID, "/recorded/new.ts", "hash_new", "Recorded")

	// デフォルト (降順) では新しい番組が先に返る
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 || len(response.RecordedPrograms) != 2 {
		t.Fatalf("response = %+v", response)
	}
	if response.RecordedPrograms[0].ID != secondID || response.RecordedPrograms[1].ID != firstID {
		t.Errorf("the order is wrong: %+v", response.RecordedPrograms)
	}
	// チャンネル情報と録画ファイル情報が展開されている
	program := response.RecordedPrograms[0]
	if program.Channel == nil || program.Channel.Name != "NHK総合1・東京" {
		t.Errorf("channel = %+v", program.Channel)
	}
	if program.RecordedVideo == nil || program.RecordedVideo.FileHash != "hash_new" {
		t.Errorf("recorded_video = %+v", program.RecordedVideo)
	}
	if program.Title != "新しい番組" {
		t.Errorf("title = %q", program.Title)
	}

	// 昇順
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos?order=asc", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RecordedPrograms[0].ID != firstID {
		t.Errorf("the order is wrong: %+v", response.RecordedPrograms)
	}

	// ids を指定した場合は指定された ID のみが返る
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos?ids="+formatTestID(firstID), "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.RecordedPrograms) != 1 || response.RecordedPrograms[0].ID != firstID {
		t.Errorf("response = %+v", response)
	}

	// order=ids の場合は指定された順序が維持される
	url := "/api/videos?order=ids&ids=" + formatTestID(secondID) + "&ids=" + formatTestID(firstID)
	recorder = doJSONRequest(t, handler, http.MethodGet, url, "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.RecordedPrograms) != 2 || response.RecordedPrograms[0].ID != secondID || response.RecordedPrograms[1].ID != firstID {
		t.Errorf("response = %+v", response.RecordedPrograms)
	}

	// 不正なパラメータは 422
	for _, path := range []string{"/api/videos?order=invalid", "/api/videos?page=0", "/api/videos?ids=abc"} {
		recorder = doJSONRequest(t, handler, http.MethodGet, path, "", "", "")
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", path, recorder.Code)
		}
	}
}

// formatTestID はテスト用に ID を文字列化する。
func formatTestID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// newProxyTestBackend は未実装 API のプロキシ検証用のバックエンドサーバーを起動し、URL を返す。
func newProxyTestBackend(t *testing.T) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"python","path":"` + r.URL.Path + `"}`))
	}))
	t.Cleanup(backend.Close)
	return backend.URL
}

// TestVideosSearch は録画番組検索 API の挙動を検証する。
func TestVideosSearch(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	channelID := "NID32736-SID1024"
	insertTestChannel(t, server.db, testChannel{
		ID: channelID, DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	now := time.Now()
	firstID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "テストドラマ", now.Add(-48*time.Hour))
	insertTestRecordedVideoWithFile(t, server.db, firstID, "/recorded/1.ts", "hash1", "Recorded")
	secondID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "ABCニュース", now.Add(-24*time.Hour))
	insertTestRecordedVideoWithFile(t, server.db, secondID, "/recorded/2.ts", "hash2", "Recorded")

	// キーワード検索 (title の部分一致)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/videos/search?query=%E3%83%89%E3%83%A9%E3%83%9E", "", "", "")
	var response recordedProgramsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.RecordedPrograms) != 1 || response.RecordedPrograms[0].ID != firstID {
		t.Errorf("response = %+v", response)
	}

	// チャンネル名でも検索できる
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/search?query=NHK", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 {
		t.Errorf("total = %d, want 2", response.Total)
	}

	// 複数キーワードの AND 検索
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/search?query=ABC%20%E3%83%8B%E3%83%A5%E3%83%BC%E3%82%B9", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || response.RecordedPrograms[0].ID != secondID {
		t.Errorf("response = %+v", response)
	}

	// 一致しないキーワード
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/search?query=zzzz", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 0 || len(response.RecordedPrograms) != 0 {
		t.Errorf("response = %+v", response)
	}

	// クエリが空の場合は全件取得と同じ
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/search", "", "", "")
	response = recordedProgramsResponse{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 {
		t.Errorf("total = %d, want 2", response.Total)
	}
}

// TestVideoDetail は録画番組 API の挙動を検証する。
func TestVideoDetail(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	channelID := "NID32736-SID1024"
	insertTestChannel(t, server.db, testChannel{
		ID: channelID, DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	programID := insertTestRecordedProgramWithChannel(t, server.db, &channelID, "テスト番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, programID, "/recorded/test.ts", "hash_test", "Recorded")

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/videos/"+formatTestID(programID), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var program recordedProgramResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &program); err != nil {
		t.Fatal(err)
	}
	if program.ID != programID || program.Title != "テスト番組" {
		t.Errorf("program = %+v", program)
	}
	if program.RecordedVideo == nil || program.RecordedVideo.Status != "Recorded" {
		t.Errorf("recorded_video = %+v", program.RecordedVideo)
	}

	// 存在しない ID は 422
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/99999", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", recorder.Code)
	}
	// ID が数値でない場合も 422
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/abc", "", "", "")
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", recorder.Code)
	}
}

// TestVideoThumbnail は録画番組サムネイル画像取得 API の挙動を検証する。
func TestVideoThumbnail(t *testing.T) {
	server, paths := newTestServer(t, "")
	handler := server.Handler()

	// デフォルトのサムネイル画像を配置する
	defaultThumbnailPath := filepath.Join(paths.StaticDir, "thumbnails", "default.webp")
	if err := os.MkdirAll(filepath.Dir(defaultThumbnailPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultThumbnailPath, []byte("default-thumbnail"), 0o644); err != nil {
		t.Fatal(err)
	}

	programID := insertTestRecordedProgramWithChannel(t, server.db, nil, "テスト番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, programID, "/recorded/test.ts", "hash_test", "Recorded")

	// サムネイル画像が存在しない場合はデフォルトのサムネイル画像が返る (キャッシュさせない)
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/videos/"+formatTestID(programID)+"/thumbnail", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "default-thumbnail" {
		t.Errorf("body = %q", recorder.Body.String())
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store, no-cache, must-revalidate, proxy-revalidate" {
		t.Errorf("Cache-Control = %q", cacheControl)
	}

	// サムネイル画像が存在する場合は ETag 付きで返る
	thumbnailPath := filepath.Join(paths.ThumbnailsDir, "hash_test.webp")
	if err := os.MkdirAll(filepath.Dir(thumbnailPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbnailPath, []byte("thumbnail-body"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/videos/"+formatTestID(programID)+"/thumbnail", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Body.String() != "thumbnail-body" {
		t.Errorf("body = %q", recorder.Body.String())
	}
	etag := recorder.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("ETag is empty")
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "public, no-transform, immutable, max-age=2592000" {
		t.Errorf("Cache-Control = %q", cacheControl)
	}
	if lastModified := recorder.Header().Get("Last-Modified"); lastModified == "" {
		t.Errorf("Last-Modified is empty")
	}

	// If-None-Match が一致する場合は 304
	recorder = doJSONRequestWithHeaders(t, handler, http.MethodGet,
		"/api/videos/"+formatTestID(programID)+"/thumbnail", map[string]string{"If-None-Match": etag})
	if recorder.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", recorder.Body.String())
	}
	if contentLength := recorder.Header().Get("Content-Length"); contentLength != "" {
		t.Errorf("Content-Length = %q, want empty", contentLength)
	}

	// 一致しない If-None-Match の場合は 200
	recorder = doJSONRequestWithHeaders(t, handler, http.MethodGet,
		"/api/videos/"+formatTestID(programID)+"/thumbnail", map[string]string{"If-None-Match": `"invalid"`})
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}

	// If-Modified-Since が新しい場合は 304
	recorder = doJSONRequestWithHeaders(t, handler, http.MethodGet,
		"/api/videos/"+formatTestID(programID)+"/thumbnail",
		map[string]string{"If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)})
	if recorder.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", recorder.Code)
	}

	// 録画中の場合はデフォルトのサムネイル画像が返る
	recordingProgramID := insertTestRecordedProgramWithChannel(t, server.db, nil, "録画中の番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, recordingProgramID, "/recorded/recording.ts", "hash_test", "Recording")
	recorder = doJSONRequest(t, handler, http.MethodGet,
		"/api/videos/"+formatTestID(recordingProgramID)+"/thumbnail", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Body.String() != "default-thumbnail" {
		t.Errorf("body = %q", recorder.Body.String())
	}
}

// TestVideoDelete は録画ファイル削除 API の挙動を検証する。
func TestVideoDelete(t *testing.T) {
	server, paths := newTestServer(t, "")
	handler := server.Handler()

	// 録画ファイルとサムネイル・補助ファイルを実際に作成する
	recordedDirectory := filepath.Join(filepath.Dir(paths.ServerDir), "recorded")
	if err := os.MkdirAll(recordedDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(recordedDirectory, "test.ts")
	if err := os.WriteFile(filePath, []byte("recorded-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".program.txt", ".err"} {
		if err := os.WriteFile(filePath+suffix, []byte("auxiliary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.ThumbnailsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	thumbnailPaths := []string{
		filepath.Join(paths.ThumbnailsDir, "hash_test.webp"),
		filepath.Join(paths.ThumbnailsDir, "hash_test_tile.webp"),
	}
	for _, thumbnailPath := range thumbnailPaths {
		if err := os.WriteFile(thumbnailPath, []byte("thumbnail"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	programID := insertTestRecordedProgramWithChannel(t, server.db, nil, "テスト番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, programID, filePath, "hash_test", "Recorded")

	// 未ログインの場合は 401
	recorder := doJSONRequest(t, handler, http.MethodDelete, "/api/videos/"+formatTestID(programID), "", "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	// 一般ユーザーの場合は 403
	userID := createTestUser(t, server.db, "video_user", "password", false)
	userToken := issueTestToken(t, userID)
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/videos/"+formatTestID(programID), "", userToken, "")
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", recorder.Code)
	}

	// 管理者の場合は削除できる
	adminID := createTestUser(t, server.db, "video_admin", "password", true)
	adminToken := issueTestToken(t, adminID)
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/videos/"+formatTestID(programID), "", adminToken, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// データベースから削除されている
	var count int64
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM recorded_programs WHERE id = ?`, programID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("the recorded program was not deleted")
	}
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM recorded_videos WHERE recorded_program_id = ?`, programID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("the recorded video was not deleted")
	}

	// ファイルが削除されている
	for _, path := range append(thumbnailPaths, filePath, filePath+".program.txt", filePath+".err") {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not deleted", path)
		}
	}
}

// TestVideoJikkyoWithoutChannel はチャンネル情報がない録画番組の過去ログコメント API の挙動を検証する。
func TestVideoJikkyoWithoutChannel(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	programID := insertTestRecordedProgramWithChannel(t, server.db, nil, "テスト番組", time.Now())
	insertTestRecordedVideoWithFile(t, server.db, programID, "/recorded/test.ts", "hash_test", "Recorded")

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/videos/"+formatTestID(programID)+"/jikkyo", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		IsSuccess bool   `json:"is_success"`
		Comments  []any  `json:"comments"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.IsSuccess {
		t.Errorf("is_success = true, want false")
	}
	if response.Detail != "チャンネル情報または録画開始時刻/録画終了時刻の情報がない録画番組です。" {
		t.Errorf("detail = %q", response.Detail)
	}
	if response.Comments == nil || len(response.Comments) != 0 {
		t.Errorf("comments = %+v", response.Comments)
	}
}

// TestVideoReanalyzeIsHandledByGo はメタデータ再解析 API が Go で処理され、Python 版へプロキシされないことを検証する。
func TestVideoReanalyzeIsHandledByGo(t *testing.T) {
	backend := newProxyTestBackend(t)
	server, _ := newTestServer(t, backend)
	handler := server.Handler()

	for _, path := range []string{"/api/videos/1/reanalyze", "/api/videos/1/thumbnail/regenerate"} {
		recorder := doJSONRequest(t, handler, http.MethodPost, path, "", "", "")
		if strings.Contains(recorder.Body.String(), `"from":"python"`) {
			t.Errorf("%s: should not be proxied, body = %s", path, recorder.Body.String())
		}
		// 存在しない video_id は Python 版と同じ 422 を返す
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422, body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}
