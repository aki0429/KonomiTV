package videostream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// ---- フェイク: EncodingSession / EncodingSegment ----

type fakeSegment struct {
	mu            sync.Mutex
	index         int
	playlistStart float64
	duration      float64
	position      *int64
	dts           *int64
	status        string
	data          []byte
	setCount      int
}

func (s *fakeSegment) Index() int                    { return s.index }
func (s *fakeSegment) PlaylistStartSeconds() float64 { return s.playlistStart }
func (s *fakeSegment) DurationSeconds() float64      { return s.duration }
func (s *fakeSegment) SourceStartDTS() (int64, bool) {
	if s.dts == nil {
		return 0, false
	}
	return *s.dts, true
}
func (s *fakeSegment) SourceFilePosition() (int64, bool) {
	if s.position == nil {
		return 0, false
	}
	return *s.position, true
}
func (s *fakeSegment) MarkEncoding() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = "Encoding"
}
func (s *fakeSegment) SetEncoded(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = "Completed"
	s.data = data
	s.setCount++
}
func (s *fakeSegment) snapshot() (string, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.data
}

type fakeSession struct {
	mu          sync.Mutex
	quality     string
	options     stream.StreamEncodingOptions
	encoder     string
	video       EncodingVideoInfo
	channel     *EncodingChannel
	segments    []*fakeSegment
	info        *StreamInfo
	ensureErr   error
	keyFrames   []KeyFrame
	addCalls    int
	flushCalls  int
	advanceUsed int
}

func (s *fakeSession) LogPrefix() string                             { return "[fake]" }
func (s *fakeSession) Quality() string                               { return s.quality }
func (s *fakeSession) EncodingOptions() stream.StreamEncodingOptions { return s.options }
func (s *fakeSession) Encoder() string                               { return s.encoder }
func (s *fakeSession) Video() EncodingVideoInfo                      { return s.video }
func (s *fakeSession) Channel() *EncodingChannel                     { return s.channel }
func (s *fakeSession) TSStreamInfo() *StreamInfo                     { return s.info }
func (s *fakeSession) EnsureTSKeyFrameContext() error                { return s.ensureErr }
func (s *fakeSession) Segments() []EncodingSegment {
	result := make([]EncodingSegment, len(s.segments))
	for i, segment := range s.segments {
		result[i] = segment
	}
	return result
}
func (s *fakeSession) AddKeyFrames(_ context.Context, keyFrames []KeyFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCalls++
	s.keyFrames = append(s.keyFrames, keyFrames...)
}
func (s *fakeSession) FlushKeyFrames(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushCalls++
}

// CompleteAndAdvance は Session.CompleteAndAdvance と同じ契約 (完了と次セグメントの Encoding 化を同時に行う) 。
func (s *fakeSession) CompleteAndAdvance(sequence int, data []byte) {
	s.mu.Lock()
	s.advanceUsed++
	s.mu.Unlock()
	s.segments[sequence].SetEncoded(data)
	if sequence+1 < len(s.segments) {
		s.segments[sequence+1].MarkEncoding()
	}
}

func (s *fakeSession) sortedUniqueKeyFrames() []KeyFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]KeyFrame(nil), s.keyFrames...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].DTS != result[j].DTS {
			return result[i].DTS < result[j].DTS
		}
		return result[i].Offset < result[j].Offset
	})
	unique := result[:0]
	for i, keyFrame := range result {
		if i > 0 && keyFrame == result[i-1] {
			continue
		}
		unique = append(unique, keyFrame)
	}
	return unique
}

// ---- フェイク: ProcessRunner ----

type fakeProcess struct {
	runner *fakeRunner
	spec   ProcessSpec
	index  int

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	killOnce sync.Once
	killed   chan struct{}
	done     chan struct{}
	finish   func() // 正常終了させる (入力消費や出力公開が終わった時点で呼ぶ)
	closers  []func()

	// inputDrained は tsreadex の有限入力の記録と close が済んだことを通知する。
	// Kill による終了通知 done とは分け、未消費の入力を正常終了と取り違えない。
	inputDrained chan struct{}

	mu       sync.Mutex
	received bytes.Buffer
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *fakeProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *fakeProcess) Wait() error {
	<-p.done
	return nil
}
func (p *fakeProcess) Kill() error {
	p.killOnce.Do(func() {
		p.runner.mu.Lock()
		p.runner.killOrder = append(p.runner.killOrder, string(p.spec.Kind))
		p.runner.mu.Unlock()
		close(p.killed)
		for _, closer := range p.closers {
			closer()
		}
	})
	return nil
}
func (p *fakeProcess) receivedData() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.received.Bytes()...)
}

