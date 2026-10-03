// Package stream はライブストリーミングとビデオストリーミングのエンコード処理を提供する。
//
// 移植元: server/app/streams/ (LiveStream.py / LiveEncodingTask.py / StreamEncodingOptions.py)
// および server/app/constants.py の QUALITY テーブル。
package stream

import "fmt"

// Quality は映像と音声の品質を表す (server/app/constants.py の Quality モデル相当) 。
type Quality struct {
	// IsHEVC は映像コーデックが HEVC かどうか。
	IsHEVC bool
	// Is60fps はフレームレートが 60fps かどうか。
	Is60fps bool
	// Width は横解像度。
	Width int
	// Height は縦解像度。
	Height int
	// VideoBitrate は映像のビットレート。
	VideoBitrate string
	// VideoBitrateMax は映像の最大ビットレート。
	VideoBitrateMax string
	// AudioBitrate は音声のビットレート。
	AudioBitrate string
}

// QualityTypes は QUALITY に定義されているベース画質の一覧 (Python 版の QUALITY_TYPES と同じ並び) 。
var QualityTypes = []string{
	"1080p-60fps",
	"1080p-60fps-hevc",
	"1080p",
	"1080p-hevc",
	"810p",
	"810p-hevc",
	"720p",
	"720p-hevc",
	"540p",
	"540p-hevc",
	"480p",
	"480p-hevc",
	"360p",
	"360p-hevc",
	"240p",
	"240p-hevc",
}

// Qualities は映像と音声の品質の定義 (server/app/constants.py の QUALITY と一致させること) 。
var Qualities = map[string]Quality{
	"1080p-60fps":      {IsHEVC: false, Is60fps: true, Width: 1440, Height: 1080, VideoBitrate: "9500K", VideoBitrateMax: "13000K", AudioBitrate: "256K"},
	"1080p-60fps-hevc": {IsHEVC: true, Is60fps: true, Width: 1440, Height: 1080, VideoBitrate: "3500K", VideoBitrateMax: "5200K", AudioBitrate: "192K"},
	"1080p":            {IsHEVC: false, Is60fps: false, Width: 1440, Height: 1080, VideoBitrate: "9500K", VideoBitrateMax: "13000K", AudioBitrate: "256K"},
	"1080p-hevc":       {IsHEVC: true, Is60fps: false, Width: 1440, Height: 1080, VideoBitrate: "3000K", VideoBitrateMax: "4500K", AudioBitrate: "192K"},
	"810p":             {IsHEVC: false, Is60fps: false, Width: 1440, Height: 810, VideoBitrate: "5500K", VideoBitrateMax: "7600K", AudioBitrate: "192K"},
	"810p-hevc":        {IsHEVC: true, Is60fps: false, Width: 1440, Height: 810, VideoBitrate: "2500K", VideoBitrateMax: "3700K", AudioBitrate: "192K"},
	"720p":             {IsHEVC: false, Is60fps: false, Width: 1280, Height: 720, VideoBitrate: "4500K", VideoBitrateMax: "6200K", AudioBitrate: "192K"},
	"720p-hevc":        {IsHEVC: true, Is60fps: false, Width: 1280, Height: 720, VideoBitrate: "2000K", VideoBitrateMax: "3000K", AudioBitrate: "192K"},
	"540p":             {IsHEVC: false, Is60fps: false, Width: 960, Height: 540, VideoBitrate: "3000K", VideoBitrateMax: "4100K", AudioBitrate: "192K"},
	"540p-hevc":        {IsHEVC: true, Is60fps: false, Width: 960, Height: 540, VideoBitrate: "1400K", VideoBitrateMax: "2100K", AudioBitrate: "192K"},
	"480p":             {IsHEVC: false, Is60fps: false, Width: 854, Height: 480, VideoBitrate: "2000K", VideoBitrateMax: "2800K", AudioBitrate: "192K"},
	"480p-hevc":        {IsHEVC: true, Is60fps: false, Width: 854, Height: 480, VideoBitrate: "1050K", VideoBitrateMax: "1750K", AudioBitrate: "192K"},
	"360p":             {IsHEVC: false, Is60fps: false, Width: 640, Height: 360, VideoBitrate: "1100K", VideoBitrateMax: "1800K", AudioBitrate: "128K"},
	"360p-hevc":        {IsHEVC: true, Is60fps: false, Width: 640, Height: 360, VideoBitrate: "750K", VideoBitrateMax: "1250K", AudioBitrate: "128K"},
	"240p":             {IsHEVC: false, Is60fps: false, Width: 426, Height: 240, VideoBitrate: "550K", VideoBitrateMax: "650K", AudioBitrate: "128K"},
	"240p-hevc":        {IsHEVC: true, Is60fps: false, Width: 426, Height: 240, VideoBitrate: "450K", VideoBitrateMax: "650K", AudioBitrate: "128K"},
}

