package metadata

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// FFprobe の解析パラメータ (MetadataAnalyzer.FFPROBE_ANALYZE_DURATION_US / FFPROBE_PROBESIZE) 。
const (
	ffprobeAnalyzeDurationUS = "30000000"
	ffprobeProbeSize         = "80M"
)

// FullProbeArgs は録画ファイル全体の解析 (全体解析) に使う FFprobe の引数を返す。
// 移植元: MetadataAnalyzer.__analyzeFFprobe() の args_full
func FullProbeArgs(path string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-analyzeduration", ffprobeAnalyzeDurationUS,
		"-probesize", ffprobeProbeSize,
		"-show_format",
		"-show_streams",
		"-show_programs",
		"-of", "json",
		path,
	}
}

// SampleProbeArgs は 25% 位置から切り出したサンプルを標準入力から解析 (部分解析) する FFprobe の引数を返す。
// 移植元: MetadataAnalyzer.__analyzeFFprobe() の args_sample
func SampleProbeArgs() []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-analyzeduration", ffprobeAnalyzeDurationUS,
		"-probesize", ffprobeProbeSize,
		"-f", "mpegts",
		"-i", "pipe:0",
		"-show_streams",
		"-of", "json",
	}
}

// ProbeFormat は FFprobe の format セクション (FFprobeFormat) 。
type ProbeFormat struct {
	FormatName string
	Duration   *string
}

// ProbeVideoStream は FFprobe の映像ストリーム (FFprobeVideoStream) 。
type ProbeVideoStream struct {
	CodecName     string
	Duration      *float64
	Profile       *string
	Width         int
	Height        int
	AvgFrameRate  string
	RFrameRate    string
	FieldOrder    *string
	TSPacketSize  *string
	TSPacketSizeV int
}

// ProbeAudioStream は FFprobe の音声ストリーム (FFprobeAudioStream) 。
type ProbeAudioStream struct {
	CodecName  string
	Duration   *float64
	Profile    *string
	Channels   int
	SampleRate string
	TSPacket   *string
}

// ProbeOtherStream は映像・音声として検証できなかったストリーム (FFprobeOtherStream) 。
type ProbeOtherStream struct {
	CodecType string
	CodecName string
	TSPacket  *string
}

// ProbeProgram は FFprobe の program セクション (FFprobeProgram) 。
type ProbeProgram struct {
	ProgramNum *int
	NbStreams  *int
	PCRPID     *int
}

// ProbeStream は検証済みのストリーム 1 件 (Python の FFprobeVideoStream | FFprobeAudioStream | FFprobeOtherStream) 。
// Video / Audio / Other のうち 1 つだけが非 nil になる。
type ProbeStream struct {
	Video *ProbeVideoStream
	Audio *ProbeAudioStream
	Other *ProbeOtherStream
}

// ts_packetsize を返す (どのストリーム種別でも同じ意味) 。
func (s ProbeStream) tsPacketSize() *string {
	switch {
	case s.Video != nil:
		return s.Video.TSPacketSize
	case s.Audio != nil:
		return s.Audio.TSPacket
	case s.Other != nil:
		return s.Other.TSPacket
	}
	return nil
}

// codecTypeIs はストリームの codec_type が指定値かを返す。
func (s ProbeStream) codecTypeIs(codecType string) bool {
	switch codecType {
	case "video":
		return s.Video != nil || (s.Other != nil && s.Other.CodecType == "video")
	case "audio":
		return s.Audio != nil || (s.Other != nil && s.Other.CodecType == "audio")
	}
	return false
}

// ProbeResult は FFprobe の全体解析結果 (FFprobeResult) 。
type ProbeResult struct {
	Format   ProbeFormat
	Streams  []ProbeStream
	Programs []ProbeProgram
}

// ProbeSampleResult は FFprobe の部分解析結果 (FFprobeSampleResult) 。
type ProbeSampleResult struct {
	Streams []ProbeStream
}

// VideoStreams は映像ストリームのみを返す。
func videoStreamsOf(streams []ProbeStream) []*ProbeVideoStream {
	result := []*ProbeVideoStream{}
	for _, stream := range streams {
		if stream.Video != nil {
			result = append(result, stream.Video)
		}
	}
	return result
}

// audioStreamsOf は音声ストリームのみを返す。
func audioStreamsOf(streams []ProbeStream) []*ProbeAudioStream {
	result := []*ProbeAudioStream{}
	for _, stream := range streams {
		if stream.Audio != nil {
			result = append(result, stream.Audio)
		}
	}
	return result
}