type fakeRunner struct {
	mu        sync.Mutex
	synth     []byte
	specs     []ProcessSpec
	procs     []*fakeProcess
	killOrder []string
	// encoderOutput は N 回目 (0 始まり) のエンコーダー起動で出力する TS を返す。
	encoderOutput func(attempt int) []byte
	// blockProcesses が true の場合、プロセスは Kill されるまで終了せず、エンコーダーは出力後も EOF にならない。
	blockProcesses bool
	startErrors    map[ProcessKind]error
	encoderStarts  int
	allSpawned     chan struct{}
}

func (r *fakeRunner) run(spec ProcessSpec) (Process, error) {
	r.mu.Lock()
	if err := r.startErrors[spec.Kind]; err != nil {
		r.mu.Unlock()
		return nil, err
	}
	process := &fakeProcess{runner: r, spec: spec, index: len(r.specs), killed: make(chan struct{}), done: make(chan struct{}), inputDrained: make(chan struct{})}
	r.specs = append(r.specs, spec)
	r.procs = append(r.procs, process)
	attempt := r.encoderStarts
	if spec.Kind == ProcessEncoder {
		r.encoderStarts++
	}
	r.mu.Unlock()

	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(process.done) }) }
	process.finish = finish
	go func() {
		<-process.killed
		finish()
	}()

	copyStdin := func(reader io.Reader) {
		go func() {
			defer close(process.inputDrained)
			buffer := make([]byte, 32*1024)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					process.mu.Lock()
					process.received.Write(buffer[:n])
					process.mu.Unlock()
				}
				if err != nil {
					break
				}
			}
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
			if !r.blockProcesses {
				finish()
			}
		}()
	}

	empty := func() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }
	switch spec.Kind {
	case ProcessPsisimux:
		process.stdout = empty()
		if r.blockProcesses {
			pr, pw := io.Pipe()
			process.stdout = pr
			process.closers = append(process.closers, func() { _ = pw.Close() })
		} else {
			finish()
		}
	case ProcessTSReadEx:
		process.stdout = empty()
		if r.blockProcesses {
			pr, pw := io.Pipe()
			process.stdout = pr
			process.closers = append(process.closers, func() { _ = pw.Close() })
		}
		if spec.StdinPipe {
			pr, pw := io.Pipe()
			process.stdin = pw
			process.closers = append(process.closers, func() { _ = pr.CloseWithError(errors.New("killed")) })
			copyStdin(pr)
		} else if spec.Stdin != nil {
			copyStdin(spec.Stdin)
		} else {
			close(process.inputDrained)
			finish()
		}
	case ProcessEncoder:
		process.stderr = empty()
		var output []byte
		if r.encoderOutput != nil {
			output = r.encoderOutput(attempt)
		}
		if r.blockProcesses {
			pr, pw := io.Pipe()
			process.stdout = pr
			go func() { _, _ = pw.Write(output) }()
			process.closers = append(process.closers, func() { _ = pw.Close(); _ = pr.Close() })
		} else {
			// 固定 oracle 用の有限入力を記録してから合成出力を公開する。
			// 実エンコーダー一般の streaming 契約ではなく、この fake だけの同期。
			// リトライ時は今回起動した tsreadex を待ち、Kill では待機を解除する。
			r.mu.Lock()
			var upstream *fakeProcess
			for i := len(r.procs) - 1; i >= 0; i-- {
				if r.procs[i].spec.Kind == ProcessTSReadEx {
					upstream = r.procs[i]
					break
				}
			}
			r.mu.Unlock()
			pr, pw := io.Pipe()
			process.stdout = pr
			process.closers = append(process.closers, func() { _ = pw.Close(); _ = pr.Close() })
			go func() {
				defer finish()
				defer pw.Close()
				if upstream != nil {
					select {
					case <-upstream.inputDrained:
					case <-process.killed:
						return
					}
				}
				select {
				case <-process.killed:
					return
				default:
				}
				_, _ = pw.Write(output)
			}()
		}
		if spec.Stdin != nil {
			// エンコーダーは入力を読み捨てる (tsreadex の出力は空)
			go func() { _, _ = io.Copy(io.Discard, spec.Stdin) }()
		}
	}
	return process, nil
}

