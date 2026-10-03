package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

// streamOptionsFixture は Python 版で生成したパリティ検証用フィクスチャ。
// 引数の組み立ては全ケースの SHA-256 ハッシュを比較し、代表的なケースのみ引数の完全一致を検証する。
type streamOptionsFixture struct {
	Qualities    map[string]fixtureQuality `json:"qualities"`
	QualityTypes []string                  `json:"quality_types"`
	Split        []fixtureSplit            `json:"split"`
	FFmpeg       []fixtureFFmpeg           `json:"ffmpeg"`
	FFmpegRadio  []fixtureFFmpegRadio      `json:"ffmpeg_radio"`
	HWEnc        []fixtureHWEnc            `json:"hwenc"`
}

type fixtureQuality struct {
	IsHEVC          bool   `json:"is_hevc"`
	Is60fps         bool   `json:"is_60fps"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	VideoBitrate    string `json:"video_bitrate"`
	VideoBitrateMax string `json:"video_bitrate_max"`
	AudioBitrate    string `json:"audio_bitrate"`
}

type fixtureSplit struct {
	Request string `json:"request"`
	Encoder string `json:"encoder"`
	Result  *struct {
		Quality            string `json:"quality"`
		Suffix             string `json:"suffix"`
		IsHEVC10bitEnabled bool   `json:"is_hevc_10bit_enabled"`
		Is24fpsModeEnabled bool   `json:"is_24fps_mode_enabled"`
	} `json:"result"`
}

type fixtureFFmpeg struct {
	Quality            string   `json:"q"`
	ChannelType        string   `json:"ct"`
	IsFullHDChannel    bool     `json:"fh"`
	RetryCount         int      `json:"r"`
	Is24fpsModeEnabled bool     `json:"fps"`
	ArgsHash           string   `json:"h"`
	Args               []string `json:"args"`
}

type fixtureFFmpegRadio struct {
	RetryCount int      `json:"r"`
	ArgsHash   string   `json:"h"`
	Args       []string `json:"args"`
}

type fixtureHWEnc struct {
	Quality                    string   `json:"q"`
	Encoder                    string   `json:"e"`
	ChannelType                string   `json:"ct"`
	IsFullHDChannel            bool     `json:"fh"`
	RetryCount                 int      `json:"r"`
	IsHEVC10bitEnabled         bool     `json:"b10"`
	Is24fpsModeEnabled         bool     `json:"fps"`
	IsAdaptResolutionAvailable bool     `json:"ar"`
	ArgsHash                   string   `json:"h"`
	Args                       []string `json:"args"`
}

// loadStreamOptionsFixture はフィクスチャを読み込む。
func loadStreamOptionsFixture(t *testing.T) *streamOptionsFixture {
	t.Helper()
	file, err := os.Open("testdata/stream_options.json")
	if err != nil {
		t.Fatalf("failed to open fixture: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	fixture := &streamOptionsFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	return fixture
}

// hashArgs は引数の配列の SHA-256 ハッシュを返す (Python 版の args_hash() と同じ計算) 。
func hashArgs(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\n")))
	return hex.EncodeToString(sum[:])
}

// TestQualityTableMatchesPython は画質テーブルが Python 版と一致することを検証する。
func TestQualityTableMatchesPython(t *testing.T) {
	fixture := loadStreamOptionsFixture(t)
	if len(Qualities) != len(fixture.Qualities) {
		t.Fatalf("qualities = %d, want %d", len(Qualities), len(fixture.Qualities))
	}
	for quality, expected := range fixture.Qualities {
		actual, ok := Qualities[quality]
		if !ok {
			t.Errorf("quality %q is missing", quality)
			continue
		}
		if actual.IsHEVC != expected.IsHEVC || actual.Is60fps != expected.Is60fps ||
			actual.Width != expected.Width || actual.Height != expected.Height ||
			actual.VideoBitrate != expected.VideoBitrate || actual.VideoBitrateMax != expected.VideoBitrateMax ||
			actual.AudioBitrate != expected.AudioBitrate {
			t.Errorf("quality %q = %+v, want %+v", quality, actual, expected)
		}
	}
	// 画質の並び順も Python 版と一致させる (QUALITY_TYPES の順)。
	if !reflect.DeepEqual(QualityTypes, fixture.QualityTypes) {
		t.Errorf("QualityTypes = %v, want %v", QualityTypes, fixture.QualityTypes)
	}
}

// TestSplitQualityAndEncodingOptionsMatchesPython は品質指定の分解が Python 版と一致することを検証する。
func TestSplitQualityAndEncodingOptionsMatchesPython(t *testing.T) {
	fixture := loadStreamOptionsFixture(t)
	for _, testCase := range fixture.Split {
		actual, ok := SplitQualityAndEncodingOptions(testCase.Request, testCase.Encoder)
		if testCase.Result == nil {
			if ok {
				t.Errorf("SplitQualityAndEncodingOptions(%q, %q) = %+v, want invalid", testCase.Request, testCase.Encoder, actual)
			}
			continue
		}
		if !ok {
			t.Errorf("SplitQualityAndEncodingOptions(%q, %q) is invalid, want %+v", testCase.Request, testCase.Encoder, testCase.Result)
			continue
		}
		if actual.Quality != testCase.Result.Quality ||
			actual.EncodingOptions.BuildSuffix() != testCase.Result.Suffix ||
			actual.EncodingOptions.IsHEVC10bitEnabled != testCase.Result.IsHEVC10bitEnabled ||
			actual.EncodingOptions.Is24fpsModeEnabled != testCase.Result.Is24fpsModeEnabled {
			t.Errorf("SplitQualityAndEncodingOptions(%q, %q) = %+v (suffix %q), want %+v (suffix %q)",
				testCase.Request, testCase.Encoder, actual, actual.EncodingOptions.BuildSuffix(), testCase.Result, testCase.Result.Suffix)
		}
	}
}

// TestBuildFFmpegOptionsMatchesPython は FFmpeg の引数が Python 版と一致することを検証する。
func TestBuildFFmpegOptionsMatchesPython(t *testing.T) {
	fixture := loadStreamOptionsFixture(t)
	for _, testCase := range fixture.FFmpeg {
		options := StreamEncodingOptions{Is24fpsModeEnabled: testCase.Is24fpsModeEnabled}
		actual := BuildFFmpegOptions(testCase.Quality, testCase.ChannelType, testCase.IsFullHDChannel, options, testCase.RetryCount)
		if hashArgs(actual) != testCase.ArgsHash {
			t.Errorf("BuildFFmpegOptions(%q, %q, %v, 24fps=%v, retry=%d):\nactual = %v\nwant   = %v",
				testCase.Quality, testCase.ChannelType, testCase.IsFullHDChannel,
				testCase.Is24fpsModeEnabled, testCase.RetryCount, actual, testCase.Args)
		}
		if testCase.Args != nil && !reflect.DeepEqual(actual, testCase.Args) {
			t.Errorf("BuildFFmpegOptions(%q, %q, %v, 24fps=%v, retry=%d):\nactual = %v\nwant   = %v",
				testCase.Quality, testCase.ChannelType, testCase.IsFullHDChannel,
				testCase.Is24fpsModeEnabled, testCase.RetryCount, actual, testCase.Args)
		}
	}
}

// TestBuildFFmpegOptionsForRadioMatchesPython はラジオチャンネル向け FFmpeg の引数が Python 版と一致することを検証する。
func TestBuildFFmpegOptionsForRadioMatchesPython(t *testing.T) {
	fixture := loadStreamOptionsFixture(t)
	for _, testCase := range fixture.FFmpegRadio {
		actual := BuildFFmpegOptionsForRadio(testCase.RetryCount)
		if hashArgs(actual) != testCase.ArgsHash {
			t.Errorf("BuildFFmpegOptionsForRadio(retry=%d):\nactual = %v\nwant   = %v", testCase.RetryCount, actual, testCase.Args)
		}
		if !reflect.DeepEqual(actual, testCase.Args) {
			t.Errorf("BuildFFmpegOptionsForRadio(retry=%d):\nactual = %v\nwant   = %v", testCase.RetryCount, actual, testCase.Args)
		}
	}
}

// TestBuildHWEncCOptionsMatchesPython は HWEncC の引数が Python 版と一致することを検証する。
func TestBuildHWEncCOptionsMatchesPython(t *testing.T) {
	fixture := loadStreamOptionsFixture(t)
	for _, testCase := range fixture.HWEnc {
		options := StreamEncodingOptions{
			IsHEVC10bitEnabled: testCase.IsHEVC10bitEnabled,
			Is24fpsModeEnabled: testCase.Is24fpsModeEnabled,
		}
		isAvailable := func(string) bool { return testCase.IsAdaptResolutionAvailable }
		actual := BuildHWEncCOptions(testCase.Quality, testCase.Encoder, testCase.ChannelType,
			testCase.IsFullHDChannel, options, testCase.RetryCount, isAvailable)
		if hashArgs(actual) != testCase.ArgsHash {
			t.Errorf("BuildHWEncCOptions(%q, %q, %q, retry=%d, 10bit=%v, 24fps=%v, adapt=%v):\nactual = %v\nwant   = %v",
				testCase.Quality, testCase.Encoder, testCase.ChannelType, testCase.RetryCount,
				testCase.IsHEVC10bitEnabled, testCase.Is24fpsModeEnabled, testCase.IsAdaptResolutionAvailable,
				actual, testCase.Args)
		}
		if testCase.Args != nil && !reflect.DeepEqual(actual, testCase.Args) {
			t.Errorf("BuildHWEncCOptions(%q, %q, %q, retry=%d, 10bit=%v, 24fps=%v, adapt=%v):\nactual = %v\nwant   = %v",
				testCase.Quality, testCase.Encoder, testCase.ChannelType, testCase.RetryCount,
				testCase.IsHEVC10bitEnabled, testCase.Is24fpsModeEnabled, testCase.IsAdaptResolutionAvailable,
				actual, testCase.Args)
		}
	}
}
