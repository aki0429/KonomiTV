package videostream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// encodingFixture は tools/generate_videostream_encoding_fixture.py が Python 版 VideoEncodingTask の実コードから生成した期待値。
type encodingFixture struct {
	Synth struct {
		FileSize  int64  `json:"file_size"`
		SHA256    string `json:"sha256"`
		VideoPID  int    `json:"video_pid"`
		AudioPID  int    `json:"audio_pid"`
		PCRPID    int    `json:"pcr_pid"`
		BaseDTS   int64  `json:"base_dts"`
		IDRFrames []struct {
			Index  int   `json:"index"`
			Offset int64 `json:"offset"`
			DTS    int64 `json:"dts"`
		} `json:"idr_frames"`
	} `json:"synth"`
	OptionHashes   map[string]string   `json:"option_hashes"`
	OptionSamples  map[string][]string `json:"option_samples"`
	FloatReprs     map[string]string   `json:"float_reprs"`
	RetrySweep     map[string][]string `json:"retry_sweep"`
	CollectorCases map[string]struct {
		StartOffset int64 `json:"start_offset"`
		InitialDTS  int64 `json:"initial_dts"`
		ChunkPacket int   `json:"chunk_packets"`
		KeyFrames   []struct {
			Offset int64 `json:"offset"`
			DTS    int64 `json:"dts"`
		} `json:"key_frames"`
	} `json:"collector_cases"`
	Scenarios map[string]encodingScenario `json:"scenarios"`
	Constants struct {
		GOPLengthSecond float64 `json:"gop_length_second"`
		MaxRetryCount   int     `json:"max_retry_count"`
	} `json:"constants"`
}

type encodingScenario struct {
	Input struct {
		Video struct {
			ContainerFormat       string  `json:"container_format"`
			FilePath              string  `json:"file_path"`
			VideoCodec            string  `json:"video_codec"`
			VideoScanType         string  `json:"video_scan_type"`
			VideoFrameRate        float64 `json:"video_frame_rate"`
			VideoResolutionWidth  int     `json:"video_resolution_width"`
			VideoResolutionHeight int     `json:"video_resolution_height"`
		} `json:"video"`
		Channel *struct {
			NetworkID         int  `json:"network_id"`
			TransportStreamID *int `json:"transport_stream_id"`
			ServiceID         int  `json:"service_id"`
		} `json:"channel"`
		Quality              string           `json:"quality"`
		IsHEVC10bitEnabled   bool             `json:"is_hevc_10bit_enabled"`
		Is24fpsModeEnabled   bool             `json:"is_24fps_mode_enabled"`
		Encoder              string           `json:"encoder"`
		AdaptResolutionAvail bool             `json:"adapt_resolution_available"`
		EncoderOutputsTS     bool             `json:"encoder_outputs_ts"`
		SegmentCount         int              `json:"segment_count"`
		SegmentDuration      float64          `json:"segment_duration"`
		StartSequence        int              `json:"start_sequence"`
		UseStreamInfo        bool             `json:"use_stream_info"`
		SegmentPositions     map[string]int64 `json:"segment_positions"`
		SegmentDTS           map[string]int64 `json:"segment_dts"`
	} `json:"input"`
	Result struct {
		Spawns []struct {
			Name string   `json:"name"`
			Args []string `json:"args"`
		} `json:"spawns"`
		TSReadExStdin map[string]struct {
			Length int    `json:"length"`
			SHA256 string `json:"sha256"`
		} `json:"tsreadex_stdin"`
		Segments []struct {
			Status string  `json:"status"`
			Length *int    `json:"length"`
			SHA256 *string `json:"sha256"`
		} `json:"segments"`
		RetryCount int  `json:"retry_count"`
		IsFinished bool `json:"is_finished"`
		KeyFrames  []struct {
			Offset int64 `json:"offset"`
			DTS    int64 `json:"dts"`
		} `json:"keyframes"`
	} `json:"result"`
}