func (r *fakeRunner) kindsInOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, len(r.specs))
	for i, spec := range r.specs {
		kinds[i] = string(spec.Kind)
	}
	return kinds
}

// encodingTestLogger はテストのログを捨てるロガー (session_test.go の testLogger と衝突しないよう別名) 。
func encodingTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestTask(session EncodingSession, runner *fakeRunner, adapt bool) *VideoEncodingTask {
	return NewVideoEncodingTask(session, EncodingTaskOptions{
		Runner:                  runner.run,
		LibraryPath:             func(name string) string { return name },
		IsHWEncCOptionAvailable: func(string, string) bool { return adapt },
		Logger:                  encodingTestLogger(),
		ReadTimeout:             5 * time.Second,
	})
}

func readSynth(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "encoding_synth.ts"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sessionFromScenario(t *testing.T, scenario encodingScenario, synthInfo *StreamInfo) *fakeSession {
	t.Helper()
	input := scenario.Input
	session := &fakeSession{
		quality: input.Quality,
		options: stream.StreamEncodingOptions{IsHEVC10bitEnabled: input.IsHEVC10bitEnabled, Is24fpsModeEnabled: input.Is24fpsModeEnabled},
		encoder: input.Encoder,
		video: EncodingVideoInfo{
			FilePath:              input.Video.FilePath,
			ContainerFormat:       input.Video.ContainerFormat,
			VideoCodec:            input.Video.VideoCodec,
			VideoScanType:         input.Video.VideoScanType,
			VideoFrameRate:        input.Video.VideoFrameRate,
			VideoResolutionWidth:  input.Video.VideoResolutionWidth,
			VideoResolutionHeight: input.Video.VideoResolutionHeight,
		},
	}
	if input.Video.ContainerFormat == "MPEG-TS" {
		session.video.FilePath = filepath.Join("testdata", "encoding_synth.ts")
	}
	if input.Channel != nil {
		session.channel = &EncodingChannel{NetworkID: input.Channel.NetworkID, TransportStreamID: input.Channel.TransportStreamID, ServiceID: input.Channel.ServiceID}
	}
	if input.UseStreamInfo {
		session.info = synthInfo
	}
	for i := range input.SegmentCount {
		segment := &fakeSegment{index: i, playlistStart: float64(i) * input.SegmentDuration, duration: input.SegmentDuration, status: "Pending"}
		if position, ok := input.SegmentPositions[strconv.Itoa(i)]; ok {
			value := position
			segment.position = &value
		}
		if dts, ok := input.SegmentDTS[strconv.Itoa(i)]; ok {
			value := dts
			segment.dts = &value
		}
		session.segments = append(session.segments, segment)
	}
	return session
}

// TestRunMatchesPythonScenarios は Python 版 run() を偽プロセスで実行して得た期待値 (argv・tsreadex に流れたバイト列・
// 切り出された HLS セグメント・リトライ回数・収集キーフレーム) と、Go 版 Run() の結果が一致することを検証する。
func TestRunMatchesPythonScenarios(t *testing.T) {
	fixture := loadEncodingFixture(t)
	synth := readSynth(t)
	synthInfo := &StreamInfo{VideoPID: fixture.Synth.VideoPID, PCRPID: fixture.Synth.PCRPID, Codec: "H.264", PacketSize: 188}

	for name, scenario := range fixture.Scenarios {
		t.Run(name, func(t *testing.T) {
			session := sessionFromScenario(t, scenario, synthInfo)
			runner := &fakeRunner{encoderOutput: func(int) []byte {
				if scenario.Input.EncoderOutputsTS {
					return synth
				}
				return nil
			}}
			task := newTestTask(session, runner, scenario.Input.AdaptResolutionAvail)
			if err := task.Run(context.Background(), scenario.Input.StartSequence); err != nil {
				t.Fatal(err)
			}

			// 起動したプロセスの種類と引数
			wantSpawns := scenario.Result.Spawns
			if len(runner.specs) != len(wantSpawns) {
				t.Fatalf("spawn 数 = %d, want %d (%v)", len(runner.specs), len(wantSpawns), runner.kindsInOrder())
			}
			for i, want := range wantSpawns {
				got := runner.specs[i]
				wantKind := map[string]ProcessKind{"psisimux": ProcessPsisimux, "tsreadex": ProcessTSReadEx}[want.Name]
				if wantKind == "" {
					wantKind = ProcessEncoder
				}
				if got.Kind != wantKind {
					t.Errorf("spawn[%d] kind = %s, want %s", i, got.Kind, wantKind)
				}
				if strings.Join(got.Args, "\x00") != strings.Join(want.Args, "\x00") {
					t.Errorf("spawn[%d] (%s) args:\n got  %s\n want %s", i, want.Name, strings.Join(got.Args, " "), strings.Join(want.Args, " "))
				}
			}

			// tsreadex の標準入力に流れたバイト列 (PAT/PMT + 入力ファイルの続き)
			for indexText, want := range scenario.Result.TSReadExStdin {
				index, _ := strconv.Atoi(indexText)
				data := runner.procs[index].receivedData()
				if len(data) != want.Length || sha256Hex(data) != want.SHA256 {
					t.Errorf("tsreadex[%d] stdin: length=%d sha=%s, want length=%d sha=%s", index, len(data), sha256Hex(data)[:12], want.Length, want.SHA256[:12])
				}
			}

			// 切り出されたセグメント
			if scenario.Result.RetryCount != task.RetryCount() {
				t.Errorf("retry count = %d, want %d", task.RetryCount(), scenario.Result.RetryCount)
			}
			if scenario.Result.IsFinished != task.IsFinished() {
				t.Errorf("is_finished = %v, want %v", task.IsFinished(), scenario.Result.IsFinished)
			}
			for i, want := range scenario.Result.Segments {
				status, data := session.segments[i].snapshot()
				if status != want.Status {
					t.Errorf("segment %d status = %s, want %s", i, status, want.Status)
				}
				if want.Length == nil {
					continue
				}
				if len(data) != *want.Length || sha256Hex(data) != *want.SHA256 {
					t.Errorf("segment %d: length=%d sha=%s, want length=%d sha=%s", i, len(data), sha256Hex(data)[:12], *want.Length, (*want.SHA256)[:12])
				}
			}

			// 収集された入力キーフレーム (DTS・オフセット順に整列し重複を畳んだもの)
			gotKeyFrames := session.sortedUniqueKeyFrames()
			if len(gotKeyFrames) != len(scenario.Result.KeyFrames) {
				t.Fatalf("キーフレーム数 = %d, want %d", len(gotKeyFrames), len(scenario.Result.KeyFrames))
			}
			for i, want := range scenario.Result.KeyFrames {
				if gotKeyFrames[i].Offset != want.Offset || gotKeyFrames[i].DTS != want.DTS {
					t.Errorf("keyframe[%d] = %+v, want %+v", i, gotKeyFrames[i], want)
				}
			}
			if session.flushCalls == 0 {
				t.Error("終了時に FlushKeyFrames が呼ばれていない")
			}
		})
	}
}

func scenarioForTest(t *testing.T, name string) (*encodingFixture, encodingScenario, []byte, *StreamInfo) {
	t.Helper()
	fixture := loadEncodingFixture(t)
	return fixture, fixture.Scenarios[name], readSynth(t), &StreamInfo{VideoPID: fixture.Synth.VideoPID, PCRPID: fixture.Synth.PCRPID, Codec: "H.264", PacketSize: 188}
}

// TestRunUsesCompleteAndAdvance は完了と次セグメントの Encoding 化を CompleteAndAdvance で原子的に行うことを検証する。
func TestRunUsesCompleteAndAdvance(t *testing.T) {
	_, scenario, synth, info := scenarioForTest(t, "ts_ffmpeg_final")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{encoderOutput: func(int) []byte { return synth }}
	if err := newTestTask(session, runner, true).Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if session.advanceUsed != 3 {
		t.Errorf("CompleteAndAdvance 呼び出し = %d, want 3 (4 セグメント中、最終セグメント以外)", session.advanceUsed)
	}
}

// TestRunRetriesThenSucceeds は PID が取得できない試行がリトライされ、成功した時点でリトライが止まることを検証する。
func TestRunRetriesThenSucceeds(t *testing.T) {
	_, scenario, synth, info := scenarioForTest(t, "ts_ffmpeg_final")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{encoderOutput: func(attempt int) []byte {
		if attempt < 2 {
			return nil
		}
		return synth
	}}
	task := newTestTask(session, runner, true)
	if err := task.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if task.RetryCount() != 2 || runner.encoderStarts != 3 {
		t.Fatalf("retry=%d encoderStarts=%d, want 2 / 3", task.RetryCount(), runner.encoderStarts)
	}
	// リトライごとに FFmpeg の -analyzeduration が 500000 ずつ増える (H.264: 1500000 → 2000000 → 2500000)
	wantAnalyze := []string{"1500000", "2000000", "2500000"}
	index := 0
	for _, spec := range runner.specs {
		if spec.Kind != ProcessEncoder {
			continue
		}
		for i, arg := range spec.Args {
			if arg == "-analyzeduration" && spec.Args[i+1] != wantAnalyze[index] {
				t.Errorf("attempt %d analyzeduration = %s, want %s", index, spec.Args[i+1], wantAnalyze[index])
			}
		}
		index++
	}
	for i, segment := range session.segments {
		if status, _ := segment.snapshot(); status != "Completed" {
			t.Errorf("segment %d status = %s", i, status)
		}
	}
}

// TestRunStopsAtMaxRetryCount はリトライが最大 10 回で止まり、エンコーダー起動が 10 回で打ち切られることを検証する。
func TestRunStopsAtMaxRetryCount(t *testing.T) {
	_, scenario, _, info := scenarioForTest(t, "ts_ffmpeg_final")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{encoderOutput: func(int) []byte { return nil }}
	task := newTestTask(session, runner, true)
	if err := task.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if runner.encoderStarts != MaxEncodingRetryCount || task.RetryCount() != MaxEncodingRetryCount {
		t.Fatalf("encoderStarts=%d retry=%d, want %d", runner.encoderStarts, task.RetryCount(), MaxEncodingRetryCount)
	}
}

// TestCancelKillsProcessesDownstreamFirst は Cancel() がエンコーダー → tsreadex → psisimux の順に kill し、
// Run がセグメントを確定させずに戻ることを検証する。
func TestCancelKillsProcessesDownstreamFirst(t *testing.T) {
	_, scenario, _, info := scenarioForTest(t, "mp4_no_channel_ffmpeg")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{blockProcesses: true, encoderOutput: func(int) []byte { return nil }}
	task := newTestTask(session, runner, true)

	done := make(chan error, 1)
	go func() { done <- task.Run(context.Background(), 0) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		runner.mu.Lock()
		spawned := len(runner.specs)
		runner.mu.Unlock()
		if spawned == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("プロセスが起動しなかった: %v", runner.kindsInOrder())
		}
		time.Sleep(5 * time.Millisecond)
	}
	task.Cancel()
	task.Cancel() // 複数回呼んでよい
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel 後に Run が戻らない")
	}

	runner.mu.Lock()
	order := strings.Join(runner.killOrder, ",")
	runner.mu.Unlock()
	if order != "encoder,tsreadex,psisimux" {
		t.Errorf("kill 順 = %s, want encoder,tsreadex,psisimux", order)
	}
	for i, segment := range session.segments {
		if segment.setCount != 0 {
			t.Errorf("キャンセルされたのに segment %d が確定した", i)
		}
	}
	if task.IsFinished() {
		t.Error("キャンセルされたタスクが finished になっている")
	}
	if runner.encoderStarts != 1 {
		t.Errorf("キャンセル後にエンコーダーが再起動された: %d", runner.encoderStarts)
	}
}

