package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// ProgramAnalyzer は録画ファイルから番組情報・チャンネル情報を解析する。
// 移植元: TSInfoAnalyzer
//
// Go 版では MPEG-TS の SDT/EIT 解析 (ARIB 仕様のデスクリプタ解析) は未移植のため、
// 既定では nil となり番組情報はファイル名から生成される (Python 版の
// 「SDT/EIT 解析に失敗した場合のフォールバック」と同じ経路) 。
type ProgramAnalyzer interface {
	// AnalyzeProgram は録画ファイルから番組情報を解析する。
	// 取得できなかった場合は (nil, false) を返す。
	AnalyzeProgram(video *RecordedVideo, endTSOffset *int64) (*RecordedProgram, bool)
}

// Analyzer は録画ファイルのメタデータを解析する。
// 移植元: MetadataAnalyzer
//
// 外部プロセスの起動は Runner 経由で行うため、テストではフェイクに差し替えられる。
type Analyzer struct {
	// FFprobePath は FFprobe の実行ファイルパス。
	FFprobePath string
	// Runner は外部プロセス実行の抽象化。
	Runner ProcessRunner
	// Logger はログ出力先。
	Logger *slog.Logger
	// ProgramAnalyzer は番組情報の解析器 (nil の場合はファイル名から生成) 。
	ProgramAnalyzer ProgramAnalyzer
}