// OriginalQuality は放送波の MPEG-2 TS を再エンコードせずに出力する特別な画質。
const OriginalQuality = "original"

// IsValidQuality はベース画質として有効な値かどうかを返す。
func IsValidQuality(quality string) bool {
	_, ok := Qualities[quality]
	return ok
}

// StreamEncodingOptions はライブ/録画ストリームでベース画質に追加するエンコードオプションを表す。
// server/app/streams/StreamEncodingOptions.py の StreamEncodingOptions と一致させること。
type StreamEncodingOptions struct {
	// IsHEVC10bitEnabled は HEVC 10bit を要求するクライアント向けのストリームかどうか。
	IsHEVC10bitEnabled bool
	// Is24fpsModeEnabled は 24fps モードを適用するストリームかどうか。
	Is24fpsModeEnabled bool
}

// NewStreamEncodingOptionsFromRequest は API で指定されたオプションから、実際に使うストリームオプションを作る。
func NewStreamEncodingOptionsFromRequest(quality string, isHEVC10bitRequested bool, is24fpsModeRequested bool, encoder string) StreamEncodingOptions {
	qualityInfo := Qualities[quality]

	// HEVC 10bit は通信節約モードで使う HEVC 画質かつ QSVEncC / NVEncC の場合だけ有効化する。
	// VCEEncC は HEVC 10bit 対応の機種かを判定できず、rkmppenc は HEVC 10bit エンコード自体に非対応のため設定しない。
	isHEVC10bitEnabled := isHEVC10bitRequested && qualityInfo.IsHEVC && (encoder == "QSVEncC" || encoder == "NVEncC")

	// 24fps モードは 60fps 画質以外で有効化する。
	is24fpsModeEnabled := is24fpsModeRequested && !qualityInfo.Is60fps

	return StreamEncodingOptions{
		IsHEVC10bitEnabled: isHEVC10bitEnabled,
		Is24fpsModeEnabled: is24fpsModeEnabled,
	}
}

// BuildSuffix はライブストリーム ID の末尾に付ける文字列を組み立てる。
func (o StreamEncodingOptions) BuildSuffix() string {
	suffix := ""
	if o.IsHEVC10bitEnabled {
		suffix += "-10bit"
	}
	if o.Is24fpsModeEnabled {
		suffix += "-24fps"
	}
	return suffix
}

// StreamQualityWithOptions はストリーミング配信 API の品質指定を、ベース画質と追加エンコードオプションへ分解した結果。
type StreamQualityWithOptions struct {
	// Quality は QUALITY に定義されているベース画質。
	Quality string
	// EncodingOptions はベース画質に追加するエンコードオプション。
	EncodingOptions StreamEncodingOptions
}

// SplitQualityAndEncodingOptions は API パスの品質指定 (例: 720p-hevc-10bit-24fps) を、
// ベース画質 (720p-hevc) と追加オプション (-10bit / -24fps) に分解する。
// 不正な品質指定の場合は ok に false を返す。
func SplitQualityAndEncodingOptions(quality string, encoder string) (StreamQualityWithOptions, bool) {
	// オリジナル画質が指定された場合は確実にライブ配信からなので特別扱い。
	if quality == OriginalQuality {
		return StreamQualityWithOptions{Quality: OriginalQuality}, true
	}

	// -10bit / -24fps は BuildSuffix() と同じ順序でのみ受け付ける。
	// 末尾から剥がすことで、1080p-60fps-hevc のようにベース画質自体が -hevc を含むケースを安全に扱う。
	baseQuality := quality
	is24fpsModeRequested := false
	if len(baseQuality) >= len("-24fps") && baseQuality[len(baseQuality)-len("-24fps"):] == "-24fps" {
		baseQuality = baseQuality[:len(baseQuality)-len("-24fps")]
		is24fpsModeRequested = true
	}
	isHEVC10bitRequested := false
	if len(baseQuality) >= len("-10bit") && baseQuality[len(baseQuality)-len("-10bit"):] == "-10bit" {
		baseQuality = baseQuality[:len(baseQuality)-len("-10bit")]
		isHEVC10bitRequested = true
	}

	// ベース画質が QUALITY に存在しない場合は、ルーター側で従来通り 422 を返す。
	if !IsValidQuality(baseQuality) {
		return StreamQualityWithOptions{}, false
	}

	return StreamQualityWithOptions{
		Quality:         baseQuality,
		EncodingOptions: NewStreamEncodingOptionsFromRequest(baseQuality, isHEVC10bitRequested, is24fpsModeRequested, encoder),
	}, true
}

// String はデバッグ用の文字列表現を返す。
func (q Quality) String() string {
	return fmt.Sprintf("%dx%d hevc=%v 60fps=%v", q.Width, q.Height, q.IsHEVC, q.Is60fps)
}