// ***** Pydantic (lax mode) 相当の値検証ヘルパー *****

// jsonString は str 型フィールドを検証する (Pydantic v2 は int → str の暗黙変換をしない) 。
func jsonString(object map[string]any, key string, required bool) (*string, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return nil, !required
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	return &text, true
}

// jsonInt は int 型フィールドを検証する (整数値の float と数値文字列は Pydantic lax mode で受理される) 。
func jsonInt(object map[string]any, key string, required bool) (*int, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return nil, !required
	}
	switch typed := value.(type) {
	case float64:
		if typed != math.Trunc(typed) || math.IsInf(typed, 0) {
			return nil, false
		}
		result := int(typed)
		return &result, true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return nil, false
		}
		return &parsed, true
	}
	return nil, false
}

// jsonFloat は float | None 型フィールドを検証する (数値文字列は Pydantic lax mode で受理される) 。
func jsonFloat(object map[string]any, key string) (*float64, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return nil, true
	}
	switch typed := value.(type) {
	case float64:
		return &typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return nil, false
		}
		return &parsed, true
	}
	return nil, false
}

// parseVideoStream は FFprobeVideoStream として検証する。失敗した場合は ok = false 。
func parseVideoStream(object map[string]any) (*ProbeVideoStream, bool) {
	if codecType, _ := object["codec_type"].(string); codecType != "video" {
		return nil, false
	}
	if _, ok := jsonInt(object, "index", true); !ok {
		return nil, false
	}
	codecName, ok := jsonString(object, "codec_name", true)
	if !ok || codecName == nil {
		return nil, false
	}
	if *codecName != "mpeg2video" && *codecName != "h264" && *codecName != "hevc" {
		return nil, false
	}
	duration, ok := jsonFloat(object, "duration")
	if !ok {
		return nil, false
	}
	profile, ok := jsonString(object, "profile", false)
	if !ok {
		return nil, false
	}
	width, ok := jsonInt(object, "width", true)
	if !ok || width == nil || *width <= 0 {
		return nil, false
	}
	height, ok := jsonInt(object, "height", true)
	if !ok || height == nil || *height <= 0 {
		return nil, false
	}
	avgFrameRate, ok := jsonString(object, "avg_frame_rate", true)
	if !ok || avgFrameRate == nil || *avgFrameRate == "" || *avgFrameRate == "0/0" {
		return nil, false
	}
	rFrameRate, ok := jsonString(object, "r_frame_rate", true)
	if !ok || rFrameRate == nil || *rFrameRate == "" || *rFrameRate == "0/0" {
		return nil, false
	}
	fieldOrder, ok := jsonString(object, "field_order", false)
	if !ok {
		return nil, false
	}
	tsPacketSize, ok := jsonString(object, "ts_packetsize", false)
	if !ok {
		return nil, false
	}
	return &ProbeVideoStream{
		CodecName:    *codecName,
		Duration:     duration,
		Profile:      profile,
		Width:        *width,
		Height:       *height,
		AvgFrameRate: *avgFrameRate,
		RFrameRate:   *rFrameRate,
		FieldOrder:   fieldOrder,
		TSPacketSize: tsPacketSize,
	}, true
}

// parseAudioStream は FFprobeAudioStream として検証する。失敗した場合は ok = false 。
func parseAudioStream(object map[string]any) (*ProbeAudioStream, bool) {
	if codecType, _ := object["codec_type"].(string); codecType != "audio" {
		return nil, false
	}
	if _, ok := jsonInt(object, "index", true); !ok {
		return nil, false
	}
	codecName, ok := jsonString(object, "codec_name", true)
	if !ok || codecName == nil || *codecName != "aac" {
		return nil, false
	}
	duration, ok := jsonFloat(object, "duration")
	if !ok {
		return nil, false
	}
	profile, ok := jsonString(object, "profile", false)
	if !ok {
		return nil, false
	}
	channels, ok := jsonInt(object, "channels", true)
	if !ok || channels == nil || (*channels != 1 && *channels != 2 && *channels != 6) {
		return nil, false
	}
	sampleRate, ok := jsonString(object, "sample_rate", true)
	if !ok || sampleRate == nil {
		return nil, false
	}
	if rate, err := strconv.Atoi(strings.TrimSpace(*sampleRate)); err != nil || rate <= 0 {
		return nil, false
	}
	tsPacketSize, ok := jsonString(object, "ts_packetsize", false)
	if !ok {
		return nil, false
	}
	return &ProbeAudioStream{
		CodecName:  *codecName,
		Duration:   duration,
		Profile:    profile,
		Channels:   *channels,
		SampleRate: *sampleRate,
		TSPacket:   tsPacketSize,
	}, true
}

