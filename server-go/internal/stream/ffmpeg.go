package stream

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GOPLengthSecondsH264 は H.264 再生時のエンコード後のストリームの GOP 長 (秒) 。
const GOPLengthSecondsH264 = 0.5

// GOPLengthSecondsH265 は H.265 再生時のエンコード後のストリームの GOP 長 (秒) 。
const GOPLengthSecondsH265 = 2.0

// BuildFFmpegOptions は FFmpeg に渡すオプションを組み立てる。
// 移植元: LiveEncodingTask.buildFFmpegOptions()
func BuildFFmpegOptions(quality string, channelType string, isFullHDChannel bool, encodingOptions StreamEncodingOptions, retryCount int) []string {
	qualityInfo := Qualities[quality]
	options := []string{}

	// 入力ストリームの解析時間 (リトライ回数に応じて少し増やす) 。
	analyzeduration := 500000 + retryCount*200000
	if channelType == "SKY" {
		// スカパー！プレミアムサービスのチャンネルは入力ストリームの解析時間を長めにする。
		analyzeduration += 200000
	}

	// 入力
	options = append(options, "-f", "mpegts", "-analyzeduration", fmt.Sprintf("%d", analyzeduration), "-i", "pipe:0")

	// ストリームのマッピング (音声切り替えのため、主音声・副音声両方をエンコード後の TS に含む) 。
	options = append(options, "-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:1", "-map", "0:d?", "-ignore_unknown")

	// フラグ (主に FFmpeg の起動を高速化するための設定) 。
	maxInterleaveDelta := 500 + retryCount*100
	options = append(options,
		"-fflags", "nobuffer", "-flags", "low_delay", "-max_delay", "250000",
		"-max_interleave_delta", fmt.Sprintf("%dK", maxInterleaveDelta), "-threads", "auto")

	// 映像: コーデック
	if qualityInfo.IsHEVC {
		options = append(options, "-vcodec", "libx265")
	} else {
		options = append(options, "-vcodec", "libx264")
	}

	// 映像: ビットレートと品質
	options = append(options, "-flags", "+cgop", "-vb", qualityInfo.VideoBitrate, "-maxrate", qualityInfo.VideoBitrateMax)
	options = append(options, "-preset", "veryfast", "-aspect", "16:9")
	if qualityInfo.IsHEVC {
		options = append(options, "-profile:v", "main")
	} else {
		options = append(options, "-profile:v", "high")
	}

	// フル HD 放送が行われているチャンネルかつ、指定された品質の解像度が 1440×1080 (1080p) の場合のみ、
	// 特別に横解像度を 1920 に変更してフル HD (1920×1080) でエンコードする。
	videoWidth := qualityInfo.Width
	videoHeight := qualityInfo.Height
	if videoWidth == 1440 && videoHeight == 1080 && isFullHDChannel {
		videoWidth = 1920
	}

	// 最大 GOP 長 (秒) 。H.265/HEVC では高圧縮化のため、最大 GOP 長を長くする。
	gopLengthSecond := GOPLengthSecondsH264
	if qualityInfo.IsHEVC {
		gopLengthSecond = GOPLengthSecondsH265
	}

	if channelType == "BS4K" {
		// BS4K は 60p (プログレッシブ) で放送されているので、インターレース解除を行わず 60fps でエンコードする。
		options = append(options, "-vf", fmt.Sprintf("scale=%d:%d", videoWidth, videoHeight))
		options = append(options, "-r", "60000/1001", "-g", fmt.Sprintf("%d", int(gopLengthSecond*60)))
	} else if qualityInfo.Is60fps {
		// インターレース解除 (60i → 60p (フレームレート: 60fps)) 。
		options = append(options, "-vf", fmt.Sprintf("yadif=mode=1:parity=-1:deint=1,scale=%d:%d", videoWidth, videoHeight))
		options = append(options, "-r", "60000/1001", "-g", fmt.Sprintf("%d", int(gopLengthSecond*60)))
	} else if encodingOptions.Is24fpsModeEnabled {
		// 24fps モードでは、テレシネ由来の重複フレームを取り除いて 24/30p 混合 VFR で出力する。
		options = append(options, "-vf", fmt.Sprintf("pullup,dejudder,scale=%d:%d", videoWidth, videoHeight))
		options = append(options, "-fps_mode", "vfr", "-g", fmt.Sprintf("%d", int(gopLengthSecond*30)))
	} else {
		// インターレース解除 (60i → 30p (フレームレート: 30fps)) 。
		options = append(options, "-vf", fmt.Sprintf("yadif=mode=0:parity=-1:deint=1,scale=%d:%d", videoWidth, videoHeight))
		options = append(options, "-r", "30000/1001", "-g", fmt.Sprintf("%d", int(gopLengthSecond*30)))
	}

	// 音声 (音声が 5.1ch かどうかに関わらず、ステレオにダウンミックスする) 。
	options = append(options, "-acodec", "aac", "-aac_coder", "twoloop", "-ac", "2",
		"-ab", qualityInfo.AudioBitrate, "-ar", "48000", "-af", "volume=2.0")

	// 出力 (MPEG-TS 出力ということを明示し、標準出力へ出力する) 。
	options = append(options, "-y", "-f", "mpegts", "pipe:1")

	return options
}

