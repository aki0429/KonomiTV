package videostream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// 録画ストリーミングのエンコードタスク (移植元: server/app/streams/VideoEncodingTask.py の VideoEncodingTask.run()) 。
//
// 純ロジック (キーフレーム収集・引数組み立て・セグメント切り出し・リトライ・キャンセル) を移植したもので、
// 外部プロセスの起動は EncodingTaskOptions.Runner (既定は ExecProcessRunner) 経由にし、テストではフェイクに差し替える。
//
// 担当A の Session (internal/videostream/session.go) へは EncodingSession / EncodingSegment interface 経由で接続する。
// Session は sessionEncodingAdapter (encoding_task_session.go) が包む。

const (
	// encoderReadTimeout はエンコーダー出力の読み取りタイムアウト (VideoEncodingTask.run() の read_timeout) 。
	encoderReadTimeout = 10 * time.Second
	// encoderStderrLineLimit は試行ごとに保持するエンコーダー stderr の行数 (deque(maxlen=1000)) 。
	encoderStderrLineLimit = 1000
	// tsReadExChunkPackets はフィード時にまとめて読む TS パケット数 (パケットサイズ * 10000) 。
	tsReadExChunkPackets = 10000
	// tsReadExPATPMTLookbackPackets は PAT/PMT 探索で遡る最大パケット数 (188 * 5000) 。
	tsReadExPATPMTLookbackPackets = 5000
	// processTerminateTimeout は終了待機のタイムアウト (asyncio.wait_for(..., timeout=5.0)) 。
	processTerminateTimeout = 5 * time.Second
	// feedTaskWaitTimeout はフィードタスクの完了待機のタイムアウト。
	feedTaskWaitTimeout = 3 * time.Second
)

// KeyFrameQueue はワーカースレッド (フィード) からイベントループ側へキーフレームを渡すキュー。
// 移植元: queue.SimpleQueue[KeyFrame]
type keyFrameQueue struct {
	mu    sync.Mutex
	items []KeyFrame
}

func (q *keyFrameQueue) push(keyFrames ...KeyFrame) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, keyFrames...)
}

func (q *keyFrameQueue) drain() []KeyFrame {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// EncodingSegment はエンコード対象の HLS セグメント (担当A の Segment を包む) 。
type EncodingSegment interface {
	// Index は HLS シーケンス番号。
	Index() int
	// PlaylistStartSeconds / DurationSeconds はプレイリスト上の開始時刻と長さ (秒) 。
	PlaylistStartSeconds() float64
	DurationSeconds() float64
	// SourceStartDTS は入力ソース側の開始 DTS (90kHz) 。
	SourceStartDTS() (int64, bool)
	// SourceFilePosition は入力ファイルのバイト位置。
	SourceFilePosition() (int64, bool)
	// MarkEncoding は状態を Encoding にする。
	MarkEncoding()
	// SetEncoded はエンコード済みデータを確定する。
	SetEncoded(data []byte)
}

// EncodingSession はこのエンコードタスクが依存する Session の最小 interface 。
// 担当A の Session がこの形を満たせば sessionEncodingAdapter 経由で接続できる。
type EncodingSession interface {
	// LogPrefix はログの接頭辞。
	LogPrefix() string
	// Quality はベース画質 (例: 1080p-hevc) 。
	Quality() string
	// EncodingOptions は追加エンコードオプション。
	EncodingOptions() stream.StreamEncodingOptions
	// Encoder は config.general.encoder の値。
	Encoder() string
	// Video は録画ファイルの映像情報。
	Video() EncodingVideoInfo
	// Channel は録画に紐づくチャンネル (無ければ nil) 。
	Channel() *EncodingChannel
	// TSStreamInfo は MPEG-TS の入力キーフレーム収集に使うストリーム情報 (未解決なら nil) 。
	TSStreamInfo() *StreamInfo
	// EnsureTSKeyFrameContext は入力キーフレーム収集の前提 (ストリーム情報と先頭 DTS) を用意する。
	EnsureTSKeyFrameContext() error
	// Segments は HLS セグメントの一覧。
	Segments() []EncodingSegment
	// AddKeyFrames は入力 TS から収集したキーフレームを追加する。
	AddKeyFrames(ctx context.Context, keyFrames []KeyFrame)
	// FlushKeyFrames は閾値を無視して収集済みキーフレームを保存する。
	FlushKeyFrames(ctx context.Context)
}

// encodingSessionAdvancer は完了と次セグメントの Encoding 化を原子的に行える Session が実装する。
// 担当A の Session.CompleteAndAdvance() がこれに相当する。
type encodingSessionAdvancer interface {
	CompleteAndAdvance(sequence int, data []byte)
}

// EncodingTaskOptions はエンコードタスクの依存関係とテスト差し替え点。
type EncodingTaskOptions struct {
	// Runner は外部プロセスの起動 (nil なら ExecProcessRunner) 。テストではフェイクを渡す。
	Runner ProcessRunner
	// LibraryPath は外部ツール名を実行ファイルのパスへ変換する (nil なら名前をそのまま使う) 。
	LibraryPath func(name string) string
	// IsHWEncCOptionAvailable は HWEncC が指定オプションに対応しているかを返す (nil なら常に false) 。
	IsHWEncCOptionAvailable func(encoderType string, option string) bool
	// PinFFmpegOnVideoStreamChanges は映像ストリーム構成変化時に FFmpeg へ固定するかを決める。
	// 現行 Python 版には存在しないため既定 false (ResolveEncoderType を参照) 。
	PinFFmpegOnVideoStreamChanges bool
	// Logger はログ出力先 (nil なら slog.Default() ) 。
	Logger *slog.Logger
	// ReadTimeout はエンコーダー出力の読み取りタイムアウト (0 なら encoderReadTimeout) 。
	ReadTimeout time.Duration
}

// VideoEncodingTask は 1 セッション分の録画エンコードタスク (SegmentEncoder 実装) 。
// 移植元: VideoEncodingTask (使い捨て。Run は 1 回だけ呼ぶ) 。
type VideoEncodingTask struct {
	session EncodingSession
	options EncodingTaskOptions

	mu         sync.Mutex
	cancelled  bool
	finished   bool
	retryCount int
	cancelCh   chan struct{}

	processMu       sync.Mutex
	encoderProcess  Process
	tsreadexProcess Process
	psisimuxProcess Process
}

// NewVideoEncodingTask はエンコードタスクを生成する。
func NewVideoEncodingTask(session EncodingSession, options EncodingTaskOptions) *VideoEncodingTask {
	if options.Runner == nil {
		options.Runner = ExecProcessRunner
	}
	if options.LibraryPath == nil {
		options.LibraryPath = func(name string) string { return name }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = encoderReadTimeout
	}
	return &VideoEncodingTask{session: session, options: options, cancelCh: make(chan struct{})}
}

func (t *VideoEncodingTask) logger() *slog.Logger { return t.options.Logger }

func (t *VideoEncodingTask) libraryPath(name string) string { return t.options.LibraryPath(name) }

// RetryCount はこれまでに失敗した試行回数を返す (テスト用) 。
func (t *VideoEncodingTask) RetryCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retryCount
}

