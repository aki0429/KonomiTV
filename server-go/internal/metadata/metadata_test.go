package metadata

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"

	_ "modernc.org/sqlite"
)

// testLogger はテスト中にログを出力しない slog.Logger を返す。
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testSchema は録画メタデータ解析に必要な最小限のテーブル定義。
// server-go/internal/api/testutil_test.go のスキーマのうち該当部分を抜き出したもの。
const testSchema = `
CREATE TABLE channels (
    id VARCHAR(255) NOT NULL PRIMARY KEY,
    display_channel_id VARCHAR(255) NOT NULL UNIQUE,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    transport_stream_id INT,
    remocon_id INT NOT NULL,
    channel_number VARCHAR(255) NOT NULL,
    type VARCHAR(255) NOT NULL,
    name TEXT NOT NULL,
    jikkyo_force INT,
    is_subchannel INT NOT NULL,
    is_radiochannel INT NOT NULL,
    is_watchable INT NOT NULL
);
CREATE TABLE recorded_programs (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    recording_start_margin REAL NOT NULL,
    recording_end_margin REAL NOT NULL,
    is_partially_recorded INT NOT NULL,
    channel_id VARCHAR(255) REFERENCES channels (id) ON DELETE CASCADE,
    network_id INT, service_id INT, event_id INT, series_id INT, series_broadcast_period_id INT,
    title TEXT NOT NULL, series_title TEXT, episode_number VARCHAR(255), subtitle TEXT,
    description TEXT NOT NULL, detail JSON NOT NULL,
    start_time TIMESTAMP NOT NULL, end_time TIMESTAMP NOT NULL, duration REAL NOT NULL,
    is_free INT NOT NULL, genres JSON NOT NULL,
    primary_audio_type TEXT NOT NULL, primary_audio_language TEXT NOT NULL,
    secondary_audio_type TEXT, secondary_audio_language TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    is_series_manually_edited INT NOT NULL DEFAULT 0
);
CREATE TABLE recorded_videos (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    recorded_program_id INT NOT NULL REFERENCES recorded_programs (id) ON DELETE CASCADE,
    status VARCHAR(255) NOT NULL, file_path TEXT NOT NULL, file_hash TEXT NOT NULL, file_size INT NOT NULL,
    file_created_at TIMESTAMP NOT NULL, file_modified_at TIMESTAMP NOT NULL,
    recording_start_time TIMESTAMP, recording_end_time TIMESTAMP, duration REAL NOT NULL,
    container_format VARCHAR(255) NOT NULL, video_codec VARCHAR(255) NOT NULL,
    video_codec_profile VARCHAR(255) NOT NULL, video_scan_type VARCHAR(255) NOT NULL,
    video_frame_rate REAL NOT NULL, video_resolution_width INT NOT NULL, video_resolution_height INT NOT NULL,
    primary_audio_codec VARCHAR(255) NOT NULL, primary_audio_channel VARCHAR(255) NOT NULL,
    primary_audio_sampling_rate INT NOT NULL, secondary_audio_codec VARCHAR(255),
    secondary_audio_channel VARCHAR(255), secondary_audio_sampling_rate INT,
    key_frames JSON NOT NULL, cm_sections JSON,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    thumbnail_info JSON, segment_map JSON NOT NULL DEFAULT '[]',
    has_video_stream_changes INT NOT NULL DEFAULT 0
);
`

// openTestDB はテスト用の一時 SQLite データベースを開く。
func openTestDB(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}
	fixed := time.Date(2026, 10, 7, 12, 0, 0, 0, constants.JST)
	return &Store{DB: db, WriteDB: db, Now: func() time.Time { return fixed }}
}

// fakeAnalyzer は実プロセスを起動せずに固定の解析結果を返すフェイク。
type fakeAnalyzer struct {
	program *RecordedProgram
	err     error
}