// BuildFFmpegOptionsForRadio はラジオチャンネル向けの FFmpeg のオプションを組み立てる。
// 音声の品質は変えたところでほとんど差がないため、1つだけに固定されている。
// 移植元: LiveEncodingTask.buildFFmpegOptionsForRadio()
func BuildFFmpegOptionsForRadio(retryCount int) []string {
	options := []string{}

	// 入力
	analyzeduration := 500000 + retryCount*200000
	options = append(options, "-f", "mpegts", "-analyzeduration", fmt.Sprintf("%d", analyzeduration), "-i", "pipe:0")

	// ストリームのマッピング
	options = append(options, "-map", "0:a:0", "-map", "0:a:1", "-map", "0:d?", "-ignore_unknown")

	// フラグ
	maxInterleaveDelta := 500 + retryCount*100
	options = append(options,
		"-fflags", "nobuffer", "-flags", "low_delay", "-max_delay", "250000",
		"-max_interleave_delta", fmt.Sprintf("%dK", maxInterleaveDelta), "-threads", "auto")

	// 音声
	options = append(options, "-acodec", "aac", "-aac_coder", "twoloop", "-ac", "2",
		"-ab", "192K", "-ar", "48000", "-af", "volume=2.0")

	// 出力
	options = append(options, "-y", "-f", "mpegts", "pipe:1")

	return options
}