// IsFinished は最後まで処理を完了したかどうかを返す。
func (t *VideoEncodingTask) IsFinished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// IsCancelled はキャンセルされたかどうかを返す。
func (t *VideoEncodingTask) IsCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

func (t *VideoEncodingTask) isCancelled() bool { return t.IsCancelled() }

// Cancel はエンコードタスクをキャンセルし、起動中の外部プロセスを下流側から終了する。
// 移植元: VideoEncodingTask.cancel() 。複数回呼ばれてよい。
func (t *VideoEncodingTask) Cancel() {
	t.mu.Lock()
	if t.finished || t.cancelled {
		t.mu.Unlock()
		return
	}
	t.cancelled = true
	close(t.cancelCh)
	t.mu.Unlock()

	t.logger().Info(t.session.LogPrefix() + " Encoding task cancellation requested.")
	t.killProcesses()
}

// killProcesses はエンコーダー → tsreadex → psisimux の順に強制終了する (下流側から止めるのが重要) 。
func (t *VideoEncodingTask) killProcesses() {
	t.processMu.Lock()
	processes := []Process{t.encoderProcess, t.tsreadexProcess, t.psisimuxProcess}
	t.processMu.Unlock()
	for _, process := range processes {
		if process == nil {
			continue
		}
		if err := process.Kill(); err != nil {
			t.logger().Error(t.session.LogPrefix()+" Failed to terminate process: "+err.Error(), "error", err)
		}
	}
}

func (t *VideoEncodingTask) setProcess(kind ProcessKind, process Process) {
	t.processMu.Lock()
	defer t.processMu.Unlock()
	switch kind {
	case ProcessEncoder:
		t.encoderProcess = process
	case ProcessTSReadEx:
		t.tsreadexProcess = process
	case ProcessPsisimux:
		t.psisimuxProcess = process
	}
}

func (t *VideoEncodingTask) clearProcesses() {
	t.processMu.Lock()
	defer t.processMu.Unlock()
	t.encoderProcess = nil
	t.tsreadexProcess = nil
	t.psisimuxProcess = nil
}

