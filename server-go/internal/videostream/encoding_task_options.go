package videostream

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// 録画エンコードタスクの定数 (移植元: VideoEncodingTask のクラス変数) 。
const (
	// EncodingGOPLengthSecond はエンコード後のストリームの GOP 長 (秒) 。
	// ライブではないため、GOP 長は H.264 / H.265 共通で長めに設定する (VideoEncodingTask.GOP_LENGTH_SECOND) 。
	EncodingGOPLengthSecond = 3.0
	// MaxEncodingRetryCount はエンコードタスクの最大リトライ回数 (VideoEncodingTask.MAX_RETRY_COUNT) 。
	MaxEncodingRetryCount = 10
)

// EncodingVideoInfo はエンコードのコマンドライン組み立てに必要な録画ファイルの映像情報 (RecordedVideo の一部) 。
type EncodingVideoInfo struct {
	// FilePath は録画ファイルのパス。
	FilePath string
	// ContainerFormat はコンテナ形式 (MPEG-TS / MPEG-4) 。
	ContainerFormat string
	// VideoCodec は映像コーデック (MPEG-2 / H.264 / H.265) 。
	VideoCodec string
	// VideoScanType はスキャン方式 (Interlaced / Progressive) 。
	VideoScanType string
	// VideoFrameRate はフレームレート。
	VideoFrameRate float64
	// VideoResolutionWidth と VideoResolutionHeight は代表解像度。
	VideoResolutionWidth  int
	VideoResolutionHeight int
	// HasVideoStreamChanges は映像ストリーム構成が途中で変わる録画かどうか。
	HasVideoStreamChanges bool
}

// EncodingChannel は録画に紐づくチャンネルの情報 (psisimux / tsreadex の引数に使う) 。
type EncodingChannel struct {
	NetworkID         int
	TransportStreamID *int
	ServiceID         int
}

// PythonFloatRepr は float を Python 3 の repr() / str() と同じ文字列にする。
// -output_ts_offset などに Python は f'{float}' で埋め込むため、同じ表記にしないと引数が一致しない。
// (例: 0 → "0.0", 12.345 → "12.345", 1e16 → "1e+16", 1.1e-05 → "1.1e-05")
func PythonFloatRepr(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	case value == 0:
		if math.Signbit(value) {
			return "-0.0"
		}
		return "0.0"
	}
	// 最短桁の指数表記 (d.ddde±XX) から仮数と指数を取り出す
	text := strconv.FormatFloat(math.Abs(value), 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(text, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)

	sign := ""
	if value < 0 {
		sign = "-"
	}
	// Python は 10^-4 <= |x| < 10^16 を固定小数点表記、それ以外を指数表記にする
	if exponent >= -4 && exponent < 16 {
		if exponent >= 0 {
			integerPart := digits
			if len(integerPart) < exponent+1 {
				integerPart += strings.Repeat("0", exponent+1-len(integerPart))
			}
			fraction := digits[min(len(digits), exponent+1):]
			if fraction == "" {
				fraction = "0"
			}
			return sign + integerPart[:exponent+1] + "." + fraction
		}
		return sign + "0." + strings.Repeat("0", -exponent-1) + digits
	}
	body := digits[:1]
	if len(digits) > 1 {
		body += "." + digits[1:]
	}
	exponentSign := "+"
	if exponent < 0 {
		exponentSign = "-"
		exponent = -exponent
	}
	return fmt.Sprintf("%s%se%s%02d", sign, body, exponentSign, exponent)
}

// splitOptions は Python 版と同じく、各オプション文字列をスペースで区切って 1 つの引数列にする。
func splitOptions(options []string) []string {
	result := []string{}
	for _, option := range options {
		result = append(result, strings.Split(option, " ")...)
	}
	return result
}

// encodingOutputResolution は出力解像度を返す。
// 指定された品質の解像度が 1440×1080 (1080p) かつ入力ストリームがフル HD (1920×1080) の場合のみ、
// 特別に横解像度を 1920 に変更してフル HD (1920×1080) でエンコードする。
func encodingOutputResolution(qualityInfo stream.Quality, video EncodingVideoInfo) (width int, height int) {
	width, height = qualityInfo.Width, qualityInfo.Height
	if width == 1440 && height == 1080 && video.VideoResolutionWidth == 1920 && video.VideoResolutionHeight == 1080 {
		width = 1920
	}
	return width, height
}