// TestRunContextCancel は ctx のキャンセルでも Cancel() と同様に戻ることを検証する。
func TestRunContextCancel(t *testing.T) {
	_, scenario, _, info := scenarioForTest(t, "ts_ffmpeg_final")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{blockProcesses: true, encoderOutput: func(int) []byte { return nil }}
	task := newTestTask(session, runner, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx, 0) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx キャンセル後に Run が戻らない")
	}
	if runner.encoderStarts != 1 {
		t.Errorf("encoderStarts = %d, want 1", runner.encoderStarts)
	}
}

// TestCancelAfterFinishedIsNoop は完了済みタスクの Cancel() が何もしないことを検証する。
func TestCancelAfterFinishedIsNoop(t *testing.T) {
	_, scenario, synth, info := scenarioForTest(t, "ts_ffmpeg_final")
	session := sessionFromScenario(t, scenario, info)
	runner := &fakeRunner{encoderOutput: func(int) []byte { return synth }}
	task := newTestTask(session, runner, true)
	if err := task.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	task.Cancel()
	if !task.IsFinished() || task.IsCancelled() {
		t.Errorf("finished=%v cancelled=%v", task.IsFinished(), task.IsCancelled())
	}
}

// TestRunErrors は起動前に検出できる異常を、プロセスを起動せずにエラーとして返すことを検証する。
func TestRunErrors(t *testing.T) {
	_, scenario, _, info := scenarioForTest(t, "ts_ffmpeg_final")

	t.Run("SourceStartDTS が未解決", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		session.segments[0].dts = nil
		runner := &fakeRunner{}
		if err := newTestTask(session, runner, true).Run(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "Source start DTS is not resolved") {
			t.Fatalf("err = %v", err)
		}
		if len(runner.specs) != 0 {
			t.Errorf("プロセスが起動された: %v", runner.kindsInOrder())
		}
	})
	t.Run("SourceFilePosition が未解決", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		session.segments[0].position = nil
		runner := &fakeRunner{}
		if err := newTestTask(session, runner, true).Run(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "Source file position is not resolved") {
			t.Fatalf("err = %v", err)
		}
		if len(runner.specs) != 0 {
			t.Errorf("プロセスが起動された: %v", runner.kindsInOrder())
		}
	})
	t.Run("開始シーケンスが範囲外", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		if err := newTestTask(session, &fakeRunner{}, true).Run(context.Background(), 99); err == nil {
			t.Fatal("エラーにならない")
		}
	})
	t.Run("未知の画質", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		session.quality = "999p"
		runner := &fakeRunner{}
		if err := newTestTask(session, runner, true).Run(context.Background(), 0); err == nil {
			t.Fatal("エラーにならない")
		}
	})
	t.Run("エンコーダーの起動失敗は起動済みプロセスを kill して返す", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		runner := &fakeRunner{blockProcesses: true, startErrors: map[ProcessKind]error{ProcessEncoder: errors.New("boom")}}
		err := newTestTask(session, runner, true).Run(context.Background(), 0)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
		runner.mu.Lock()
		order := strings.Join(runner.killOrder, ",")
		runner.mu.Unlock()
		if order != "tsreadex" {
			t.Errorf("kill 順 = %q, want tsreadex", order)
		}
	})
	t.Run("EnsureTSKeyFrameContext の失敗は再生を止めない", func(t *testing.T) {
		session := sessionFromScenario(t, scenario, info)
		session.ensureErr = errors.New("no pat")
		synth := readSynth(t)
		runner := &fakeRunner{encoderOutput: func(int) []byte { return synth }}
		if err := newTestTask(session, runner, true).Run(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	})
}