// Run はエンコードタスクを実行する。移植元: VideoEncodingTask.run()
func (t *VideoEncodingTask) Run(ctx context.Context, startSequence int) error {
	session := t.session
	segments := session.Segments()
	if startSequence < 0 || startSequence >= len(segments) {
		return fmt.Errorf("invalid start sequence: %d (segments: %d)", startSequence, len(segments))
	}

	video := session.Video()
	encoderType := ResolveEncoderType(session.Encoder(), video.ContainerFormat, video.HasVideoStreamChanges, t.options.PinFFmpegOnVideoStreamChanges)

	startSegment := segments[startSequence]
	startDTS, ok := startSegment.SourceStartDTS()
	if !ok {
		// VideoStream 側で解決済みのソース DTS が無い場合は呼び出し順序のバグなので即座に止める
		return fmt.Errorf("Source start DTS is not resolved. [sequence: %d]", startSequence)
	}
	outputTSOffset := float64(startDTS) / float64(HZ)

	// ctx のキャンセルを Cancel() に伝える (Run の終了時は watchStop で確実に抜ける)
	watchStop := make(chan struct{})
	defer close(watchStop)
	go func() {
		select {
		case <-ctx.Done():
			t.Cancel()
		case <-watchStop:
		}
	}()

	// MPEG-TS 形式の場合のみ録画ファイルを開く
	var file *os.File
	if video.ContainerFormat == "MPEG-TS" {
		// 再生しながらのキーフレーム収集は補助的な高速化なので、初期化に失敗しても再生本体は続ける
		if err := session.EnsureTSKeyFrameContext(); err != nil {
			t.logger().Warn(session.LogPrefix()+" Failed to initialize input keyframe collector context.", "error", err)
		}
		opened, err := os.Open(video.FilePath)
		if err != nil {
			return fmt.Errorf("failed to open the recorded file: %w", err)
		}
		defer opened.Close()
		file = opened
	}

	queue := &keyFrameQueue{}
	currentIndex := startSequence
	startSegment.MarkEncoding()
	t.logger().Info(fmt.Sprintf("%s[Segment %d] Starting the Encoder...", session.LogPrefix(), currentIndex))

	var encoded []byte
	reachedFinal := false
	for t.RetryCount() < MaxEncodingRetryCount {
		if t.isCancelled() {
			break
		}
		state, err := t.runAttempt(ctx, file, video, encoderType, outputTSOffset, segments, currentIndex, queue)
		if err != nil {
			t.cleanup(state)
			t.flushKeyFrames(ctx, queue)
			t.session.FlushKeyFrames(context.Background())
			return err
		}
		t.cleanup(state)
		currentIndex = state.currentIndex
		encoded = state.encoded
		reachedFinal = state.reachedFinal

		if t.isCancelled() {
			break
		}
		// video_pid / audio_pid が取得できていない場合は正常な TS が出力されていないためリトライする
		if state.videoPID < 0 || state.audioPID < 0 {
			t.mu.Lock()
			t.retryCount++
			retry := t.retryCount
			t.mu.Unlock()
			if retry < MaxEncodingRetryCount {
				t.logger().Warn(fmt.Sprintf("%s Failed to get video/audio PID. Retrying... (%d/%d)", session.LogPrefix(), retry, MaxEncodingRetryCount))
				state.dumpEncoderStderr(t.logger(), true)
				t.clearProcesses()
				continue
			}
			t.logger().Error(fmt.Sprintf("%s Failed to get video/audio PID after %d retries.", session.LogPrefix(), MaxEncodingRetryCount))
			state.dumpEncoderStderr(t.logger(), true)
			break
		}
		break
	}

	// キャンセルで終わった旧タスクでも、読み終えた入力 TS 範囲のキーフレームは次回シークに使える
	t.flushKeyFrames(ctx, queue)
	t.session.FlushKeyFrames(context.Background())

	if t.isCancelled() {
		return nil
	}
	// 最後のセグメントが完了していない場合は、現在のバッファで確定する
	if !reachedFinal && currentIndex >= 0 && currentIndex < len(segments) {
		segments[currentIndex].SetEncoded(encoded)
		t.logger().Info(fmt.Sprintf("%s[Segment %d] Successfully Encoded Final HLS Segment.", session.LogPrefix(), currentIndex))
	}

	t.mu.Lock()
	t.finished = true
	t.mu.Unlock()
	t.logger().Info(session.LogPrefix() + " Finished the Encoding Task.")
	return nil
}

// flushKeyFrames は収集済みキーフレームを Session へ渡す (VideoEncodingTask.FlushCollectedSegmentMap 相当) 。
func (t *VideoEncodingTask) flushKeyFrames(ctx context.Context, queue *keyFrameQueue) {
	keyFrames := queue.drain()
	if len(keyFrames) == 0 {
		return
	}
	t.session.AddKeyFrames(ctx, keyFrames)
}

// completeSegment はセグメントを確定し、次セグメントを Encoding にする。
func (t *VideoEncodingTask) completeSegment(segments []EncodingSegment, sequence int, data []byte, isFinal bool) {
	if isFinal || sequence+1 >= len(segments) {
		segments[sequence].SetEncoded(data)
		return
	}
	if advancer, ok := t.session.(encodingSessionAdvancer); ok {
		advancer.CompleteAndAdvance(sequence, data)
		return
	}
	segments[sequence].SetEncoded(data)
	segments[sequence+1].MarkEncoding()
}