func (f *fakeAnalyzer) Analyze(ctx context.Context, path string) (*RecordedProgram, error) {
	return f.program, f.err
}

// buildTestProgram はテスト用の録画番組情報を組み立てる。
func buildTestProgram(filePath string, durationSec float64) *RecordedProgram {
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	end := start.Add(time.Duration(durationSec * float64(time.Second)))
	return &RecordedProgram{
		Title:       "テスト番組",
		Description: "テスト用の番組概要",
		Detail:      []DetailEntry{{Key: "出演者", Value: "テスト"}},
		StartTime:   start,
		EndTime:     end,
		Duration:    durationSec,
		IsFree:      true,
		Genres:      []Genre{{Major: "ニュース・報道", Middle: "定時ニュース"}},
		Video: RecordedVideo{
			Status:                   "Recorded",
			FilePath:                 filePath,
			FileHash:                 "0123456789abcdef0123456789abcdef",
			FileSize:                 4096,
			FileCreatedAt:            start,
			FileModifiedAt:           end,
			Duration:                 durationSec,
			ContainerFormat:          "MPEG-TS",
			VideoCodec:               "MPEG-2",
			VideoCodecProfile:        "Main",
			VideoScanType:            "Interlaced",
			VideoFrameRate:           29.97,
			VideoResolutionWidth:     1440,
			VideoResolutionHeight:    1080,
			PrimaryAudioCodec:        "AAC-LC",
			PrimaryAudioChannel:      "Stereo",
			PrimaryAudioSamplingRate: 48000,
		},
	}
}

func TestClosestMultiple(t *testing.T) {
	cases := []struct {
		n        int64
		multiple int64
		want     int64
	}{
		{0, 188, 0},
		{100, 188, 188},        // 0.53 → 1 (偶数丸めではなく最近接)
		{94, 188, 0},           // 0.5 → 0 (banker's rounding)
		{282, 188, 376},        // 1.5 → 2
		{470, 188, 376},        // 2.5 → 2
		{1365, 188, 1316},      // 7.26 → 7
		{1000000, 188, 999972}, // 5319.1 → 5319
	}
	for _, tc := range cases {
		if got := closestMultiple(tc.n, tc.multiple); got != tc.want {
			t.Errorf("closestMultiple(%d, %d) = %d, want %d", tc.n, tc.multiple, got, tc.want)
		}
	}
}