func loadEncodingFixture(t *testing.T) *encodingFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "encoding_task_fixture.json"))
	if err != nil {
		t.Fatalf("フィクスチャがありません (tools/generate_videostream_encoding_fixture.py で生成してください): %v", err)
	}
	fixture := &encodingFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func sha16(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// parseOptionKey は "FFmpeg|720p|Interlaced|H.264|1920x1080|01|2|1" を分解する。
func parseOptionKey(t *testing.T, key string) (encoder, quality string, video EncodingVideoInfo, options stream.StreamEncodingOptions, retry int, adapt bool) {
	t.Helper()
	parts := strings.Split(key, "|")
	if len(parts) != 8 {
		t.Fatalf("invalid key %q", key)
	}
	encoder, quality = parts[0], parts[1]
	video.VideoScanType = parts[2]
	video.VideoCodec = parts[3]
	size := strings.Split(parts[4], "x")
	video.VideoResolutionWidth, _ = strconv.Atoi(size[0])
	video.VideoResolutionHeight, _ = strconv.Atoi(size[1])
	if video.VideoScanType == "Progressive" {
		video.VideoFrameRate = 23.976
	} else {
		video.VideoFrameRate = 29.97
	}
	options = stream.StreamEncodingOptions{IsHEVC10bitEnabled: parts[5][0] == '1', Is24fpsModeEnabled: parts[5][1] == '1'}
	retry, _ = strconv.Atoi(parts[6])
	adapt = parts[7] == "1"
	return
}

func buildForKey(t *testing.T, encoder, quality string, video EncodingVideoInfo, options stream.StreamEncodingOptions, retry int, offset float64, adapt bool) []string {
	t.Helper()
	var args []string
	var err error
	if encoder == "FFmpeg" {
		args, err = BuildVideoFFmpegOptions(quality, video, options, retry, offset)
	} else {
		args, err = BuildVideoHWEncCOptions(quality, encoder, video, options, retry, offset, func(string) bool { return adapt })
	}
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// TestEncodingFixtureIsSelfConsistent はフィクスチャの合成 TS が改変されていないことを確認する。
func TestEncodingFixtureIsSelfConsistent(t *testing.T) {
	fixture := loadEncodingFixture(t)
	data, err := os.ReadFile(filepath.Join("testdata", "encoding_synth.ts"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != fixture.Synth.SHA256 || int64(len(data)) != fixture.Synth.FileSize {
		t.Fatalf("encoding_synth.ts がフィクスチャと一致しません")
	}
	if fixture.Constants.GOPLengthSecond != EncodingGOPLengthSecond || fixture.Constants.MaxRetryCount != MaxEncodingRetryCount {
		t.Errorf("constants mismatch: %+v", fixture.Constants)
	}
}

// TestBuildOptionsMatchPython は Python 版 buildFFmpegOptions / buildHWEncCOptions の全組み合わせ (20736 通り) と
// 引数列のハッシュが一致することを検証する。
func TestBuildOptionsMatchPython(t *testing.T) {
	fixture := loadEncodingFixture(t)
	if len(fixture.OptionHashes) < 20000 {
		t.Fatalf("option_hashes が少なすぎます: %d", len(fixture.OptionHashes))
	}
	mismatches := 0
	for key, want := range fixture.OptionHashes {
		encoder, quality, video, options, retry, adapt := parseOptionKey(t, key)
		offset := 0.0
		if retry == 0 {
			offset = 12.345
		}
		got := sha16(buildForKey(t, encoder, quality, video, options, retry, offset, adapt))
		if got != want {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("%s: hash = %s, want %s\nargs: %s", key, got, want, strings.Join(buildForKey(t, encoder, quality, video, options, retry, offset, adapt), " "))
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d / %d ケースが Python と不一致", mismatches, len(fixture.OptionHashes))
	}
}

// TestBuildOptionsSamplesExact は代表ケースの引数列が Python と完全一致する (ハッシュではなく全要素の比較) ことを検証する。
func TestBuildOptionsSamplesExact(t *testing.T) {
	fixture := loadEncodingFixture(t)
	if len(fixture.OptionSamples) == 0 {
		t.Fatal("option_samples が空です")
	}
	for key, want := range fixture.OptionSamples {
		encoder, quality, video, options, retry, adapt := parseOptionKey(t, key)
		offset := 0.0
		if retry == 0 {
			offset = 12.345
		}
		got := buildForKey(t, encoder, quality, video, options, retry, offset, adapt)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s:\n got  %s\n want %s", key, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}

// TestBuildOptionsRetrySweep はリトライ回数 0..9 でのタイムアウト系オプション (浮動小数点表記を含む) の完全一致を検証する。
func TestBuildOptionsRetrySweep(t *testing.T) {
	fixture := loadEncodingFixture(t)
	if len(fixture.RetrySweep) != 5*2*10 {
		t.Fatalf("retry_sweep の件数 = %d", len(fixture.RetrySweep))
	}
	for key, want := range fixture.RetrySweep {
		parts := strings.Split(key, "|")
		retry, _ := strconv.Atoi(parts[2])
		video := EncodingVideoInfo{VideoCodec: parts[1], VideoScanType: "Interlaced", VideoFrameRate: 29.97, VideoResolutionWidth: 1920, VideoResolutionHeight: 1080}
		got := buildForKey(t, parts[0], "720p", video, stream.StreamEncodingOptions{}, retry, float64(retry)*1.1, true)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s:\n got  %s\n want %s", key, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}

// TestPythonFloatRepr は -output_ts_offset / output_ts_offset: に入る浮動小数点の文字列表現が Python の repr() と一致することを検証する。
func TestPythonFloatRepr(t *testing.T) {
	fixture := loadEncodingFixture(t)
	for dtsText, want := range fixture.FloatReprs {
		dts, _ := strconv.ParseInt(dtsText, 10, 64)
		if got := PythonFloatRepr(float64(dts) / 90000); got != want {
			t.Errorf("repr(%d / 90000) = %s, want %s", dts, got, want)
		}
	}
	// フィクスチャ外の境界値 (Python 3 の repr 規則: 1e16 以上と 1e-4 未満は指数表記)
	for value, want := range map[float64]string{0: "0.0", 1: "1.0", 0.5: "0.5", 1e16: "1e+16", 1.0 / 90000: "1.1111111111111112e-05", 12.345: "12.345"} {
		if got := PythonFloatRepr(value); got != want {
			t.Errorf("PythonFloatRepr(%v) = %s, want %s", value, got, want)
		}
	}
}

func TestBuildOptionsRejectsUnknownQuality(t *testing.T) {
	if _, err := BuildVideoFFmpegOptions("999p", EncodingVideoInfo{}, stream.StreamEncodingOptions{}, 0, 0); err == nil {
		t.Error("不明な画質でエラーにならない")
	}
	if _, err := BuildVideoHWEncCOptions("999p", "NVEncC", EncodingVideoInfo{}, stream.StreamEncodingOptions{}, 0, 0, func(string) bool { return true }); err == nil {
		t.Error("不明な画質でエラーにならない")
	}
}

// TestPsisimuxAndTSReadExOptions は tsreadex / psisimux の引数が Python 版と一致することを検証する (チャンネルあり・TSID なし・チャンネルなし)。
func TestPsisimuxAndTSReadExOptions(t *testing.T) {
	wantTSReadEx := []string{"-x", "18/38/39", "-n", "1024", "-a", "13", "-b", "5", "-c", "5", "-u", "1", "-d", "9", "-"}
	if got := BuildTSReadExOptions("1024"); strings.Join(got, " ") != strings.Join(wantTSReadEx, " ") {
		t.Errorf("tsreadex = %v", got)
	}
	tsid := 32736
	broadcastID, serviceID := ResolveMP4BroadcastIDs(&EncodingChannel{NetworkID: 32736, TransportStreamID: &tsid, ServiceID: 1024})
	if broadcastID != "32736/32736/1024" || serviceID != "1024" {
		t.Errorf("with tsid: %s %s", broadcastID, serviceID)
	}
	broadcastID, serviceID = ResolveMP4BroadcastIDs(&EncodingChannel{NetworkID: 32736, ServiceID: 1024})
	if broadcastID != "32736/65535/1024" || serviceID != "1024" {
		t.Errorf("without tsid: %s %s", broadcastID, serviceID)
	}
	broadcastID, serviceID = ResolveMP4BroadcastIDs(nil)
	if broadcastID != "1/2/3" || serviceID != "-1" {
		t.Errorf("without channel: %s %s", broadcastID, serviceID)
	}
	got := BuildPsisimuxOptions(95549.82222222222, "1/2/3", "C:/rec/a.mp4")
	want := []string{"-m", "95549822", "-b", "1/2/3", "-8", "-x", ".vtt", "C:/rec/a.mp4", "-"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("psisimux = %v", got)
	}
}

// TestResolveEncoderType は映像ストリーム構成変化時の FFmpeg 固定を検証する。
// NOTE: 現行の Python 版 (fc98a36a 以降) にはこの固定は存在せず (--adapt-resolution に置き換えられた)、
// 固定が有効だった 96f2f506 の挙動を pinFFmpeg=true で再現する。
func TestResolveEncoderType(t *testing.T) {
	cases := []struct {
		configured, container string
		changes, pin          bool
		want                  string
	}{
		{"NVEncC", "MPEG-TS", true, true, "FFmpeg"},
		{"NVEncC", "MPEG-TS", true, false, "NVEncC"},
		{"NVEncC", "MPEG-TS", false, true, "NVEncC"},
		{"NVEncC", "MPEG-4", true, true, "NVEncC"},
		{"FFmpeg", "MPEG-TS", true, true, "FFmpeg"},
	}
	for _, c := range cases {
		if got := ResolveEncoderType(c.configured, c.container, c.changes, c.pin); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}