// cleanup は試行の後始末 (プロセス終了 → フィードタスク終了 → パイプ close) を行う。
func (t *VideoEncodingTask) cleanup(state *encodingAttempt) {
	// 下流側のプロセスから順に止める (エンコーダー → tsreadex → psisimux)
	if state.encoder != nil {
		t.stopProcess(state.encoder, "Encoder")
	}
	if state.tsreadex != nil {
		t.stopProcess(state.tsreadex, "tsreadex")
	}
	if state.feedWriter != nil {
		// tsreadex を止めた時点でフィード側の書き込みが解除されるので、完了を待ってから閉じる
		state.waitFeeder(feedTaskWaitTimeout)
		state.closeFeedWriter()
	}
	if state.psisimux != nil {
		t.stopProcess(state.psisimux, "psisimux")
	}
}

func (t *VideoEncodingTask) stopProcess(process Process, name string) {
	if err := process.Kill(); err != nil {
		t.logger().Debug(t.session.LogPrefix() + " " + name + " process was already terminated.")
	}
	done := make(chan struct{})
	go func() {
		_ = process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(processTerminateTimeout):
		t.logger().Warn(t.session.LogPrefix() + " " + name + " process termination wait timed out.")
	}
}

// runAttempt は 1 回のエンコード試行 (パイプライン起動 → 出力読み取り → セグメント切り出し) を実行する。
func (t *VideoEncodingTask) runAttempt(
	ctx context.Context,
	file *os.File,
	video EncodingVideoInfo,
	encoderType string,
	outputTSOffset float64,
	segments []EncodingSegment,
	currentIndex int,
	queue *keyFrameQueue,
) (*encodingAttempt, error) {
	state := &encodingAttempt{
		task:                       t,
		ctx:                        ctx,
		video:                      video,
		segments:                   segments,
		currentIndex:               currentIndex,
		queue:                      queue,
		pmtPID:                     -1,
		videoPID:                   -1,
		audioPID:                   -1,
		patParser:                  newSectionParser(),
		pmtParser:                  newSectionParser(),
		videoParser:                newPESParser(),
		audioParser:                newPESParser(),
		firstSegmentSourceStartDTS: 0,
		firstSegmentPlaylistStart:  0,
		encoderStderrLines:         []string{fmt.Sprintf("Retry %d/%d started.", t.RetryCount()+1, MaxEncodingRetryCount)},
	}
	if dts, ok := segments[currentIndex].SourceStartDTS(); ok {
		state.firstSegmentSourceStartDTS = dts
	}
	state.firstSegmentPlaylistStart = segments[currentIndex].PlaylistStartSeconds()

	// ---- tsreadex の入力 (PAT/PMT 付きパイプ or ファイル / psisimux) と tsreadex の起動 ----
	tsreadexOptions := []string{}
	var tsreadexSpec ProcessSpec
	var patPMTData []byte
	if video.ContainerFormat == "MPEG-4" {
		// MP4 は psisimux で MPEG-TS に合成してから tsreadex へ流す
		broadcastID, serviceID := ResolveMP4BroadcastIDs(t.session.Channel())
		psisimuxOptions := BuildPsisimuxOptions(outputTSOffset, broadcastID, video.FilePath)
		psisimux, err := t.options.Runner(ProcessSpec{
			Kind: ProcessPsisimux, Name: "psisimux", Path: t.libraryPath("psisimux"), Args: psisimuxOptions, CaptureStdout: true,
		})
		if err != nil {
			return state, fmt.Errorf("failed to start psisimux: %w", err)
		}
		state.psisimux = psisimux
		t.setProcess(ProcessPsisimux, psisimux)
		tsreadexOptions = BuildTSReadExOptions(serviceID)
		tsreadexSpec = ProcessSpec{
			Kind: ProcessTSReadEx, Name: "tsreadex", Path: t.libraryPath("tsreadex"), Args: tsreadexOptions,
			Stdin: psisimux.Stdout(), CloseStdinAfterStart: true, CaptureStdout: true,
		}
	} else {
		// MPEG-TS: セグメント開始位置に最も近い PAT/PMT を抽出し、あれば先頭に付けて tsreadex へ流す
		serviceID := "-1"
		if channel := t.session.Channel(); channel != nil {
			serviceID = fmt.Sprintf("%d", channel.ServiceID)
		}
		tsreadexOptions = BuildTSReadExOptions(serviceID)
		if file == nil {
			return state, errors.New("the recorded file is not opened")
		}
		sourcePosition, ok := segments[currentIndex].SourceFilePosition()
		if !ok {
			return state, fmt.Errorf("Source file position is not resolved. [sequence: %d]", currentIndex)
		}
		patPMT := extractClosestPATPMT(file, sourcePosition)
		if patPMT.Data() != nil {
			patPMTData = patPMT.Data()
			t.logger().Info(fmt.Sprintf("%s[Segment %d] Extracted PAT/PMT (PAT at -%d bytes, PMT at -%d bytes)",
				t.session.LogPrefix(), currentIndex, patPMT.PATDistance, patPMT.PMTDistance))
		} else {
			t.logger().Warn(fmt.Sprintf("%s[Segment %d] Failed to extract complete PAT/PMT", t.session.LogPrefix(), currentIndex))
		}
		if _, err := file.Seek(sourcePosition, io.SeekStart); err != nil {
			return state, fmt.Errorf("failed to seek the recorded file: %w", err)
		}
		if patPMTData != nil {
			tsreadexSpec = ProcessSpec{
				Kind: ProcessTSReadEx, Name: "tsreadex", Path: t.libraryPath("tsreadex"), Args: tsreadexOptions,
				StdinPipe: true, CaptureStdout: true,
			}
		} else {
			tsreadexSpec = ProcessSpec{
				Kind: ProcessTSReadEx, Name: "tsreadex", Path: t.libraryPath("tsreadex"), Args: tsreadexOptions,
				Stdin: file, CaptureStdout: true,
			}
		}
	}

	tsreadex, err := t.options.Runner(tsreadexSpec)
	if err != nil {
		if state.psisimux != nil {
			_ = state.psisimux.Kill()
		}
		return state, fmt.Errorf("failed to start tsreadex: %w", err)
	}
	state.tsreadex = tsreadex
	t.setProcess(ProcessTSReadEx, tsreadex)

	// tsreadex へ流し込むデータ (PAT/PMT + ファイル) を別 goroutine で書きつつ、入力キーフレームを収集する
	if patPMTData != nil {
		state.feedWriter = tsreadex.Stdin()
		if state.feedWriter != nil {
			state.feedDone = make(chan struct{})
			t.startFeeder(state, file, tsreadex, patPMTData, segments[currentIndex], queue)
		}
	}

	// ---- エンコーダーの起動 ----
	encodingOptions := t.session.EncodingOptions()
	var encoderArgs []string
	if encoderType == "FFmpeg" {
		encoderArgs, err = BuildVideoFFmpegOptions(t.session.Quality(), video, encodingOptions, t.RetryCount(), outputTSOffset)
	} else {
		encoderArgs, err = BuildVideoHWEncCOptions(t.session.Quality(), encoderType, video, encodingOptions, t.RetryCount(), outputTSOffset,
			func(option string) bool {
				if t.options.IsHWEncCOptionAvailable == nil {
					return false
				}
				return t.options.IsHWEncCOptionAvailable(encoderType, option)
			})
	}
	if err != nil {
		return state, err
	}
	encoder, err := t.options.Runner(ProcessSpec{
		Kind: ProcessEncoder, Name: encoderType, Path: t.libraryPath(encoderType), Args: encoderArgs,
		Stdin: tsreadex.Stdout(), CloseStdinAfterStart: true, CaptureStdout: true, CaptureStderr: true,
	})
	if err != nil {
		return state, fmt.Errorf("failed to start %s: %w", encoderType, err)
	}
	state.encoder = encoder
	t.setProcess(ProcessEncoder, encoder)

	// ---- エンコーダー出力の読み取り ----
	state.readEncoderOutput()

	return state, nil
}