func TestParseFPS(t *testing.T) {
	cases := []struct {
		input *string
		want  *float64
	}{
		{ptr("30000/1001"), ptr(29.97)},
		{ptr("30/1"), ptr(30.0)},
		{ptr("24"), ptr(24.0)},
		{ptr("0/0"), nil},
		{ptr("abc"), nil},
		{nil, nil},
	}
	for _, tc := range cases {
		got := parseFPS(tc.input)
		if tc.want == nil {
			if got != nil {
				t.Errorf("parseFPS(%v) = %v, want nil", tc.input, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseFPS(%v) = nil, want %v", *tc.input, *tc.want)
			continue
		}
		if *got != *tc.want {
			t.Errorf("parseFPS(%v) = %v, want %v", *tc.input, *got, *tc.want)
		}
	}
}

func ptr[T any](value T) *T { return &value }

func TestCalculateThumbnailLayout(t *testing.T) {
	// 期待値は Python 版 ThumbnailGenerator.__calculateBaseTileInterval() /
	// __calculateTileLayout() を実行して得た値と一致する。
	cases := []struct {
		duration float64
		interval float64
		cols     int
		rows     int
		total    int
		width    int
		height   int
	}{
		{600, 5.0, 51, 3, 120, 16320, 540},
		{1800, 5.0, 51, 8, 360, 16320, 1440},
		{3600, 10.0, 51, 8, 360, 16320, 1440},
		{5400, 11.3, 51, 10, 478, 16320, 1800},
		{7200, 12.6, 51, 12, 572, 16320, 2160},
		{86400, 30.0, 51, 57, 2880, 16320, 10260},
	}
	for _, tc := range cases {
		got := CalculateThumbnailLayout(tc.duration)
		if got.TileIntervalSec != tc.interval || got.TileCols != tc.cols || got.TileRows != tc.rows ||
			got.TotalTiles != tc.total || got.TileImageWidth != tc.width || got.TileImageHeight != tc.height {
			t.Errorf("CalculateThumbnailLayout(%v) = %+v, want interval=%v cols=%d rows=%d total=%d %dx%d",
				tc.duration, got, tc.interval, tc.cols, tc.rows, tc.total, tc.width, tc.height)
		}
	}
}

func TestCalculateCandidateOffsets(t *testing.T) {
	layout := CalculateThumbnailLayout(600)
	offsets := CalculateCandidateOffsets(600, layout)
	if len(offsets) != layout.TotalTiles {
		t.Fatalf("offsets length = %d, want %d", len(offsets), layout.TotalTiles)
	}
	if offsets[0] != 0 {
		t.Errorf("offsets[0] = %v, want 0", offsets[0])
	}
	// 末尾は必ず動画長を超えないこと
	for index, offset := range offsets {
		if offset < 0 || offset+0.01 > 600 {
			t.Errorf("offsets[%d] = %v out of range", index, offset)
		}
	}
}

func TestFormatString(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ＡＢＣ１２３", "ABC123"},
		{"m^2", "m²"},
		{"[株]テスト", "㈱テスト"},
		{"プレーン", "プレーン"},
	}
	for _, tc := range cases {
		if got := formatString(tc.input); got != tc.want {
			t.Errorf("formatString(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCMSectionsJSON(t *testing.T) {
	got := CMSectionsJSON([]CMSection{{StartTime: 0.0, EndTime: 15.0}, {StartTime: 30.0, EndTime: 45.5}})
	want := `[{"start_time": 0.0, "end_time": 15.0}, {"start_time": 30.0, "end_time": 45.5}]`
	if got != want {
		t.Errorf("CMSectionsJSON = %q, want %q", got, want)
	}
	// 解析済みだが CM 区間がなかった場合は [] になる
	if got := CMSectionsJSON([]CMSection{}); got != "[]" {
		t.Errorf("CMSectionsJSON(empty) = %q, want []", got)
	}
}

func TestMarshalThumbnailInfo(t *testing.T) {
	layout := CalculateThumbnailLayout(3600)
	got, err := marshalThumbnailInfo(BuildThumbnailInfo(layout))
	if err != nil {
		t.Fatalf("marshalThumbnailInfo returned error: %v", err)
	}
	want := `{"version": 1, "representative": {"format": "WebP", "width": 480, "height": 270}, ` +
		`"tile": {"format": "WebP", "image_width": 16320, "image_height": 1440, "tile_width": 320, ` +
		`"tile_height": 180, "total_tiles": 360, "column_count": 51, "row_count": 8, "interval_sec": 10.0}}`
	if got != want {
		t.Errorf("marshalThumbnailInfo =\n%s\nwant\n%s", got, want)
	}
}

func TestParseGenresAndCMSections(t *testing.T) {
	genres, err := ParseGenres(`[{"major": "ニュース・報道", "middle": "定時ニュース"}]`)
	if err != nil {
		t.Fatalf("ParseGenres returned error: %v", err)
	}
	if len(genres) != 1 || genres[0].Major != "ニュース・報道" {
		t.Errorf("ParseGenres = %+v", genres)
	}
	// JSON null は未解析を表す
	if sections, err := ParseCMSections("null"); err != nil || len(sections) != 0 {
		t.Errorf("ParseCMSections(null) = %v, %v", sections, err)
	}
	sections, err := ParseCMSections(`[{"start_time": 30.0, "end_time": 45.0}, {"start_time": 0.0, "end_time": 15.0}]`)
	if err != nil {
		t.Fatalf("ParseCMSections returned error: %v", err)
	}
	if len(sections) != 2 || sections[0].StartTime != 0.0 || sections[1].StartTime != 30.0 {
		t.Errorf("ParseCMSections did not sort by start_time: %+v", sections)
	}
}

func TestDetectCMSectionsFromChapterFile(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "sample.ts")
	chapterPath := filepath.Join(directory, "sample.chapter.txt")
	if err := os.WriteFile(filePath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	chapter := "CHAPTER01=00:00:00.000\r\nCHAPTER01NAME=CM\r\nCHAPTER02=00:00:15.000\r\nCHAPTER02NAME=本編\r\n" +
		"CHAPTER03=01:00:00.000\r\nCHAPTER03NAME=CM\r\nCHAPTER04=01:00:30.000\r\nCHAPTER04NAME=本編\r\n"
	if err := os.WriteFile(chapterPath, []byte(chapter), 0o644); err != nil {
		t.Fatalf("failed to write chapter file: %v", err)
	}
	sections, ok := DetectCMSectionsFromChapterFile(filePath, 3700)
	if !ok {
		t.Fatalf("DetectCMSectionsFromChapterFile returned ok=false")
	}
	want := []CMSection{{StartTime: 0, EndTime: 15}, {StartTime: 3600, EndTime: 3630}}
	if len(sections) != len(want) {
		t.Fatalf("sections = %+v, want %+v", sections, want)
	}
	for index := range want {
		if sections[index] != want[index] {
			t.Errorf("sections[%d] = %+v, want %+v", index, sections[index], want[index])
		}
	}
}

func TestParseProbeResult(t *testing.T) {
	data := []byte(`{
		"streams": [
			{"index": 0, "codec_type": "video", "codec_name": "mpeg2video", "duration": "3600.5",
			 "profile": "Main@High", "width": 1440, "height": 1080, "avg_frame_rate": "30000/1001",
			 "r_frame_rate": "30000/1001", "field_order": "tt", "ts_packetsize": "188"},
			{"index": 1, "codec_type": "audio", "codec_name": "aac", "duration": "3600.5",
			 "profile": "LC", "channels": 2, "sample_rate": "48000", "ts_packetsize": "188"}
		],
		"format": {"format_name": "mpegts", "duration": "3600.5", "size": "100", "bit_rate": "100"},
		"programs": [{"program_num": 1, "nb_streams": 2, "pcr_pid": 256, "program_id": 1, "pmt_pid": 100}]
	}`)
	result, err := ParseProbeResult(data)
	if err != nil {
		t.Fatalf("ParseProbeResult returned error: %v", err)
	}
	if result.Format.FormatName != "mpegts" || result.Format.Duration == nil || *result.Format.Duration != "3600.5" {
		t.Errorf("unexpected format: %+v", result.Format)
	}
	videos := videoStreamsOf(result.Streams)
	audios := audioStreamsOf(result.Streams)
	if len(videos) != 1 || len(audios) != 1 {
		t.Fatalf("streams parsed incorrectly: %d video, %d audio", len(videos), len(audios))
	}
	if videos[0].CodecName != "mpeg2video" || videos[0].Width != 1440 {
		t.Errorf("unexpected video stream: %+v", videos[0])
	}
	if audios[0].Channels != 2 || audios[0].SampleRate != "48000" {
		t.Errorf("unexpected audio stream: %+v", audios[0])
	}
	if len(result.Programs) != 1 || *result.Programs[0].PCRPID != 256 {
		t.Errorf("unexpected programs: %+v", result.Programs)
	}
}

func TestSaveRecordedMetadataCreatesAndUpdates(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	program := buildTestProgram("/recorded/sample.ts", 3600)

	programID, err := store.SaveRecordedMetadata(ctx, program, 0)
	if err != nil {
		t.Fatalf("SaveRecordedMetadata returned error: %v", err)
	}
	if programID == 0 {
		t.Fatalf("programID is 0")
	}
	summaries, err := store.ListAllVideoSummaries(ctx)
	if err != nil {
		t.Fatalf("ListAllVideoSummaries returned error: %v", err)
	}
	if len(summaries) != 1 || summaries[0].FilePath != "/recorded/sample.ts" {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}
	// cm_sections は未解析の NULL で作成される
	var cmSections sql.NullString
	if err := store.DB.QueryRow(`SELECT cm_sections FROM recorded_videos`).Scan(&cmSections); err != nil {
		t.Fatalf("failed to read cm_sections: %v", err)
	}
	if cmSections.Valid {
		t.Errorf("cm_sections should be NULL, got %q", cmSections.String)
	}
	var keyFrames string
	if err := store.DB.QueryRow(`SELECT key_frames FROM recorded_videos`).Scan(&keyFrames); err != nil {
		t.Fatalf("failed to read key_frames: %v", err)
	}
	if keyFrames != "[]" {
		t.Errorf("key_frames = %q, want []", keyFrames)
	}

	// 同一ファイルパスで再保存すると UPDATE になる (重複行を作らない)
	program2 := buildTestProgram("/recorded/sample.ts", 3700)
	program2.Video.FileSize = 8192
	programID2, err := store.SaveRecordedMetadata(ctx, program2, programID)
	if err != nil {
		t.Fatalf("SaveRecordedMetadata (update) returned error: %v", err)
	}
	if programID2 != programID {
		t.Errorf("programID changed: %d -> %d", programID, programID2)
	}
	summaries, _ = store.ListAllVideoSummaries(ctx)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary after update, got %d", len(summaries))
	}
	if summaries[0].FileSize != 8192 {
		t.Errorf("file_size not updated: %d", summaries[0].FileSize)
	}
}

func TestSaveRecordedMetadataChannelAndTransportStreamID(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	program := buildTestProgram("/recorded/gr.ts", 3600)
	transportStreamID := 12345
	program.Channel = &Channel{
		ID:                "gr011",
		DisplayChannelID:  "gr011",
		NetworkID:         1,
		ServiceID:         1,
		TransportStreamID: &transportStreamID,
		RemoconID:         1,
		ChannelNumber:     "011",
		Type:              "GR",
		Name:              "テストチャンネル",
		IsWatchable:       true,
	}
	if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
		t.Fatalf("SaveRecordedMetadata returned error: %v", err)
	}
	var tsid sql.NullInt64
	if err := store.DB.QueryRow(`SELECT transport_stream_id FROM channels WHERE id = 'gr011'`).Scan(&tsid); err != nil {
		t.Fatalf("failed to read channel: %v", err)
	}
	if !tsid.Valid || tsid.Int64 != 12345 {
		t.Errorf("transport_stream_id = %v, want 12345", tsid)
	}
}

func TestProcessRecordedFileAndRunBatchScan(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "program.ts")
	if err := os.WriteFile(filePath, []byte("dummy recording data"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	// ファイル更新日時を 1 時間前にして「録画完了」扱いにする
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filePath, old, old); err != nil {
		t.Fatalf("failed to change times: %v", err)
	}

	program := buildTestProgram(filePath, 3600)
	analyzer := &fakeAnalyzer{program: program}
	service := &Service{
		Store:    store,
		Config:   ServiceConfig{RecordedFolders: []string{directory}},
		Analyzer: analyzer,
		Logger:   testLogger(t),
	}
	ok, err := service.RunBatchScan(ctx)
	if err != nil || !ok {
		t.Fatalf("RunBatchScan = %v, %v", ok, err)
	}
	summaries, err := store.ListAllVideoSummaries(ctx)
	if err != nil {
		t.Fatalf("ListAllVideoSummaries returned error: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 recorded video, got %d", len(summaries))
	}
	if summaries[0].Status != "Recorded" {
		t.Errorf("status = %q, want Recorded", summaries[0].Status)
	}

	// 2 回目のスキャンではファイルが変更されていないため再解析されない
	program.Title = "変更後タイトル"
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatalf("second RunBatchScan returned error: %v", err)
	}
	var title string
	if err := store.DB.QueryRow(`SELECT title FROM recorded_programs`).Scan(&title); err != nil {
		t.Fatalf("failed to read title: %v", err)
	}
	if title != "テスト番組" {
		t.Errorf("title was re-analyzed unexpectedly: %q", title)
	}

	// ファイルを削除すると、対応するレコードも削除される
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatalf("third RunBatchScan returned error: %v", err)
	}
	summaries, _ = store.ListAllVideoSummaries(ctx)
	if len(summaries) != 0 {
		t.Errorf("expected records to be deleted, got %d", len(summaries))
	}
}