// BuildVideoFFmpegOptions は録画再生用に FFmpeg へ渡すオプションを組み立てる。
// 移植元: VideoEncodingTask.buildFFmpegOptions()
// (ライブ用の stream.BuildFFmpegOptions とは GOP 長・max_delay・max_interleave_delta・-flags 等が異なるため別実装)
func BuildVideoFFmpegOptions(quality string, video EncodingVideoInfo, encodingOptions stream.StreamEncodingOptions, retryCount int, outputTSOffset float64) ([]string, error) {
	qualityInfo, ok := stream.Qualities[quality]
	if !ok {
		return nil, fmt.Errorf("unknown quality: %q", quality)
	}
	options := []string{}

	// 入力ストリームの解析時間 (リトライ回数に応じて少し増やす)
	analyzeduration := 500000 + retryCount*500000
	if video.VideoCodec != "MPEG-2" {
		// MPEG-2 以外のコーデックでは入力ストリームの解析時間を長めにする (その方がうまくいく)
		analyzeduration += 1000000
	}

	// 入力
	options = append(options, fmt.Sprintf("-f mpegts -analyzeduration %d -i pipe:0", analyzeduration))

	// ストリームのマッピング (音声切り替えのため、主音声・副音声両方をエンコード後の TS に含む)
	options = append(options, "-map 0:v:0 -map 0:a:0 -map 0:a:1 -map 0:d? -ignore_unknown")

	// フラグ
	// 録画再生では max_interleave_delta が大きめでないと映像/音声のずれが大きくなりセグメント分割時に問題が生じるため、
	// 5000K (5秒) に設定し、リトライ回数に応じて 1000K (1秒) ずつ増やす
	maxInterleaveDelta := 5000 + retryCount*1000
	options = append(options, fmt.Sprintf("-fflags nobuffer -flags low_delay -max_delay 0 -tune zerolatency -max_interleave_delta %dK -threads auto", maxInterleaveDelta))

	// 映像: コーデック
	if qualityInfo.IsHEVC {
		options = append(options, "-vcodec libx265")
	} else {
		options = append(options, "-vcodec libx264")
	}

	// 映像: ビットレートと品質
	options = append(options, fmt.Sprintf("-flags +cgop+global_header -vb %s -maxrate %s", qualityInfo.VideoBitrate, qualityInfo.VideoBitrateMax))
	options = append(options, "-preset veryfast -aspect 16:9 -pix_fmt:v yuv420p")
	if qualityInfo.IsHEVC {
		options = append(options, "-profile:v main")
	} else {
		options = append(options, "-profile:v high")
	}

	videoWidth, videoHeight := encodingOutputResolution(qualityInfo, video)
	gop := EncodingGOPLengthSecond

	switch video.VideoScanType {
	case "Interlaced":
		if qualityInfo.Is60fps {
			// インターレース解除 (60i → 60p)
			options = append(options, fmt.Sprintf("-vf yadif=mode=1:parity=-1:deint=1,scale=%d:%d", videoWidth, videoHeight))
			options = append(options, fmt.Sprintf("-r 60000/1001 -g %d", int(gop*60)))
		} else if encodingOptions.Is24fpsModeEnabled {
			// 24fps モードでは、テレシネ由来の重複フレームを取り除いて 24/30p 混合 VFR で出力する
			options = append(options, fmt.Sprintf("-vf pullup,dejudder,scale=%d:%d", videoWidth, videoHeight))
			options = append(options, fmt.Sprintf("-fps_mode vfr -g %d", int(gop*30)))
		} else {
			// インターレース解除 (60i → 30p)
			options = append(options, fmt.Sprintf("-vf yadif=mode=0:parity=-1:deint=1,scale=%d:%d", videoWidth, videoHeight))
			options = append(options, fmt.Sprintf("-r 30000/1001 -g %d", int(gop*30)))
		}
	case "Progressive":
		// プログレッシブ映像の場合は 60fps 化する方法はないため、無視して入力ファイルと同じ fps でエンコードする
		intFPS := int(math.Ceil(video.VideoFrameRate)) // 29.97 -> 30
		options = append(options, fmt.Sprintf("-vf scale=%d:%d", videoWidth, videoHeight))
		options = append(options, fmt.Sprintf("-g %d", int(gop*float64(intFPS))))
	}

	// 音声 (音声が 5.1ch かどうかに関わらず、ステレオにダウンミックスする)
	options = append(options, fmt.Sprintf("-acodec aac -aac_coder twoloop -ac 2 -ab %s -ar 48000 -af volume=2.0", qualityInfo.AudioBitrate))

	// 出力 TS のタイムスタンプオフセット
	options = append(options, "-output_ts_offset "+PythonFloatRepr(outputTSOffset))

	// 出力 (MPEG-TS 出力ということを明示し、標準出力へ出力する)
	options = append(options, "-y -f mpegts")
	options = append(options, "pipe:1")

	return splitOptions(options), nil
}