// extractClosestPATPMT は sourcePosition に最も近い PAT/PMT を録画ファイルから抽出する。
// 移植元: VideoEncodingTask.run() の「PAT/PMT を抽出（セグメント開始位置に最も近いものを保持）」ブロック
func extractClosestPATPMT(file *os.File, sourcePosition int64) initialPATPMT {
	maxLookback := int64(PacketSize * tsReadExPATPMTLookbackPackets)
	searchStart := max(int64(0), sourcePosition-maxLookback)
	searchData := readFileAt(file, searchStart, sourcePosition-searchStart)
	return findClosestPATPMT(searchData, searchStart, sourcePosition)
}

// readFileAt は file の offset から length バイトを読む (EOF で切れても読めた分を返す) 。
func readFileAt(file *os.File, offset int64, length int64) []byte {
	if length <= 0 {
		return nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	buffer := make([]byte, length)
	read, _ := io.ReadFull(file, buffer)
	return buffer[:read]
}

// encodingAttempt は 1 回のエンコード試行の状態。
type encodingAttempt struct {
	task     *VideoEncodingTask
	ctx      context.Context
	video    EncodingVideoInfo
	segments []EncodingSegment

	psisimux Process
	tsreadex Process
	encoder  Process

	feedWriter io.WriteCloser
	feedDone   chan struct{}
	feedOnce   sync.Once

	encoderStderrLines []string
	stderrMu           sync.Mutex

	patParser   *sectionParser
	pmtParser   *sectionParser
	videoParser *pesParser
	audioParser *pesParser

	queue *keyFrameQueue

	pmtPID   int
	videoPID int
	audioPID int

	videoCodec string
	patCC      int
	pmtCC      int
	videoCC    int
	audioCC    int

	encoded   []byte
	latestPAT []byte
	latestPMT []byte

	currentIndex               int
	reachedFinal               bool
	firstSegmentSourceStartDTS int64
	firstSegmentPlaylistStart  float64

	firstVideoTS33  int64
	lastVideoTS33   int64
	hasFirstVideoTS bool
	wrapOffset      int64
	isSplitPending  bool
}

func (t *VideoEncodingTask) sessionLogPrefix() string { return t.session.LogPrefix() }

// startFeeder は PAT/PMT + 録画ファイルの続きを tsreadex へ書き込み、入力キーフレームを収集する。
// 移植元: VideoEncodingTask.run() の FeedTSStream()
func (t *VideoEncodingTask) startFeeder(state *encodingAttempt, file *os.File, tsreadex Process, patPMTData []byte, segment EncodingSegment, queue *keyFrameQueue) {
	streamInfo := t.session.TSStreamInfo()
	var collector *TSKeyFrameCollector
	startDTS, _ := segment.SourceStartDTS()
	if streamInfo != nil {
		collector = NewTSKeyFrameCollector(streamInfo, startDTS)
	}
	packetSize := PacketSize
	if streamInfo != nil && streamInfo.PacketSize > 0 {
		packetSize = streamInfo.PacketSize
	}

	go func() {
		defer close(state.feedDone)
		defer state.closeFeedWriter()
		writer := state.feedWriter
		if !writeAll(writer, patPMTData) {
			return
		}
		chunkSize := packetSize * tsReadExChunkPackets
		for {
			if t.isCancelled() {
				return
			}
			chunkOffset, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				return
			}
			chunk := readFileChunk(file, chunkSize)
			if len(chunk) == 0 {
				return
			}
			if collector != nil {
				found := collector.Push(chunk, chunkOffset)
				keyFrames := make([]KeyFrame, 0, len(found))
				for _, keyFrame := range found {
					keyFrames = append(keyFrames, KeyFrame{Offset: keyFrame.SourceFilePosition, DTS: keyFrame.SourceStartDTS})
				}
				queue.push(keyFrames...)
			}
			if !writeAll(writer, chunk) {
				return
			}
		}
	}()
}