// BuildHWEncCOptions は QSVEncC・NVEncC・VCEEncC・rkmppenc (便宜上 HWEncC と総称) に渡すオプションを組み立てる。
// 移植元: LiveEncodingTask.buildHWEncCOptions()
//
// isOptionAvailable はエンコーダーが指定されたオプションに対応しているかを返す関数 (HWEncCOptionAvailable) 。
func BuildHWEncCOptions(quality string, encoderType string, channelType string, isFullHDChannel bool, encodingOptions StreamEncodingOptions, retryCount int, isOptionAvailable func(string) bool) []string {
	qualityInfo := Qualities[quality]
	options := []string{}

	// 入力ストリームの解析時間 (リトライ回数に応じて少し増やす) 。
	inputProbesize := 1000 + retryCount*500
	// Python 版と同じく round(value, 1) してから SKY チャンネル分を加算する。
	// SKY チャンネルでは浮動小数点の誤差が文字列に現れるが、Python 版の出力と一致させるためそのまま再現する。
	inputAnalyze := math.Round((0.7+float64(retryCount)*0.2)*10) / 10
	if channelType == "SKY" {
		inputProbesize += 500
		inputAnalyze += 0.2
	}

	// 入力
	options = append(options, "--input-format", "mpegts", "--input-probesize", fmt.Sprintf("%dK", inputProbesize),
		"--input-analyze", strconv.FormatFloat(inputAnalyze, 'g', -1, 64))
	// BS4K 以外では 29.97fps (59.94i) を指定する。
	if channelType != "BS4K" {
		options = append(options, "--fps", "30000/1001")
	}
	options = append(options, "--input", "-")
	// VCEEncC の HW デコーダーはエラー耐性が低く TS を扱う用途では不安定なので、SW デコーダーを利用する。
	if encoderType == "VCEEncC" {
		options = append(options, "--avsw")
	} else {
		options = append(options, "--avhw")
	}
	// 入力途中の解像度変更に備えて、デコーダー/入力サーフェスの最大確保解像度を指定する。
	if isOptionAvailable("--adapt-resolution") {
		if channelType == "BS4K" {
			options = append(options, "--adapt-resolution", "3840x2160")
		} else {
			options = append(options, "--adapt-resolution", "1920x1080")
		}
	}

	// ストリームのマッピング (音声が 5.1ch かどうかに関わらず、ステレオにダウンミックスする) 。
	options = append(options, "--audio-stream", "1?:stereo", "--audio-stream", "2?:stereo", "--data-copy", "timed_id3")

	// フラグ
	maxInterleaveDelta := 500 + retryCount*100
	options = append(options, "-m", "avioflags:direct", "-m", "fflags:nobuffer+flush_packets",
		"-m", "flush_packets:1", "-m", "max_delay:250000")
	options = append(options, "-m", fmt.Sprintf("max_interleave_delta:%dK", maxInterleaveDelta), "--output-thread", "0", "--lowlatency")
	// QSVEncC と rkmppenc では OpenCL を使用しないので、無効化することで初期化フェーズを高速化する。
	if (encoderType == "QSVEncC" || encoderType == "rkmppenc") && !encodingOptions.Is24fpsModeEnabled {
		options = append(options, "--disable-opencl")
	}
	// NVEncC では NVML によるモニタリングと DX11, Vulkan を無効化することで初期化フェーズを高速化する。
	if encoderType == "NVEncC" {
		options = append(options, "--disable-nvml", "1", "--disable-dx11", "--disable-vulkan")
	}
	options = append(options, "--log-level", "debug")

	// 映像: コーデック
	if qualityInfo.IsHEVC {
		options = append(options, "--codec", "hevc")
	} else {
		options = append(options, "--codec", "h264")
	}

	// 映像: ビットレート
	// H.265/HEVC かつ QSVEncC の場合のみ、--qvbr (品質ベース可変ビットレート) モードでエンコードする。
	if qualityInfo.IsHEVC && encoderType == "QSVEncC" {
		options = append(options, "--qvbr", qualityInfo.VideoBitrate, "--fallback-rc")
	} else {
		options = append(options, "--vbr", qualityInfo.VideoBitrate)
	}
	options = append(options, "--max-bitrate", qualityInfo.VideoBitrateMax)

	// 映像: H.265/HEVC の高圧縮化調整
	if qualityInfo.IsHEVC {
		if encoderType == "QSVEncC" {
			options = append(options, "--qvbr-quality", "20", "--extbrc", "--mbbrc", "--scenario-info", "game_streaming", "--tune", "perceptual")
			options = append(options, "--i-adapt", "--b-adapt", "--b-pyramid", "--weightp", "--weightb",
				"--adapt-ref", "--adapt-ltr", "--adapt-cqm")
		} else if encoderType == "NVEncC" {
			// --weightp は過去の GPU 世代で不安定な場合があるので使用しない。
			options = append(options, "--qp-min", "23:26:30", "--lookahead", "16", "--multipass", "2pass-full",
				"--bref-mode", "middle", "--aq", "--aq-temporal")
		}
	}

	// 映像: ヘッダ情報制御 (GOP ごとにヘッダを再送する) 。VCEEncC ではデフォルトで有効であり、当該オプションは存在しない。
	if encoderType != "VCEEncC" {
		options = append(options, "--repeat-headers")
	}

	// 映像: 品質
	switch encoderType {
	case "QSVEncC":
		options = append(options, "--quality", "balanced")
	case "NVEncC":
		options = append(options, "--preset", "default")
	case "VCEEncC":
		options = append(options, "--preset", "balanced")
	case "rkmppenc":
		options = append(options, "--preset", "best")
	}
	if qualityInfo.IsHEVC {
		options = append(options, "--profile", "main")
	} else {
		options = append(options, "--profile", "high")
	}
	options = append(options, "--dar", "16:9")

	// 映像: バンディング軽減のためのオプション (速度低下を鑑みて当面 NVEncC でのみ有効にする) 。
	if encoderType == "NVEncC" {
		options = append(options, "--vpp-deband")
	}
	// 通信節約モードでは、HEVC 10bit のデコードに対応したクライアント向けに HEVC 10bit でエンコードする。
	if qualityInfo.IsHEVC && encodingOptions.IsHEVC10bitEnabled {
		options = append(options, "--output-depth", "10", "--fallback-bitdepth")
	}

	// 映像: 最大 GOP 長 (秒)
	gopLengthSecond := GOPLengthSecondsH264
	if qualityInfo.IsHEVC {
		gopLengthSecond = GOPLengthSecondsH265
	}

	if channelType == "BS4K" {
		options = append(options, "--avsync", "vfr", "--gop-len", fmt.Sprintf("%d", int(gopLengthSecond*60)))
	} else {
		// インターレース映像として読み込む。
		options = append(options, "--interlace", "tff")
		if qualityInfo.Is60fps {
			// インターレース解除 (60i → 60p) 。NVEncC / VCEEncC は --vpp-yadif を使う。
			switch encoderType {
			case "QSVEncC":
				options = append(options, "--vpp-deinterlace", "bob")
			case "NVEncC", "VCEEncC":
				options = append(options, "--vpp-yadif", "mode=bob")
			case "rkmppenc":
				options = append(options, "--vpp-deinterlace", "bob_i5")
			}
			options = append(options, "--avsync", "vfr", "--gop-len", fmt.Sprintf("%d", int(gopLengthSecond*60)))
		} else {
			if encodingOptions.Is24fpsModeEnabled {
				// 24fps モードでは --vpp-afs で 24fps 区間を検出し、24/30p 混合 VFR で出力する。
				options = append(options, "--vpp-afs", "preset=default,drop=on,smooth=on")
			} else {
				switch encoderType {
				case "QSVEncC":
					options = append(options, "--vpp-deinterlace", "normal")
				case "NVEncC", "VCEEncC":
					options = append(options, "--vpp-afs", "preset=default")
				case "rkmppenc":
					options = append(options, "--vpp-deinterlace", "normal_i5")
				}
			}
			options = append(options, "--avsync", "vfr", "--gop-len", fmt.Sprintf("%d", int(gopLengthSecond*30)))
		}
	}

	// 映像: 解像度 (フル HD 放送のチャンネルかつ 1080p の場合のみ 1920×1080 にする) 。
	videoWidth := qualityInfo.Width
	videoHeight := qualityInfo.Height
	if videoWidth == 1440 && videoHeight == 1080 && isFullHDChannel {
		videoWidth = 1920
	}
	options = append(options, "--output-res", fmt.Sprintf("%dx%d", videoWidth, videoHeight))

	// 音声
	options = append(options, "--audio-codec", "aac:aac_coder=twoloop", "--audio-bitrate", qualityInfo.AudioBitrate)
	options = append(options, "--audio-samplerate", "48000", "--audio-filter", "volume=2.0", "--audio-ignore-decode-error", "30")

	// 出力
	options = append(options, "--output-format", "mpegts", "--output", "-")

	return options
}