func TestRunBackgroundAnalysisSavesCMSections(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "program.ts")
	if err := os.WriteFile(filePath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	chapter := "CHAPTER01=00:00:00.000\nCHAPTER01NAME=CM\nCHAPTER02=00:00:15.000\nCHAPTER02NAME=本編\n"
	if err := os.WriteFile(filepath.Join(directory, "program.chapter.txt"), []byte(chapter), 0o644); err != nil {
		t.Fatalf("failed to write chapter file: %v", err)
	}
	program := buildTestProgram(filePath, 3600)
	program.Video.FileHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
		t.Fatalf("SaveRecordedMetadata returned error: %v", err)
	}

	service := &Service{Store: store, Config: ServiceConfig{ThumbnailsDir: directory}, Logger: testLogger(t)}
	if err := service.RunBackgroundAnalysis(ctx); err != nil {
		t.Fatalf("RunBackgroundAnalysis returned error: %v", err)
	}
	var cmSections sql.NullString
	if err := store.DB.QueryRow(`SELECT cm_sections FROM recorded_videos`).Scan(&cmSections); err != nil {
		t.Fatalf("failed to read cm_sections: %v", err)
	}
	if !cmSections.Valid {
		t.Fatalf("cm_sections is still NULL")
	}
	if !strings.Contains(cmSections.String, `"start_time": 0.0`) {
		t.Errorf("unexpected cm_sections: %q", cmSections.String)
	}
}