// readFileChunk は file から最大 size バイトを読む (EOF で切れても読めた分を返す) 。
func readFileChunk(file *os.File, size int) []byte {
	buffer := make([]byte, size)
	read, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil
	}
	return buffer[:read]
}

// writeAll は data をすべて書き込む (書き込めなければ false) 。
func writeAll(writer io.Writer, data []byte) bool {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return false
		}
		if written == 0 {
			return false
		}
		data = data[written:]
	}
	return true
}

func (state *encodingAttempt) closeFeedWriter() {
	if state.feedWriter == nil {
		return
	}
	state.feedOnce.Do(func() {
		_ = state.feedWriter.Close()
	})
}

func (state *encodingAttempt) waitFeeder(timeout time.Duration) {
	if state.feedDone == nil {
		return
	}
	select {
	case <-state.feedDone:
	case <-time.After(timeout):
		state.task.logger().Warn(state.task.sessionLogPrefix() + " Feed task did not complete within timeout.")
	}
}

// readEncoderOutput はエンコーダーの標準出力を TS パケットとして読み取り、セグメントを切り出す。
// 移植元: VideoEncodingTask.run() の while True ループ
func (state *encodingAttempt) readEncoderOutput() {
	t := state.task
	encoder := state.encoder
	if encoder == nil || encoder.Stdout() == nil {
		return
	}

	packets := make(chan []byte, 128)
	go func() {
		defer close(packets)
		reader := bufio.NewReaderSize(encoder.Stdout(), 1<<16)
		syncBuffer := make([]byte, 1)
		for {
			if _, err := io.ReadFull(reader, syncBuffer); err != nil {
				return
			}
			if syncBuffer[0] != SyncByte {
				continue
			}
			packet := make([]byte, PacketSize)
			packet[0] = SyncByte
			if _, err := io.ReadFull(reader, packet[1:]); err != nil {
				return
			}
			select {
			case packets <- packet:
			case <-t.cancelCh:
				return
			}
		}
	}()

	// エンコーダーの stderr を読み続け、直近ログを保持する (パイプ詰まり防止)
	if encoder.Stderr() != nil {
		go state.observeEncoderStderr(encoder.Stderr())
	}

	timer := time.NewTimer(t.options.ReadTimeout)
	defer timer.Stop()
	stop := false
	for !stop {
		select {
		case <-t.cancelCh:
			stop = true
		case packet, ok := <-packets:
			if !ok {
				stop = true
				break
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(t.options.ReadTimeout)
			state.pushPacket(packet)
			if state.reachedFinal {
				stop = true
			}
		case <-timer.C:
			t.logger().Warn(fmt.Sprintf("%s[Segment %d] Encoder output read timeout.", t.sessionLogPrefix(), state.currentIndex))
			stop = true
		}
	}
}

// observeEncoderStderr はエンコーダーの stderr を行単位 (CR/LF) で読み、直近 1000 行を保持する。
// 移植元: VideoEncodingTask.run() の ObserveEncoderStderr()
func (state *encodingAttempt) observeEncoderStderr(stderr io.Reader) {
	reader := bufio.NewReaderSize(stderr, 4096)
	var line []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}
		if b == '\r' || b == '\n' {
			state.appendStderrLine(string(line))
			line = line[:0]
			continue
		}
		line = append(line, b)
	}
}