// TestFindClosestPATPMT は開始位置に最も近い PAT/PMT を探すロジックの単体検証
// (Python 版と同じ規則: 開始位置以前を優先し、無ければ以降の最初のものを使う) 。
func TestFindClosestPATPMT(t *testing.T) {
	synth := readSynth(t)
	fixture := loadEncodingFixture(t)

	// 先頭から 5000 パケット分 (Python 版の最大遡り量) を、IDR3 の位置を開始位置として探索する
	idr3 := fixture.Synth.IDRFrames[3]
	start := max(int64(0), idr3.Offset-188*5000)
	result := findClosestPATPMT(synth[start:idr3.Offset], start, idr3.Offset)
	if result.Data() == nil {
		t.Fatal("PAT/PMT が見つからない")
	}
	// 合成 TS は 30 フレームごとに PAT/PMT を出力する。IDR3 (フレーム 45) の直前の PAT はフレーム 30 の位置。
	if result.PATDistance <= 0 || result.PMTDistance <= 0 || result.PATDistance < result.PMTDistance {
		t.Errorf("distance PAT=%d PMT=%d (PAT は PMT より前にあるはず)", result.PATDistance, result.PMTDistance)
	}
	if result.PAT[0] != SyncByte || pid(result.PAT) != 0 || len(result.PAT) != 188 || len(result.PMT) != 188 {
		t.Errorf("PAT/PMT パケットが不正")
	}
	// 先頭 (位置 0) では範囲が空なので見つからない (Python 版の ts_no_patpmt と同じ)
	if empty := findClosestPATPMT(nil, 0, 0); empty.Data() != nil {
		t.Error("空の探索範囲で PAT/PMT が見つかった")
	}
	// 開始位置より後ろにしか無い場合は、後ろの最初のものをフォールバックに使う
	fallback := findClosestPATPMT(synth[:2000], 0, 0)
	if fallback.Data() == nil || fallback.PATDistance == 0 && fallback.PMTDistance == 0 {
		t.Errorf("フォールバックが使われない: %+v", fallback)
	}
}