// Analyze は録画ファイルのメタデータを解析する。
// KonomiTV で再生可能でないファイルの場合は (nil, nil) を返す。
// 移植元: MetadataAnalyzer.analyze()
func (a *Analyzer) Analyze(ctx context.Context, path string) (*RecordedProgram, error) {
	logger := a.logger()

	// FFprobe から録画ファイルのメディア情報を取得
	fullProbe, sampleProbe, endTSOffset, err := a.analyzeFFprobe(ctx, path)
	if err != nil {
		if errors.Is(err, errNotPlayable) {
			return nil, nil
		}
		return nil, err
	}

	// MPEG-TS の TS パケットサイズが 188 以外であれば弾く (BDAV 等は非対応)
	if fullProbe.Format.FormatName == "mpegts" {
		for _, stream := range fullProbe.Streams {
			packetSize := stream.tsPacketSize()
			if packetSize == nil {
				continue
			}
			if size, err := strconv.Atoi(strings.TrimSpace(*packetSize)); err == nil && size != 188 {
				logger.Warn("Unsupported TS packet size detected.", "path", path, "size", size)
				return nil, nil
			}
		}
	}

	// コンテナ形式
	var containerFormat string
	switch {
	case fullProbe.Format.FormatName == "mpegts":
		containerFormat = "MPEG-TS"
	case strings.Contains(fullProbe.Format.FormatName, "mp4"):
		containerFormat = "MPEG-4"
	default:
		return nil, nil
	}

	fullProbeVideoStreams := videoStreamsOf(fullProbe.Streams)
	fullProbeAudioStreams := audioStreamsOf(fullProbe.Streams)
	sampleProbeVideoStreams := videoStreamsOf(sampleProbe.Streams)
	sampleProbeAudioStreams := audioStreamsOf(sampleProbe.Streams)
	if len(fullProbeVideoStreams) == 0 || len(fullProbeAudioStreams) == 0 ||
		len(sampleProbeVideoStreams) == 0 || len(sampleProbeAudioStreams) == 0 {
		logger.Warn("No valid video or audio streams found.", "path", path)
		return nil, nil
	}
	hasVideoCodecType := false
	hasAudioCodecType := false
	for _, stream := range sampleProbe.Streams {
		if stream.codecTypeIs("video") {
			hasVideoCodecType = true
		}
		if stream.codecTypeIs("audio") {
			hasAudioCodecType = true
		}
	}
	if len(sampleProbeVideoStreams) == 0 && hasVideoCodecType {
		logger.Warn("Video stream details are missing. (Is the TS scrambled or unsupported?)", "path", path)
		return nil, nil
	}
	if len(sampleProbeAudioStreams) == 0 && hasAudioCodecType {
		logger.Warn("Audio stream details are missing. (Is the TS scrambled or unsupported?)", "path", path)
		return nil, nil
	}

	// 再生時間 (コンテナの値よりも映像ストリームの値が短い場合は映像側を優先する)
	var duration float64
	if fullProbe.Format.Duration != nil {
		formatDuration, err := strconv.ParseFloat(*fullProbe.Format.Duration, 64)
		if err != nil {
			logger.Warn("Duration is missing or invalid. ignored.", "path", path)
			return nil, nil
		}
		if fullProbeVideoStreams[0].Duration != nil && *fullProbeVideoStreams[0].Duration < formatDuration {
			duration = *fullProbeVideoStreams[0].Duration
		} else {
			duration = formatDuration
		}
	} else if fullProbeVideoStreams[0].Duration != nil {
		duration = *fullProbeVideoStreams[0].Duration
	} else {
		logger.Warn("Duration is missing or invalid. ignored.", "path", path)
		return nil, nil
	}

	// 映像情報 (部分解析の結果を優先)
	var (
		videoCodec        string
		videoCodecProfile string
		videoScanType     string
		videoFrameRate    float64
		videoWidth        int
		videoHeight       int
	)
	videoStream := sampleProbeVideoStreams[0]
	switch videoStream.CodecName {
	case "mpeg2video":
		videoCodec = "MPEG-2"
	case "h264":
		videoCodec = "H.264"
	case "hevc":
		videoCodec = "H.265"
	}
	profile := ""
	if videoStream.Profile != nil {
		profile = *videoStream.Profile
	}
	if profile == "" {
		videoCodecProfile = "Main"
	} else {
		videoCodecProfile = strings.Split(profile, "@")[0]
	}
	fieldOrder := "tt"
	if videoStream.FieldOrder != nil {
		fieldOrder = *videoStream.FieldOrder
	}
	if strings.EqualFold(fieldOrder, "progressive") {
		videoScanType = "Progressive"
	} else {
		videoScanType = "Interlaced"
	}
	// 全体解析側の field_order が progressive でない場合はインターレースとする
	if videoScanType == "Progressive" {
		fullFieldOrder := ""
		if fullProbeVideoStreams[0].FieldOrder != nil {
			fullFieldOrder = *fullProbeVideoStreams[0].FieldOrder
		}
		if !strings.EqualFold(fullFieldOrder, "progressive") {
			videoScanType = "Interlaced"
		}
	}
	if value := parseFPS(&videoStream.AvgFrameRate); value != nil {
		videoFrameRate = *value
	} else if value := parseFPS(&videoStream.RFrameRate); value != nil {
		videoFrameRate = *value
	}
	videoWidth = videoStream.Width
	videoHeight = videoStream.Height

	// 音声情報 (主音声・副音声)
	var (
		primaryAudioCodec          string
		primaryAudioChannel        string
		primaryAudioSamplingRate   int
		secondaryAudioCodec        *string
		secondaryAudioChannel      *string
		secondaryAudioSamplingRate *int
	)
	isPrimaryAnalyzed := false
	isSecondaryAnalyzed := false
	for _, audioStream := range sampleProbeAudioStreams {
		if !isPrimaryAnalyzed {
			if audioStream.CodecName == "aac" {
				if audioStream.Profile == nil || strings.Contains(*audioStream.Profile, "LC") {
					primaryAudioCodec = "AAC-LC"
				}
			}
			if primaryAudioCodec == "" {
				continue
			}
			channel := audioChannelName(audioStream.Channels)
			if channel == "" {
				continue
			}
			primaryAudioChannel = channel
			rate, err := strconv.Atoi(strings.TrimSpace(audioStream.SampleRate))
			if err != nil {
				continue
			}
			primaryAudioSamplingRate = rate
			isPrimaryAnalyzed = true
			continue
		}
		if !isSecondaryAnalyzed {
			codec := ""
			if audioStream.CodecName == "aac" {
				if audioStream.Profile == nil || strings.Contains(*audioStream.Profile, "LC") {
					codec = "AAC-LC"
				}
			}
			if codec == "" {
				continue
			}
			channel := audioChannelName(audioStream.Channels)
			if channel == "" {
				continue
			}
			rate, err := strconv.Atoi(strings.TrimSpace(audioStream.SampleRate))
			if err != nil {
				continue
			}
			secondaryAudioCodec = &codec
			secondaryAudioChannel = &channel
			secondaryAudioSamplingRate = &rate
			isSecondaryAnalyzed = true
		}
	}

	if videoCodec == "" || primaryAudioCodec == "" {
		logger.Warn("Video or primary audio track is missing or invalid. ignored.", "path", path)
		return nil, nil
	}

	// MPEG-TS は先頭バイトが sync_byte であることと、映像ストリームの変化を確認する
	hasVideoStreamChanges := false
	if containerFormat == "MPEG-TS" {
		valid, err := readFirstSyncByteIsValid(path)
		if err != nil {
			// 空ファイルなど。Python 版では例外 → 解析失敗扱いになる
			logger.Warn("sync_byte is missing. ignored.", "path", path)
			return nil, nil
		}
		if !valid {
			logger.Warn("sync_byte is missing. ignored.", "path", path)
			return nil, nil
		}
		hasVideoStreamChanges = detectTSVideoStreamChanges(path, endTSOffset)
	}

	// ファイルハッシュを計算
	fileHash, err := calculateFileHash(path, endTSOffset)
	if err != nil {
		logger.Warn("File size is too small. ignored.", "path", path, "error", err)
		return nil, nil
	}

	// 録画ファイル情報を表すモデルを作成
	now := time.Now().In(constants.JST)
	fileInfo, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat the recording file: %w", err)
	}
	recordedVideo := RecordedVideo{
		Status:                     "Recorded",
		FilePath:                   path,
		FileHash:                   fileHash,
		FileSize:                   fileInfo.Size(),
		FileCreatedAt:              fileCreationTime(fileInfo),
		FileModifiedAt:             fileInfo.ModTime().In(constants.JST),
		Duration:                   duration,
		ContainerFormat:            containerFormat,
		VideoCodec:                 videoCodec,
		VideoCodecProfile:          videoCodecProfile,
		VideoScanType:              videoScanType,
		VideoFrameRate:             videoFrameRate,
		VideoResolutionWidth:       videoWidth,
		VideoResolutionHeight:      videoHeight,
		HasVideoStreamChanges:      hasVideoStreamChanges,
		PrimaryAudioCodec:          primaryAudioCodec,
		PrimaryAudioChannel:        primaryAudioChannel,
		PrimaryAudioSamplingRate:   primaryAudioSamplingRate,
		SecondaryAudioCodec:        secondaryAudioCodec,
		SecondaryAudioChannel:      secondaryAudioChannel,
		SecondaryAudioSamplingRate: secondaryAudioSamplingRate,
	}

	// FFprobe の programs 配列から実際にストリームが存在する service_id を特定する
	var preferredServiceID *int
	for _, program := range fullProbe.Programs {
		if program.NbStreams != nil && *program.NbStreams > 0 &&
			program.PCRPID != nil && *program.PCRPID > 0 && program.ProgramNum != nil {
			value := *program.ProgramNum
			preferredServiceID = &value
			break
		}
	}

	// 番組情報・チャンネル情報を解析する
	var recordedProgram *RecordedProgram
	analyzed := false
	if a.ProgramAnalyzer != nil {
		program, ok := a.ProgramAnalyzer.AnalyzeProgram(&recordedVideo, endTSOffset)
		if ok {
			recordedProgram = program
			analyzed = true
			// 取得成功時は録画開始時刻と録画終了時刻も解析する
			if recorder, ok := a.ProgramAnalyzer.(RecordingTimeAnalyzer); ok {
				start, end, ok := recorder.AnalyzeRecordingTime()
				if ok {
					recordedVideo.RecordingStartTime = &start
					recordedVideo.RecordingEndTime = &end
				}
			}
		}
	}
	_ = preferredServiceID

	// 番組情報を取得できなかった場合、末尾の更新日時が近ければまだ録画中の可能性が高いため None を返す
	if !analyzed {
		if now.Sub(recordedVideo.FileModifiedAt).Seconds() < 30 {
			logger.Warn("MPEG-TS SDT/EIT analysis failed. (still recording?)", "path", path)
			return nil, nil
		}
		// ファイル名などから最低限の情報を設定する
		var recordingStartTime time.Time
		if recordedVideo.RecordingStartTime != nil {
			recordingStartTime = *recordedVideo.RecordingStartTime
		} else {
			recordingStartTime = recordedVideo.FileModifiedAt.Add(-time.Duration(recordedVideo.Duration * float64(time.Second)))
		}
		title := formatString(fileNameWithoutExtension(path))
		recordedProgram = &RecordedProgram{
			Video:                recordedVideo,
			Title:                title,
			Description:          DefaultDescription,
			Detail:               []DetailEntry{},
			StartTime:            recordingStartTime,
			EndTime:              recordingStartTime.Add(time.Duration(recordedVideo.Duration * float64(time.Second))),
			Duration:             recordedVideo.Duration,
			IsFree:               true,
			Genres:               []Genre{},
			PrimaryAudioType:     DefaultPrimaryAudioType,
			PrimaryAudioLanguage: DefaultPrimaryAudioLanguage,
		}
	} else {
		recordedProgram.Video = recordedVideo
		// 録画マージンを算出する
		recordedProgram.RecordingStartMargin = math.Max(recordedProgram.StartTime.Sub(*recordedVideo.RecordingStartTime).Seconds(), 0.0)
		recordedProgram.RecordingEndMargin = math.Max(recordedVideo.RecordingEndTime.Sub(recordedProgram.EndTime).Seconds(), 0.0)
		// 部分的に録画されているかを判定する
		recordedProgram.IsPartiallyRecorded = recordedProgram.StartTime.Before(*recordedVideo.RecordingStartTime) ||
			recordedVideo.RecordingEndTime.Before(recordedProgram.EndTime)
	}
	return recordedProgram, nil
}