func (state *encodingAttempt) appendStderrLine(line string) {
	if line == "" {
		return
	}
	state.stderrMu.Lock()
	defer state.stderrMu.Unlock()
	if len(state.encoderStderrLines) >= encoderStderrLineLimit {
		state.encoderStderrLines = state.encoderStderrLines[1:]
	}
	state.encoderStderrLines = append(state.encoderStderrLines, line)
}

// dumpEncoderStderr は保持している stderr を出力する (移植元: DumpEncoderStderr) 。
func (state *encodingAttempt) dumpEncoderStderr(logger *slog.Logger, isWarning bool) {
	state.stderrMu.Lock()
	lines := append([]string(nil), state.encoderStderrLines...)
	state.stderrMu.Unlock()
	if len(lines) == 0 {
		return
	}
	for _, line := range lines {
		if isWarning {
			logger.Warn(line)
		} else {
			logger.Debug(line)
		}
	}
}

// pushPacket はエンコーダー出力の TS パケット 1 つを処理する。
// 移植元: VideoEncodingTask.run() の while True ループ本体 (PAT/PMT/映像/音声/その他)
func (state *encodingAttempt) pushPacket(packet []byte) {
	packetID := pid(packet)

	// PAT (Program Association Table)
	if packetID == 0x00 {
		state.patParser.push(packet)
		for {
			section, ok := state.patParser.pop()
			if !ok {
				break
			}
			if mpegCRC32(section) != 0 {
				continue
			}
			state.latestPAT = section
			// PMT の PID を取得 (Python と同じく全エントリを走査して最後の非ゼロ program_number を採用)
			for _, entry := range parsePAT(section) {
				if entry.programNumber == 0 {
					continue
				}
				state.pmtPID = entry.pid
			}
			for _, output := range packetizeSection(section, false, false, 0, 0, state.patCC) {
				state.encoded = append(state.encoded, output...)
				state.patCC = (state.patCC + 1) & 0x0F
			}
		}
		return
	}

	// PMT (Program Map Table)
	if state.pmtPID >= 0 && packetID == state.pmtPID {
		state.pmtParser.push(packet)
		for {
			section, ok := state.pmtParser.pop()
			if !ok {
				break
			}
			if mpegCRC32(section) != 0 {
				continue
			}
			state.latestPMT = section
			_, streams := parsePMT(section)
			for _, streamInfo := range streams {
				switch streamInfo.streamType {
				case 0x1B: // H.264
					if state.videoPID < 0 {
						state.videoPID = streamInfo.pid
						state.videoCodec = "H.264"
					}
				case 0x24: // H.265
					if state.videoPID < 0 {
						state.videoPID = streamInfo.pid
						state.videoCodec = "H.265"
					}
				case 0x0F: // AAC
					if state.audioPID < 0 {
						state.audioPID = streamInfo.pid
					}
				}
			}
			for _, output := range packetizeSection(section, false, false, state.pmtPID, 0, state.pmtCC) {
				state.encoded = append(state.encoded, output...)
				state.pmtCC = (state.pmtCC + 1) & 0x0F
			}
		}
		return
	}

	// 映像ストリーム
	if state.videoPID >= 0 && packetID == state.videoPID {
		state.videoParser.push(packet)
		for {
			video, ok := state.videoParser.pop()
			if !ok {
				break
			}
			if state.handleVideoPES(video) {
				return
			}
		}
		return
	}

	// 音声ストリーム
	if state.audioPID >= 0 && packetID == state.audioPID {
		state.audioParser.push(packet)
		for {
			audio, ok := state.audioParser.pop()
			if !ok {
				break
			}
			state.appendPES(audio, state.audioPID, &state.audioCC)
		}
		return
	}

	// その他のパケットは素通し
	state.encoded = append(state.encoded, packet...)
}