func TestUpdateCMSectionsAndThumbnailInfo(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	program := buildTestProgram("/recorded/update.ts", 3600)
	if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
		t.Fatalf("SaveRecordedMetadata returned error: %v", err)
	}
	if err := store.UpdateCMSections(ctx, "/recorded/update.ts", []CMSection{{StartTime: 100, EndTime: 130}}); err != nil {
		t.Fatalf("UpdateCMSections returned error: %v", err)
	}
	layout := CalculateThumbnailLayout(3600)
	if err := store.UpdateThumbnailInfo(ctx, "/recorded/update.ts", BuildThumbnailInfo(layout)); err != nil {
		t.Fatalf("UpdateThumbnailInfo returned error: %v", err)
	}
	var cmSections, thumbnailInfo sql.NullString
	if err := store.DB.QueryRow(`SELECT cm_sections, thumbnail_info FROM recorded_videos`).Scan(&cmSections, &thumbnailInfo); err != nil {
		t.Fatalf("failed to read row: %v", err)
	}
	if !cmSections.Valid || !strings.Contains(cmSections.String, `"start_time": 100.0`) {
		t.Errorf("unexpected cm_sections: %v", cmSections)
	}
	if !thumbnailInfo.Valid || !strings.Contains(thumbnailInfo.String, `"total_tiles": 360`) {
		t.Errorf("unexpected thumbnail_info: %v", thumbnailInfo)
	}
	// 保存された cm_sections を再解析できること
	sections, err := ParseCMSections(cmSections.String)
	if err != nil || len(sections) != 1 || sections[0].StartTime != 100 {
		t.Errorf("ParseCMSections round-trip failed: %+v, %v", sections, err)
	}
}