// BuildVideoHWEncCOptions は録画再生用に QSVEncC・NVEncC・VCEEncC・rkmppenc へ渡すオプションを組み立てる。
// 移植元: VideoEncodingTask.buildHWEncCOptions()
//
// isOptionAvailable はエンコーダーが指定されたオプション (--adapt-resolution) に対応しているかを返す関数。
func BuildVideoHWEncCOptions(quality string, encoderType string, video EncodingVideoInfo, encodingOptions stream.StreamEncodingOptions, retryCount int, outputTSOffset float64, isOptionAvailable func(option string) bool) ([]string, error) {
	qualityInfo, ok := stream.Qualities[quality]
	if !ok {
		return nil, fmt.Errorf("unknown quality: %q", quality)
	}
	options := []string{}

	// 入力ストリームの解析時間 (リトライ回数に応じて少し増やす)
	inputProbesize := 1000 + retryCount*1000
	inputAnalyze := math.Round((0.7+float64(retryCount)*1)*10) / 10
	if video.VideoCodec != "MPEG-2" {
		// MPEG-2 以外のコーデックでは入力ストリームの解析時間を長めにする (その方がうまくいく)
		inputProbesize += 2000
		inputAnalyze += 6.3
	}

	// 入力 (--input-probesize と --input-analyze は両方つけるのが重要)
	options = append(options, fmt.Sprintf("--input-format mpegts --input-probesize %dK --input-analyze %s --input -", inputProbesize, PythonFloatRepr(inputAnalyze)))
	// VCEEncC の HW デコーダーはエラー耐性が低く TS を扱う用途では不安定なので、SW デコーダーを利用する
	if encoderType == "VCEEncC" {
		options = append(options, "--avsw")
	} else {
		options = append(options, "--avhw")
	}
	// 入力途中の解像度変更に備えて、デコーダー/入力サーフェスの最大確保解像度を指定する (対応している場合のみ)
	if isOptionAvailable("--adapt-resolution") {
		if video.VideoResolutionWidth >= 3840 || video.VideoResolutionHeight >= 2160 {
			options = append(options, "--adapt-resolution 3840x2160")
		} else {
			options = append(options, "--adapt-resolution 1920x1080")
		}
	}

	// ストリームのマッピング (音声が 5.1ch かどうかに関わらず、ステレオにダウンミックスする)
	options = append(options, "--audio-stream 1?:stereo --audio-stream 2?:stereo --data-copy timed_id3")

	// フラグ
	maxInterleaveDelta := 5000 + retryCount*1000
	options = append(options, "-m avioflags:direct -m fflags:nobuffer+flush_packets -m flush_packets:1 -m max_delay:0")
	options = append(options, fmt.Sprintf("-m max_interleave_delta:%dK", maxInterleaveDelta))
	// QSVEncC と rkmppenc では OpenCL を使用しないので、無効化することで初期化フェーズを高速化する
	if (encoderType == "QSVEncC" || encoderType == "rkmppenc") && !encodingOptions.Is24fpsModeEnabled {
		options = append(options, "--disable-opencl")
	}
	// NVEncC では NVML によるモニタリングと DX11, Vulkan を無効化することで初期化フェーズを高速化する
	if encoderType == "NVEncC" {
		options = append(options, "--disable-nvml 1 --disable-dx11 --disable-vulkan")
	}

	// 映像: コーデック
	if qualityInfo.IsHEVC {
		options = append(options, "--codec hevc")
	} else {
		options = append(options, "--codec h264")
	}

	// 映像: ビットレート (H.265/HEVC かつ QSVEncC の場合のみ --qvbr 、それ以外は --vbr)
	if qualityInfo.IsHEVC && encoderType == "QSVEncC" {
		options = append(options, fmt.Sprintf("--qvbr %s --fallback-rc", qualityInfo.VideoBitrate))
	} else {
		options = append(options, fmt.Sprintf("--vbr %s", qualityInfo.VideoBitrate))
	}
	options = append(options, fmt.Sprintf("--max-bitrate %s", qualityInfo.VideoBitrateMax))

	// 映像: H.265/HEVC の高圧縮化調整
	if qualityInfo.IsHEVC {
		switch encoderType {
		case "QSVEncC":
			options = append(options, "--qvbr-quality 20 --extbrc --mbbrc --scenario-info game_streaming --tune perceptual")
			options = append(options, "--i-adapt --b-adapt --b-pyramid --weightp --weightb --adapt-ref --adapt-ltr --adapt-cqm")
		case "NVEncC":
			// --weightp は過去の GPU 世代で不安定な場合があるので使用しない
			options = append(options, "--qp-min 23:26:30 --lookahead 16 --multipass 2pass-full --bref-mode middle --aq --aq-temporal")
		}
	}

	// 映像: ヘッダ情報制御 (VCEEncC ではデフォルトで有効であり、当該オプションは存在しない)
	if encoderType != "VCEEncC" {
		options = append(options, "--repeat-headers")
	}

	// 映像: GOP 長を固定 (VCEEncC / rkmppenc では下記オプションは存在しない)
	switch encoderType {
	case "QSVEncC":
		options = append(options, "--strict-gop")
	case "NVEncC":
		options = append(options, "--no-i-adapt")
	}

	// 映像: 品質
	switch encoderType {
	case "QSVEncC":
		options = append(options, "--quality balanced")
	case "NVEncC":
		options = append(options, "--preset default")
	case "VCEEncC":
		options = append(options, "--preset balanced")
	case "rkmppenc":
		options = append(options, "--preset best")
	}
	if qualityInfo.IsHEVC {
		options = append(options, "--profile main")
	} else {
		options = append(options, "--profile high")
	}
	options = append(options, "--dar 16:9")

	// 映像: バンディング軽減のためのオプション (速度低下を鑑みて当面 NVEncC でのみ有効にする)
	if encoderType == "NVEncC" {
		options = append(options, "--vpp-deband")
	}
	// 通信節約モードでは、HEVC 10bit のデコードに対応したクライアント向けに HEVC 10bit でエンコードする
	// (--fallback-bitdepth により、GPU 側が HEVC 10bit 非対応の場合でも 8bit へフォールバックされる)
	if qualityInfo.IsHEVC && encodingOptions.IsHEVC10bitEnabled {
		options = append(options, "--output-depth 10 --fallback-bitdepth")
	}

	gop := EncodingGOPLengthSecond
	switch video.VideoScanType {
	case "Interlaced":
		// インターレース映像として読み込む
		options = append(options, "--interlace tff")
		if qualityInfo.Is60fps {
			// インターレース解除 (60i → 60p)
			switch encoderType {
			case "QSVEncC":
				options = append(options, "--vpp-deinterlace bob")
			case "NVEncC", "VCEEncC":
				options = append(options, "--vpp-yadif mode=bob")
			case "rkmppenc":
				options = append(options, "--vpp-deinterlace bob_i5")
			}
			options = append(options, fmt.Sprintf("--avsync vfr --gop-len %d", int(gop*60)))
		} else {
			// インターレース解除 (60i → 30p)
			if encodingOptions.Is24fpsModeEnabled {
				// 24fps モードでは --vpp-afs で 24fps 区間を検出し、24/30p 混合 VFR で出力する
				options = append(options, "--vpp-afs preset=default,drop=on,smooth=on")
			} else {
				switch encoderType {
				case "QSVEncC":
					options = append(options, "--vpp-deinterlace normal")
				case "NVEncC", "VCEEncC":
					options = append(options, "--vpp-afs preset=default,coeff_shift=0")
				case "rkmppenc":
					options = append(options, "--vpp-deinterlace normal_i5")
				}
			}
			options = append(options, fmt.Sprintf("--avsync vfr --gop-len %d", int(gop*30)))
		}
	case "Progressive":
		// プログレッシブ映像の場合は 60fps 化する方法はないため、無視して入力ファイルと同じ fps でエンコードする
		intFPS := int(math.Ceil(video.VideoFrameRate)) // 29.97 -> 30
		options = append(options, fmt.Sprintf("--avsync vfr --gop-len %d", int(gop*float64(intFPS))))
	}

	videoWidth, videoHeight := encodingOutputResolution(qualityInfo, video)
	options = append(options, fmt.Sprintf("--output-res %dx%d", videoWidth, videoHeight))

	// 音声
	options = append(options, fmt.Sprintf("--audio-codec aac:aac_coder=twoloop --audio-bitrate %s", qualityInfo.AudioBitrate))
	options = append(options, "--audio-samplerate 48000 --audio-filter volume=2.0 --audio-ignore-decode-error 30")

	// 出力 TS のタイムスタンプオフセット
	options = append(options, "-m output_ts_offset:"+PythonFloatRepr(outputTSOffset))
	// dts 合わせにするため、B フレームによる pts-dts ずれ量を補正する
	options = append(options, "--offset-video-dts-advance")

	// 出力 (MPEG-TS 出力ということを明示し、標準出力へ出力する)
	options = append(options, "--output-format mpegts")
	options = append(options, "--output -")

	return splitOptions(options), nil
}