// TestKeyFrameCollectorMatchesPython は TSKeyFrameCollector の出力が Python 版と一致し、
// チャンク分割の仕方 (10000/1000/7/1 パケット) に依存しないことを検証する。
func TestKeyFrameCollectorMatchesPython(t *testing.T) {
	fixture := loadEncodingFixture(t)
	synth := readSynth(t)
	info := &StreamInfo{VideoPID: fixture.Synth.VideoPID, PCRPID: fixture.Synth.PCRPID, Codec: "H.264", PacketSize: 188}
	if len(fixture.CollectorCases) != 12 {
		t.Fatalf("collector_cases = %d", len(fixture.CollectorCases))
	}
	for name, testCase := range fixture.CollectorCases {
		collector := NewTSKeyFrameCollector(info, testCase.InitialDTS)
		got := []KeyFramePosition{}
		position := testCase.StartOffset
		for position < int64(len(synth)) {
			end := min(int64(len(synth)), position+int64(188*testCase.ChunkPacket))
			got = append(got, collector.Push(synth[position:end], position)...)
			position = end
		}
		if len(got) != len(testCase.KeyFrames) || len(got) == 0 {
			t.Errorf("%s: キーフレーム数 = %d, want %d", name, len(got), len(testCase.KeyFrames))
			continue
		}
		for i, want := range testCase.KeyFrames {
			if got[i].SourceFilePosition != want.Offset || got[i].SourceStartDTS != want.DTS {
				t.Errorf("%s[%d] = %+v, want offset=%d dts=%d", name, i, got[i], want.Offset, want.DTS)
			}
		}
	}
}