// handleVideoPES は映像 PES を処理する。セグメントの最終セグメントまで到達した場合のみ true を返す。
// 移植元: VideoEncodingTask.run() の「映像ストリーム」ブロック
func (state *encodingAttempt) handleVideoPES(video *pes) bool {
	t := state.task
	rawTimestamp, ok := pesTimestamp(video)
	if !ok {
		return false
	}
	currentTimestamp := rawTimestamp

	if !state.hasFirstVideoTS {
		state.firstVideoTS33 = currentTimestamp
		state.lastVideoTS33 = currentTimestamp
		state.hasFirstVideoTS = true
	}
	// 33bit ラップアラウンドの検出 (大きく逆行した場合のみラップとみなす)
	if currentTimestamp < state.lastVideoTS33 && (state.lastVideoTS33-currentTimestamp) > PCRCycle/2 {
		state.wrapOffset += PCRCycle
	}
	state.lastVideoTS33 = currentTimestamp

	// 単調増加となるよう展開した現在の DTS
	unwrapped := state.firstSegmentSourceStartDTS + (currentTimestamp - state.firstVideoTS33 + state.wrapOffset)

	// 次セグメント開始時刻はプレイリスト上の経過時間から逆算する
	currentSegment := state.segments[state.currentIndex]
	nextSegmentStart := state.firstSegmentSourceStartDTS + int64(math.RoundToEven(
		(currentSegment.PlaylistStartSeconds()+currentSegment.DurationSeconds()-state.firstSegmentPlaylistStart)*float64(HZ)))

	hasRandomAccessFrame := hasKeyFrame(video, state.videoCodec)
	isReachedBoundary := unwrapped >= nextSegmentStart
	shouldFinalize := false
	if state.isSplitPending {
		// 次に来たランダムアクセスフレームで確定する
		if hasRandomAccessFrame {
			shouldFinalize = true
		}
	} else if isReachedBoundary {
		if hasRandomAccessFrame {
			shouldFinalize = true
		} else {
			// ランダムアクセスフレームまで現在のセグメントを延長
			state.isSplitPending = true
		}
	}

	if !shouldFinalize {
		state.appendPES(video, state.videoPID, &state.videoCC)
		return false
	}

	// 現在のセグメントを確定
	data := append([]byte(nil), state.encoded...)
	sequence := state.currentIndex
	state.currentIndex++
	if state.currentIndex >= len(state.segments) {
		state.reachedFinal = true
		t.completeSegment(state.segments, sequence, data, true)
		state.flushCollectedKeyFrames()
		t.logger().Info(t.sessionLogPrefix() + " Reached the final segment.")
		// 最終セグメントの場合は現在の映像 PES を破棄する
		return true
	}

	t.completeSegment(state.segments, sequence, data, false)
	t.logger().Info(fmt.Sprintf("%s[Segment %d] Successfully Encoded HLS Segment.", t.sessionLogPrefix(), sequence))

	// 新しいセグメント用のデータと状態を初期化
	state.encoded = nil
	state.isSplitPending = false
	t.logger().Info(fmt.Sprintf("%s[Segment %d] Encoding...", t.sessionLogPrefix(), state.currentIndex))
	state.flushCollectedKeyFrames()

	// 新しいセグメントの先頭に PAT と PMT を追加
	if state.latestPAT != nil {
		for _, output := range packetizeSection(state.latestPAT, false, false, 0, 0, state.patCC) {
			state.encoded = append(state.encoded, output...)
			state.patCC = (state.patCC + 1) & 0x0F
		}
	}
	if state.latestPMT != nil {
		for _, output := range packetizeSection(state.latestPMT, false, false, state.pmtPID, 0, state.pmtCC) {
			state.encoded = append(state.encoded, output...)
			state.pmtCC = (state.pmtCC + 1) & 0x0F
		}
	}

	// 現在の映像 PES を新しいセグメントへ追加する
	state.appendPES(video, state.videoPID, &state.videoCC)
	return false
}

// flushCollectedKeyFrames はセグメント境界で収集済みキーフレームの保存を試みる。
// 移植元: VideoEncodingTask.run() の FlushCollectedSegmentMap()
func (state *encodingAttempt) flushCollectedKeyFrames() {
	if state.queue == nil {
		return
	}
	state.task.flushKeyFrames(state.ctx, state.queue)
}

// appendPES は PES を TS パケット化して現在のセグメントへ追加する。
func (state *encodingAttempt) appendPES(p *pes, packetPID int, continuityCounter *int) {
	for _, output := range packetizePES(p.payload, packetPID, *continuityCounter) {
		state.encoded = append(state.encoded, output...)
		*continuityCounter = (*continuityCounter + 1) & 0x0F
	}
}