// RecordingTimeAnalyzer は録画開始時刻・録画終了時刻を解析できる ProgramAnalyzer の追加インターフェース。
// 移植元: TSInfoAnalyzer.analyzeRecordingTime()
type RecordingTimeAnalyzer interface {
	AnalyzeRecordingTime() (time.Time, time.Time, bool)
}

// errNotPlayable は KonomiTV で再生可能でないファイルを表す内部エラー。
var errNotPlayable = errors.New("not a KonomiTV playable file")

// analyzeFFprobe は FFprobe の全体解析と部分解析を実行する。
// 移植元: MetadataAnalyzer.__analyzeFFprobe()
func (a *Analyzer) analyzeFFprobe(ctx context.Context, path string) (*ProbeResult, *ProbeSampleResult, *int64, error) {
	logger := a.logger()

	fullJSON, err := a.runFFprobe(ctx, FullProbeArgs(path), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if fullJSON == nil {
		return nil, nil, nil, nil
	}
	fullProbe, err := ParseProbeResult(fullJSON)
	if err != nil {
		logger.Warn("Failed to parse full ffprobe result.", "path", path, "error", err)
		return nil, nil, nil, nil
	}

	// FFprobe から再生時間を取得できない場合のフォールバック処理
	var endTSOffset *int64
	if fullProbe.Format.Duration == nil && fullProbe.Format.FormatName == "mpegts" {
		duration, validEnd, ok := calculateTSFileDuration(path, 1024*1024)
		if !ok {
			logger.Warn("Duration is missing and fallback failed.", "path", path)
			return nil, nil, nil, nil
		}
		formatted := strconv.FormatFloat(duration, 'g', -1, 64)
		fullProbe.Format.Duration = &formatted
		endTSOffset = &validEnd
	}

	// 部分解析: 録画ファイルの 25% 位置から 30 秒程度のデータを解析する
	var sampleJSON []byte
	if strings.Contains(strings.ToLower(fullProbe.Format.FormatName), "mpegts") {
		sampleData, sampleEndOffset, err := extractSampleData(path)
		if err != nil {
			logger.Warn("Failed to analyze sample via ffprobe.", "path", path, "error", err)
			return nil, nil, nil, nil
		}
		if sampleEndOffset != nil {
			endTSOffset = sampleEndOffset
		}
		sampleJSON, err = a.runFFprobe(ctx, SampleProbeArgs(), sampleData)
		if err != nil {
			return nil, nil, nil, err
		}
		if sampleJSON == nil {
			return nil, nil, nil, nil
		}
	} else {
		sampleJSON = fullJSON
	}

	sampleProbe, err := ParseProbeSampleResult(sampleJSON)
	if err != nil {
		logger.Warn("Failed to parse sample ffprobe result.", "path", path, "error", err)
		return nil, nil, nil, nil
	}
	return fullProbe, sampleProbe, endTSOffset, nil
}

// runFFprobe は FFprobe を実行して JSON を返す (失敗時は nil) 。
// 移植元: MetadataAnalyzer.__runFFprobe()
func (a *Analyzer) runFFprobe(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	result, err := a.runner().Run(ctx, a.FFprobePath, args, stdin)
	if err != nil {
		a.logger().Warn("Failed to run ffprobe.", "error", err)
		return nil, nil
	}
	if result.ExitCode != 0 {
		a.logger().Warn("ffprobe failed.", "exit_code", result.ExitCode, "stderr", strings.TrimSpace(string(result.Stderr)))
		return nil, nil
	}
	return []byte(strings.TrimSpace(string(result.Stdout))), nil
}

// runner は ProcessRunner を返す (未指定の場合は os/exec 実装) 。
func (a *Analyzer) runner() ProcessRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return ExecRunner{}
}