func TestDeterminedFaceDetectionMode(t *testing.T) {
	if got := DetermineFaceDetectionMode([]Genre{{Major: "アニメ・特撮", Middle: "国内アニメ"}}); got != "Anime" {
		t.Errorf("Anime mode = %q", got)
	}
	if got := DetermineFaceDetectionMode([]Genre{{Major: "ニュース・報道", Middle: "定時ニュース"}}); got != "Human" {
		t.Errorf("Human mode = %q", got)
	}
	if got := DetermineFaceDetectionMode(nil); got != "" {
		t.Errorf("empty mode = %q", got)
	}
}

func TestFFprobeArgsMatchPython(t *testing.T) {
	full := strings.Join(FullProbeArgs("/recorded/sample.ts"), " ")
	wantFull := "-hide_banner -loglevel error -analyzeduration 30000000 -probesize 80M " +
		"-show_format -show_streams -show_programs -of json /recorded/sample.ts"
	if full != wantFull {
		t.Errorf("FullProbeArgs =\n%s\nwant\n%s", full, wantFull)
	}
	sample := strings.Join(SampleProbeArgs(), " ")
	wantSample := "-hide_banner -loglevel error -analyzeduration 30000000 -probesize 80M " +
		"-f mpegts -i pipe:0 -show_streams -of json"
	if sample != wantSample {
		t.Errorf("SampleProbeArgs =\n%s\nwant\n%s", sample, wantSample)
	}
}