// contextWithTimeout は指定された秒数のタイムアウトを持つ context を返す。
func contextWithTimeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}

// hwencHelpCache はエンコーダーの --help の出力のキャッシュ。
// プロセスが生きている間はエンコーダーの対応オプションが変わることはないため、一度だけ取得する。
var hwencHelpCache = struct {
	sync.Mutex
	helps map[string]string
}{helps: map[string]string{}}

// HWEncCOptionAvailable は指定された HWEncC がコマンドラインオプションに対応しているかどうかを返す。
// エンコーダーのバージョンによって利用できるオプションは異なり、未対応のオプションを指定すると
// エンコーダーが即座に終了してエンコードが失敗するため、--help の出力を調べる。
// 移植元: app/utils/HWEncC.py
func HWEncCOptionAvailable(encoderPath string, option string) bool {
	hwencHelpCache.Lock()
	defer hwencHelpCache.Unlock()
	if help, ok := hwencHelpCache.helps[encoderPath]; ok {
		return strings.Contains(help, option)
	}
	help := ""
	if _, err := exec.LookPath(encoderPath); err == nil {
		ctx, cancel := contextWithTimeout(15)
		defer cancel()
		// --help は標準エラー出力に出すエンコーダーもあるため、両方を取得する。
		if output, err := exec.CommandContext(ctx, encoderPath, "--help").CombinedOutput(); err == nil {
			help = string(output)
		}
	}
	hwencHelpCache.helps[encoderPath] = help
	return strings.Contains(help, option)
}