// logger は slog.Logger を返す (未指定の場合は標準ロガー) 。
func (a *Analyzer) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// ParseFPS は "30000/1001" などのフレームレート文字列を float に変換する。
// 移植元: MetadataAnalyzer.analyze() の ParseFPS()
func parseFPS(value *string) *float64 {
	if value == nil {
		return nil
	}
	if strings.Contains(*value, "/") {
		parts := strings.SplitN(*value, "/", 2)
		numerator, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		denominator, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil || denominator == 0.0 {
			return nil
		}
		result := roundToDigits(numerator/denominator, 2)
		return &result
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(*value), 64)
	if err != nil {
		return nil
	}
	result := roundToDigits(parsed, 2)
	return &result
}

// audioChannelName は音声チャンネル数を KonomiTV の表記に変換する (未対応の場合は空文字) 。
func audioChannelName(channels int) string {
	switch channels {
	case 1:
		return "Monaural"
	case 2:
		return "Stereo"
	case 6:
		return "5.1ch"
	}
	return ""
}

// fileNameWithoutExtension は拡張子を除いたファイル名を返す。
func fileNameWithoutExtension(path string) string {
	name := path
	if index := strings.LastIndexAny(path, `/\`); index >= 0 {
		name = path[index+1:]
	}
	if index := strings.LastIndex(name, "."); index > 0 {
		name = name[:index]
	}
	return name
}