// BuildTSReadExOptions は録画再生用の tsreadex の引数を組み立てる。
// serviceID は `-n` に渡すサービス ID (チャンネルが分からない場合は "-1") 。
// 移植元: VideoEncodingTask.run() 内の tsreadex_options
func BuildTSReadExOptions(serviceID string) []string {
	return []string{
		// 取り除く TS パケットの10進数の PID (EIT の PID を指定)
		"-x", "18/38/39",
		// 特定サービスのみを選択して出力するフィルタを有効にする
		"-n", serviceID,
		// 主音声ストリームが常に存在する状態にする (+1: 無し→無音 AAC, +4: モノラル→ステレオ, +8: デュアルモノ分離)
		"-a", "13",
		// 副音声ストリームが常に存在する状態にする (+1: 無し→無音 AAC, +4: モノラル→ステレオ)
		"-b", "5",
		// 字幕ストリームが常に存在する状態にする (+1: PMT の項目を補う, +4: 5秒ごとに非表示のデータを挿入)
		"-c", "5",
		// 文字スーパーストリームが常に存在する状態にする
		"-u", "1",
		// 字幕と文字スーパーを aribb24.js が解釈できる ID3 timed-metadata に変換する (+8: PTS を単調増加に調整)
		"-d", "9",
		// 標準入力からの入力を受け付ける
		"-",
	}
}

