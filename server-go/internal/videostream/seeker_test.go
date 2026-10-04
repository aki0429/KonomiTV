package videostream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tsSeekerFixture は TS キーフレーム探索の期待値 (tools/generate_videostream_fixture.py が生成する) 。
type tsSeekerFixture struct {
	FileSize   int64 `json:"file_size"`
	StreamInfo struct {
		VideoPID   int    `json:"video_pid"`
		PCRPID     int    `json:"pcr_pid"`
		Codec      string `json:"codec"`
		PacketSize int    `json:"packet_size"`
	} `json:"stream_info"`
	BaseDTS                int64   `json:"base_dts"`
	VideoFrameRate         float64 `json:"video_frame_rate"`
	SegmentDurationSeconds float64 `json:"segment_duration_seconds"`
	Seeks                  []struct {
		PlaylistStartSeconds float64 `json:"playlist_start_seconds"`
		SourceFilePosition   *int64  `json:"source_file_position"`
		SourceStartDTS       *int64  `json:"source_start_dts"`
	} `json:"seeks"`
	SegmentDurations map[string]float64 `json:"segment_durations"`
}

// loadTSSeekerFixture は期待値を読み込む。
func loadTSSeekerFixture(t *testing.T) *tsSeekerFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "ts_seeker.json"))
	if err != nil {
		t.Skipf("フィクスチャがありません (tools/generate_videostream_fixture.py で生成してください): %v", err)
	}
	fixture := &tsSeekerFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// tsFixturePath はフィクスチャ生成に使った TS ファイルのパスを返す。
// 環境変数 KONOMITV_TS_FIXTURE で指定する (未指定の場合はテストをスキップする) 。
func tsFixturePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("KONOMITV_TS_FIXTURE")
	if path == "" {
		t.Skip("KONOMITV_TS_FIXTURE が設定されていないためスキップします")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("TS ファイルが見つかりません: %v", err)
	}
	return path
}

// TestFindStreamInfo は PAT / PMT からの映像 PID の取得が Python 版と一致することを検証する。
func TestFindStreamInfo(t *testing.T) {
	path := tsFixturePath(t)
	fixture := loadTSSeekerFixture(t)

	streamInfo, err := FindStreamInfo(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if streamInfo.VideoPID != fixture.StreamInfo.VideoPID {
		t.Errorf("VideoPID = %d, want %d", streamInfo.VideoPID, fixture.StreamInfo.VideoPID)
	}
	if streamInfo.PCRPID != fixture.StreamInfo.PCRPID {
		t.Errorf("PCRPID = %d, want %d", streamInfo.PCRPID, fixture.StreamInfo.PCRPID)
	}
	if streamInfo.Codec != fixture.StreamInfo.Codec {
		t.Errorf("Codec = %q, want %q", streamInfo.Codec, fixture.StreamInfo.Codec)
	}
	if streamInfo.PacketSize != fixture.StreamInfo.PacketSize {
		t.Errorf("PacketSize = %d, want %d", streamInfo.PacketSize, fixture.StreamInfo.PacketSize)
	}
}

// TestFindBaseDTS は先頭キーフレーム DTS の取得が Python 版と一致することを検証する。
func TestFindBaseDTS(t *testing.T) {
	path := tsFixturePath(t)
	fixture := loadTSSeekerFixture(t)

	streamInfo, err := FindStreamInfo(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	baseDTS, err := FindBaseDTS(path, streamInfo)
	if err != nil {
		t.Fatal(err)
	}
	if baseDTS != fixture.BaseDTS {
		t.Errorf("BaseDTS = %d, want %d", baseDTS, fixture.BaseDTS)
	}
}

// TestSeek はキーフレーム探索の結果が Python 版と完全に一致することを検証する。
func TestSeek(t *testing.T) {
	path := tsFixturePath(t)
	fixture := loadTSSeekerFixture(t)

	streamInfo, err := FindStreamInfo(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	maxKeyframeAgeTicks := int64(roundHalfEven(fixture.SegmentDurationSeconds * float64(HZ)))
	for _, expected := range fixture.Seeks {
		position, err := Seek(path, streamInfo, expected.PlaylistStartSeconds, fixture.BaseDTS, maxKeyframeAgeTicks)
		if expected.SourceFilePosition == nil {
			if err == nil {
				t.Errorf("seek(%.3f) = %+v, want error", expected.PlaylistStartSeconds, position)
			}
			continue
		}
		if err != nil {
			t.Errorf("seek(%.3f) failed: %v", expected.PlaylistStartSeconds, err)
			continue
		}
		if position.SourceFilePosition != *expected.SourceFilePosition {
			t.Errorf(
				"seek(%.3f).SourceFilePosition = %d, want %d",
				expected.PlaylistStartSeconds, position.SourceFilePosition, *expected.SourceFilePosition,
			)
		}
		if position.SourceStartDTS != *expected.SourceStartDTS {
			t.Errorf(
				"seek(%.3f).SourceStartDTS = %d, want %d",
				expected.PlaylistStartSeconds, position.SourceStartDTS, *expected.SourceStartDTS,
			)
		}
	}
}

// TestComputeSegmentDurationSeconds はセグメント長の計算が Python 版と一致することを検証する。
func TestComputeSegmentDurationSeconds(t *testing.T) {
	fixture := loadTSSeekerFixture(t)
	for frameRateText, expected := range fixture.SegmentDurations {
		frameRate := parseFloat(t, frameRateText)
		actual := ComputeSegmentDurationSeconds(frameRate)
		if actual != expected {
			t.Errorf("ComputeSegmentDurationSeconds(%s) = %v, want %v", frameRateText, actual, expected)
		}
	}
}

// parseFloat はテスト用に文字列を float64 に変換する。
func parseFloat(t *testing.T, value string) float64 {
	t.Helper()
	var result float64
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