func TestThumbnailPaths(t *testing.T) {
	tile, representative := ThumbnailPaths("/data/thumbnails", "abcdef")
	if tile != filepath.Join("/data/thumbnails", "abcdef_tile.webp") {
		t.Errorf("tile path = %q", tile)
	}
	if representative != filepath.Join("/data/thumbnails", "abcdef.webp") {
		t.Errorf("representative path = %q", representative)
	}
}

func TestProcessRecordedFileMarksRecording(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "recording.ts")
	if err := os.WriteFile(filePath, []byte("dummy recording data"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	// 最終更新日時が直近 (15 秒以内) のファイルは録画中として扱われる
	store.Now = time.Now
	program := buildTestProgram(filePath, 3600)
	service := &Service{
		Store:    store,
		Config:   ServiceConfig{RecordedFolders: []string{directory}},
		Analyzer: &fakeAnalyzer{program: program},
		Logger:   testLogger(t),
	}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatalf("RunBatchScan returned error: %v", err)
	}
	var status string
	if err := store.DB.QueryRow(`SELECT status FROM recorded_videos`).Scan(&status); err != nil {
		t.Fatalf("failed to read status: %v", err)
	}
	if status != "Recording" {
		t.Errorf("status = %q, want Recording", status)
	}
}

func TestProcessRecordedFileAnalysisFailed(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	directory := t.TempDir()
	filePath := filepath.Join(directory, "broken.ts")
	if err := os.WriteFile(filePath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filePath, old, old); err != nil {
		t.Fatalf("failed to change times: %v", err)
	}
	// 1 回目は正常に保存する
	program := buildTestProgram(filePath, 3600)
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{directory}},
		Analyzer: &fakeAnalyzer{program: program}, Logger: testLogger(t)}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatalf("RunBatchScan returned error: %v", err)
	}
	// 2 回目は解析に失敗させ、既存レコードのステータスが AnalysisFailed になることを確認する
	service.Analyzer = &fakeAnalyzer{program: nil}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatalf("second RunBatchScan returned error: %v", err)
	}
	var status string
	if err := store.DB.QueryRow(`SELECT status FROM recorded_videos`).Scan(&status); err != nil {
		t.Fatalf("failed to read status: %v", err)
	}
	if status != "AnalysisFailed" {
		t.Errorf("status = %q, want AnalysisFailed", status)
	}
}
