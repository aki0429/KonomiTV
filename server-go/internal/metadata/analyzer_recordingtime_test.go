package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// recordingTimeProbeRunner は外部プロセスを使わず、有効な MP4 のメディア情報を返す。
type recordingTimeProbeRunner struct{}

func (recordingTimeProbeRunner) Run(_ context.Context, _ string, _ []string, _ []byte) (*ProcessResult, error) {
	return &ProcessResult{Stdout: []byte(`{
		"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"600"},
		"streams":[
			{"index":0,"codec_type":"video","codec_name":"h264","profile":"High",
			 "width":1920,"height":1080,"avg_frame_rate":"30/1","r_frame_rate":"30/1",
			 "field_order":"progressive"},
			{"index":1,"codec_type":"audio","codec_name":"aac","profile":"LC",
			 "channels":2,"sample_rate":"48000"}
		]
	}`)}, nil
}

type recordingTimeProgramStub struct {
	program *RecordedProgram
}

func (s recordingTimeProgramStub) AnalyzeProgram(_ *RecordedVideo, _ *int64) (*RecordedProgram, bool) {
	return s.program, true
}

type recordingTimeUnavailableStub struct {
	recordingTimeProgramStub
}

func (recordingTimeUnavailableStub) AnalyzeRecordingTime() (time.Time, time.Time, bool) {
	return time.Time{}, time.Time{}, false
}

// TestAnalyzerRetainsProgramWhenRecordingTimeUnavailable は Python 版の
// MetadataAnalyzer.analyze() と同様に、TOT を取得できなくても番組情報を保持することを検証する。
func TestAnalyzerRetainsProgramWhenRecordingTimeUnavailable(t *testing.T) {
	for _, name := range []string{"without time analyzer", "time analyzer returns unavailable"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("missing recording times must not panic: %v", r)
				}
			}()
			path := filepath.Join(t.TempDir(), "recorded.mp4")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(4 * 1024 * 1024); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
			program := &RecordedProgram{
				Title: "取得済み番組名", StartTime: start, EndTime: start.Add(10 * time.Minute), Duration: 600,
			}
			stub := recordingTimeProgramStub{program: program}
			var parser ProgramAnalyzer = stub
			if name == "time analyzer returns unavailable" {
				parser = recordingTimeUnavailableStub{recordingTimeProgramStub: stub}
			}
			analyzer := &Analyzer{Runner: recordingTimeProbeRunner{}, ProgramAnalyzer: parser, Logger: testLogger(t)}
			got, err := analyzer.Analyze(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Title != program.Title {
				t.Fatalf("parsed program must be retained: %#v", got)
			}
			if got.Video.RecordingStartTime != nil || got.Video.RecordingEndTime != nil {
				t.Fatal("unavailable recording times must remain nil")
			}
			if got.RecordingStartMargin != 0 || got.RecordingEndMargin != 0 || got.IsPartiallyRecorded {
				t.Fatal("recording margins and partial flag must retain default values without both timestamps")
			}
		})
	}
}

// preferredServiceProbeRunner は使われていないサービスの後に実サービスを返す。
type preferredServiceProbeRunner struct {
	recordingTimeProbeRunner
}

func (r preferredServiceProbeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error) {
	result, err := r.recordingTimeProbeRunner.Run(ctx, name, args, stdin)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(result.Stdout, &data); err != nil {
		return nil, err
	}
	data["programs"] = []map[string]any{
		{"program_num": 101, "nb_streams": 0, "pcr_pid": 0},
		{"program_num": 102, "nb_streams": 2, "pcr_pid": 256},
	}
	result.Stdout, err = json.Marshal(data)
	return result, err
}

type preferredServiceProgramStub struct {
	called       bool
	gotServiceID *int
	gotContext   any
}

func (*preferredServiceProgramStub) AnalyzeProgram(_ *RecordedVideo, _ *int64) (*RecordedProgram, bool) {
	return nil, false // 新しい context 対応経路が使われなければ番組情報のフォールバックになる。
}

func (s *preferredServiceProgramStub) AnalyzeProgramContext(ctx context.Context, video *RecordedVideo, _ *int64, preferredServiceID *int) (*RecordedProgram, bool) {
	s.called = true
	s.gotServiceID = preferredServiceID
	s.gotContext = ctx.Value(recordingTimeTestContextKey{})
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	return &RecordedProgram{
		Video: *video, Title: "選択サービス番組", StartTime: start, EndTime: start.Add(10 * time.Minute), Duration: 600,
	}, true
}

type recordingTimeTestContextKey struct{}