// ResolveMP4BroadcastIDs は MP4 録画で psisimux に渡す `-b` の値と、tsreadex の `-n` に渡すサービス ID を返す。
// 移植元: VideoEncodingTask.run() の MPEG-4 経路
func ResolveMP4BroadcastIDs(channel *EncodingChannel) (psisimuxBroadcastID string, tsreadexServiceID string) {
	if channel == nil {
		// チャンネルが紐づかない場合は、合成 TS の先頭サービスを選べば映像・音声の抽出には支障がない
		return "1/2/3", "-1"
	}
	// TSID が DB にない場合だけ、psisimux の数値パースを通せる範囲内の未使用値として 65535 を入れる
	transportStreamID := 65535
	if channel.TransportStreamID != nil {
		transportStreamID = *channel.TransportStreamID
	}
	return fmt.Sprintf("%d/%d/%d", channel.NetworkID, transportStreamID, channel.ServiceID), strconv.Itoa(channel.ServiceID)
}

// BuildPsisimuxOptions は MP4 録画を MPEG-TS に合成する psisimux の引数を組み立てる。
// 移植元: VideoEncodingTask.run() 内の psisimux_options
func BuildPsisimuxOptions(outputTSOffsetSeconds float64, broadcastID string, filePath string) []string {
	return []string{
		// 出力ファイルのミリ秒単位の初期シーク量 (Python の int() と同じく 0 方向への切り捨て)
		"-m", strconv.FormatInt(int64(outputTSOffsetSeconds*1000), 10),
		// NetworkID/TransportStreamID/ServiceID
		"-b", broadcastID,
		// 文字コードが UTF-8 の字幕を ARIB 規格の8単位符号に変換する
		"-8",
		// 字幕ファイルの拡張子
		"-x", ".vtt",
		// 入力ファイル名
		filePath,
		// 標準出力
		"-",
	}
}

// ResolveEncoderType は実際に使うエンコーダーを返す。
//
// pinFFmpegOnVideoStreamChanges が true の場合、MPEG-TS かつ映像ストリーム構成が途中で変わる録画 (has_video_stream_changes)
// では HWEncC 系ではなく FFmpeg に固定する。これは Python 版のコミット 96f2f506 で入ったロジックだが、
// 同 fc98a36a (HWEncC の可変解像度対応 = --adapt-resolution) で削除されており、現行の Python 版には存在しない。
// 現行 Python と同じ挙動にするため、通常は false を渡す。
func ResolveEncoderType(configured string, containerFormat string, hasVideoStreamChanges bool, pinFFmpegOnVideoStreamChanges bool) string {
	if pinFFmpegOnVideoStreamChanges && containerFormat == "MPEG-TS" && hasVideoStreamChanges && configured != "FFmpeg" {
		return "FFmpeg"
	}
	return configured
}