// TestPacketizePESMatchesPython は packetizePES が biim の packetize_pes と同じパケット列を作ることを検証する
// (Run のシナリオテストでセグメントのバイト列がハッシュ一致することと合わせた確認。ここでは構造を検証する)。
func TestPacketizePES(t *testing.T) {
	for _, length := range []int{1, 100, 182, 183, 184, 185, 184 * 3, 184*3 + 7, 5000} {
		data := make([]byte, length)
		for i := range data {
			data[i] = byte(i)
		}
		packets := packetizePES(data, 0x0100, 5)
		var payload []byte
		for i, packet := range packets {
			if len(packet) != 188 || packet[0] != SyncByte || pid(packet) != 0x100 {
				t.Fatalf("len=%d packet %d invalid", length, i)
			}
			if (packet[3] & 0x0F) != byte((5+i)&0x0F) {
				t.Fatalf("len=%d packet %d cc=%d", length, i, packet[3]&0x0F)
			}
			if (i == 0) != payloadUnitStartIndicator(packet) {
				t.Fatalf("len=%d packet %d pusi", length, i)
			}
			payload = append(payload, packet[payloadOffset(packet):]...)
		}
		if !bytes.Equal(payload, data) {
			t.Errorf("len=%d: ペイロードが復元できない", length)
		}
	}
}

// TestEncodingInterfacesCompile は担当A の Session / Segment / Manager と接続する面が
// 期待どおりの interface を満たすことをコンパイル時に確認する。
func TestEncodingInterfacesCompile(t *testing.T) {
	var _ EncodingSession = (*sessionEncodingAdapter)(nil)
	var _ EncodingSegment = sessionSegmentAdapter{}
	var _ SegmentEncoder = (*VideoEncodingTask)(nil)

	// *Session は EncodingSession のメソッド群をほぼ持つが、Segments() の戻り値が []*Segment のため
	// Go のスライス非共変性により直接は満たせない。アダプタ経由で接続する。
	var _ = NewSessionEncodingAdapter
	factory := NewSessionSegmentEncoderFactory(EncodingTaskOptions{})
	if factory == nil {
		t.Fatal("factory が nil")
	}
	var _ SegmentEncoderFactory = factory
}