func TestAnalyzerPassesPreferredServiceAndContextToProgramParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.mp4")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4 * 1024 * 1024); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stub := &preferredServiceProgramStub{}
	analyzer := &Analyzer{Runner: preferredServiceProbeRunner{}, ProgramAnalyzer: stub, Logger: testLogger(t)}
	ctx := context.WithValue(context.Background(), recordingTimeTestContextKey{}, "context-marker")
	program, err := analyzer.Analyze(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.called || stub.gotServiceID == nil || *stub.gotServiceID != 102 || stub.gotContext != "context-marker" {
		t.Fatalf("preferred service and context not forwarded: called=%v service=%v marker=%v", stub.called, stub.gotServiceID, stub.gotContext)
	}
	if program == nil || program.Title != "選択サービス番組" {
		t.Fatalf("selected service program not returned: %#v", program)
	}
}

// analyzerRegressionFile は FFprobe フェイクとハッシュ計算に必要なサイズの入力を作る。
func analyzerRegressionFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".mp4")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4 * 1024 * 1024); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	old := testFixedNow.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

type callbackProgramStub struct {
	apply func(*RecordedVideo) (*RecordedProgram, bool)
}

func (s callbackProgramStub) AnalyzeProgram(video *RecordedVideo, _ *int64) (*RecordedProgram, bool) {
	return s.apply(video)
}

type callbackContextProgramStub struct {
	callbackProgramStub
	applyContext func(context.Context, *RecordedVideo) (*RecordedProgram, bool)
}

func (s callbackContextProgramStub) AnalyzeProgramContext(ctx context.Context, video *RecordedVideo, _ *int64, _ *int) (*RecordedProgram, bool) {
	return s.applyContext(ctx, video)
}

func TestAnalyzerContextCancellationDoesNotFallback(t *testing.T) {
	path := analyzerRegressionFile(t, "cancelled-program")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parser := callbackContextProgramStub{applyContext: func(_ context.Context, _ *RecordedVideo) (*RecordedProgram, bool) {
		cancel()
		return nil, false
	}}
	analyzer := &Analyzer{Runner: recordingTimeProbeRunner{}, ProgramAnalyzer: parser, Logger: testLogger(t)}
	program, err := analyzer.Analyze(ctx, path)
	if !errors.Is(err, context.Canceled) || program != nil {
		t.Fatalf("cancelled analysis must not return a filename fallback: program=%#v err=%v", program, err)
	}
}

func TestAnalyzerNilProgramResponseFallsBackSafely(t *testing.T) {
	nilProgram := func(_ *RecordedVideo) (*RecordedProgram, bool) { return nil, true }
	for _, contextual := range []bool{false, true} {
		name := "legacy"
		if contextual {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			path := analyzerRegressionFile(t, "nil-program")
			var parser ProgramAnalyzer = callbackProgramStub{apply: nilProgram}
			if contextual {
				parser = callbackContextProgramStub{applyContext: func(_ context.Context, video *RecordedVideo) (*RecordedProgram, bool) {
					return nilProgram(video)
				}}
			}
			analyzer := &Analyzer{Runner: recordingTimeProbeRunner{}, ProgramAnalyzer: parser, Logger: testLogger(t)}
			program, err := analyzer.Analyze(context.Background(), path)
			if err != nil || program == nil || program.Title != "nil-program" || program.Duration != 600 {
				t.Fatalf("nil success response must use a safe fallback: program=%#v err=%v", program, err)
			}
		})
	}
}

func TestAnalyzerRecordingMarginsRequireBothTimestamps(t *testing.T) {
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	end := start.Add(10 * time.Minute)
	cases := []struct {
		name                   string
		recordingStart         *time.Time
		recordingEnd           *time.Time
		startMargin, endMargin float64
		partial                bool
	}{
		{name: "start only", recordingStart: ptr(start.Add(-10 * time.Second))},
		{name: "end only", recordingEnd: ptr(end.Add(5 * time.Second))},
		{name: "both with margins", recordingStart: ptr(start.Add(-10 * time.Second)), recordingEnd: ptr(end.Add(5 * time.Second)), startMargin: 10, endMargin: 5},
		{name: "partial recording", recordingStart: ptr(start.Add(3 * time.Second)), recordingEnd: ptr(end.Add(-4 * time.Second)), partial: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := analyzerRegressionFile(t, "margin-program")
			parser := callbackProgramStub{apply: func(video *RecordedVideo) (*RecordedProgram, bool) {
				video.RecordingStartTime = tc.recordingStart
				video.RecordingEndTime = tc.recordingEnd
				return &RecordedProgram{Title: "マージン計算", StartTime: start, EndTime: end, Duration: 600}, true
			}}
			analyzer := &Analyzer{Runner: recordingTimeProbeRunner{}, ProgramAnalyzer: parser, Logger: testLogger(t)}
			program, err := analyzer.Analyze(context.Background(), path)
			if err != nil || program == nil {
				t.Fatalf("analysis failed: %v", err)
			}
			if program.RecordingStartMargin != tc.startMargin || program.RecordingEndMargin != tc.endMargin || program.IsPartiallyRecorded != tc.partial {
				t.Fatalf("unexpected margins/partial: start=%v end=%v partial=%v", program.RecordingStartMargin, program.RecordingEndMargin, program.IsPartiallyRecorded)
			}
		})
	}
}