// parseOtherStream は FFprobeOtherStream として検証する。失敗した場合は ok = false 。
func parseOtherStream(object map[string]any) (*ProbeOtherStream, bool) {
	if _, ok := jsonInt(object, "index", true); !ok {
		return nil, false
	}
	other := &ProbeOtherStream{CodecType: "unknown", CodecName: "unknown"}
	if value, exists := object["codec_type"]; exists && value != nil {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		other.CodecType = text
	}
	if value, exists := object["codec_name"]; exists && value != nil {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		other.CodecName = text
	}
	tsPacketSize, ok := jsonString(object, "ts_packetsize", false)
	if !ok {
		return nil, false
	}
	other.TSPacket = tsPacketSize
	return other, true
}

// parseStreams は streams 配列を Union (映像 | 音声 | その他) として検証する。
func parseStreams(raw any) ([]ProbeStream, error) {
	if raw == nil {
		return []ProbeStream{}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("streams must be a list")
	}
	streams := make([]ProbeStream, 0, len(items))
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("streams[%d] must be an object", index)
		}
		if video, ok := parseVideoStream(object); ok {
			streams = append(streams, ProbeStream{Video: video})
			continue
		}
		if audio, ok := parseAudioStream(object); ok {
			streams = append(streams, ProbeStream{Audio: audio})
			continue
		}
		if other, ok := parseOtherStream(object); ok {
			streams = append(streams, ProbeStream{Other: other})
			continue
		}
		return nil, fmt.Errorf("streams[%d] is invalid", index)
	}
	return streams, nil
}

// ParseProbeResult は FFprobe の全体解析 JSON を FFprobeResult として検証する。
func ParseProbeResult(data []byte) (*ProbeResult, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	formatObject, ok := root["format"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("format is required")
	}
	formatName, ok := jsonString(formatObject, "format_name", true)
	if !ok || formatName == nil {
		return nil, fmt.Errorf("format_name is required")
	}
	// H.264/H.265 + AAC が入った映像のみサポート
	if *formatName != "mpegts" && !strings.Contains(*formatName, "mp4") {
		return nil, fmt.Errorf("Unsupported container format: %s", *formatName)
	}
	duration, ok := jsonString(formatObject, "duration", false)
	if !ok {
		return nil, fmt.Errorf("format.duration is invalid")
	}
	for _, key := range []string{"size", "bit_rate"} {
		if _, ok := jsonString(formatObject, key, false); !ok {
			return nil, fmt.Errorf("format.%s is invalid", key)
		}
	}
	streams, err := parseStreams(root["streams"])
	if err != nil {
		return nil, err
	}
	result := &ProbeResult{
		Format:   ProbeFormat{FormatName: *formatName, Duration: duration},
		Streams:  streams,
		Programs: []ProbeProgram{},
	}
	if rawPrograms, exists := root["programs"]; exists && rawPrograms != nil {
		items, ok := rawPrograms.([]any)
		if !ok {
			return nil, fmt.Errorf("programs must be a list")
		}
		for index, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("programs[%d] must be an object", index)
			}
			program := ProbeProgram{}
			for _, field := range []struct {
				key    string
				target **int
			}{{"program_num", &program.ProgramNum}, {"nb_streams", &program.NbStreams}, {"pcr_pid", &program.PCRPID}} {
				value, ok := jsonInt(object, field.key, false)
				if !ok {
					return nil, fmt.Errorf("programs[%d].%s is invalid", index, field.key)
				}
				*field.target = value
			}
			for _, key := range []string{"program_id", "pmt_pid"} {
				if _, ok := jsonInt(object, key, false); !ok {
					return nil, fmt.Errorf("programs[%d].%s is invalid", index, key)
				}
			}
			result.Programs = append(result.Programs, program)
		}
	}
	return result, nil
}

// ParseProbeSampleResult は FFprobe の部分解析 JSON を FFprobeSampleResult として検証する。
func ParseProbeSampleResult(data []byte) (*ProbeSampleResult, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	streams, err := parseStreams(root["streams"])
	if err != nil {
		return nil, err
	}
	return &ProbeSampleResult{Streams: streams}, nil
}
