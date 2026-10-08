package metadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// nilProbeFullJSON は実録画由来ではない、解析に必要な最小限の FFprobe 応答。
const nilProbeFullJSON = `{
	"format":{"format_name":"mpegts","duration":"60"},
	"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
		 "avg_frame_rate":"30/1","r_frame_rate":"30/1","field_order":"progressive","ts_packetsize":"188"},
		{"index":1,"codec_type":"audio","codec_name":"aac","profile":"LC","channels":2,"sample_rate":"48000"}
	]
}`

type nilProbeReply struct {
	stdout   string
	exitCode int
	err      error
}

// nilProbeRunner は実 Analyze の外部プロセス境界だけを置換する。
// 全体解析の入力パスと、実ファイルから抽出された部分解析の stdin も検証する。
type nilProbeRunner struct {
	t       *testing.T
	path    string
	data    []byte
	ctx     context.Context
	replies []nilProbeReply
	calls   int
}

func (r *nilProbeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error) {
	r.t.Helper()
	if ctx != r.ctx || name != "synthetic-ffprobe-only" {
		r.t.Fatalf("unexpected process/context: name=%q ctx=%v", name, ctx)
	}
	if r.calls >= len(r.replies) {
		r.t.Fatal("unexpected extra ffprobe invocation")
	}
	if r.calls == 0 {
		if !reflect.DeepEqual(args, FullProbeArgs(r.path)) || stdin != nil {
			r.t.Fatalf("unexpected full probe input: args=%v stdin=%d bytes", args, len(stdin))
		}
	} else {
		offset := closestMultiple(int64(len(r.data)/4), 188)
		if !reflect.DeepEqual(args, SampleProbeArgs()) || !bytes.Equal(stdin, r.data[offset:]) {
			r.t.Fatalf("sample probe did not receive the synthetic source: args=%v stdin=%d bytes", args, len(stdin))
		}
	}
	reply := r.replies[r.calls]
	r.calls++
	if reply.err != nil {
		return nil, reply.err
	}
	return &ProcessResult{Stdout: []byte(reply.stdout), Stderr: []byte("synthetic ffprobe diagnostic"), ExitCode: reply.exitCode}, nil
}

// nilProbeProgramTrap は probe 失敗から番組・時計解析へ進まないことを検証する。
type nilProbeProgramTrap struct{ t *testing.T }

func (p nilProbeProgramTrap) AnalyzeProgram(_ *RecordedVideo, _ *int64) (*RecordedProgram, bool) {
	p.t.Fatal("unplayable probe reached program analysis")
	return nil, false
}

func (p nilProbeProgramTrap) AnalyzeProgramContext(_ context.Context, _ *RecordedVideo, _ *int64, _ *int) (*RecordedProgram, bool) {
	p.t.Fatal("unplayable probe reached contextual program/clock analysis")
	return nil, false
}

func (p nilProbeProgramTrap) AnalyzeRecordingTime() (time.Time, time.Time, bool) {
	p.t.Fatal("unplayable probe reached recording clock analysis")
	return time.Time{}, time.Time{}, false
}

// nilProbeSource は PCR・番組・実録画を含まない TS null packet のみの入力を作る。
// 解析の前後で全バイトの SHA-256・サイズ・mtime・作成時刻が不変であることを検証する。
func nilProbeSource(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-not-playable.ts")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	modified := testFixedNow.Add(-time.Hour)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(data)
	t.Cleanup(func() {
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		afterHash := sha256.Sum256(actual)
		if beforeHash != afterHash || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !fileCreationTime(before).Equal(fileCreationTime(after)) {
			t.Errorf("analyzer changed source bytes/size/timestamps: before=%x/%d/%s after=%x/%d/%s", beforeHash, before.Size(), before.ModTime(), afterHash, after.Size(), after.ModTime())
		}
		t.Logf("source SHA-256 before=%x after=%x; bytes=%d; mtime before=%s after=%s; created_at_unchanged=%v", beforeHash, afterHash, after.Size(), before.ModTime().Format(time.RFC3339Nano), after.ModTime().Format(time.RFC3339Nano), fileCreationTime(before).Equal(fileCreationTime(after)))
	})
	return path
}

func nilProbePacket() []byte {
	packet := bytes.Repeat([]byte{0xff}, 188)
	copy(packet, []byte{0x47, 0x1f, 0xff, 0x10})
	return packet
}

// TestAnalyzerNilProbeRejected は FFprobe の全体/部分解析の各失敗経路を
// 本物の Analyze で通し、panic せず (nil program, nil error) に戻ることを検証する。
func TestAnalyzerNilProbeRejected(t *testing.T) {
	processFailure := errors.New("synthetic ffprobe startup failure")
	cases := []struct {
		name      string
		replies   []nilProbeReply
		zeroInput bool
		canceled  bool
	}{
		{name: "full_exit", replies: []nilProbeReply{{stdout: nilProbeFullJSON, exitCode: 1}}},
		{name: "full_empty_json", replies: []nilProbeReply{{}}},
		{name: "full_invalid_json", replies: []nilProbeReply{{stdout: `{"format":`}}},
		{name: "full_null_json", replies: []nilProbeReply{{stdout: `null`}}},
		{name: "full_missing_format", replies: []nilProbeReply{{stdout: `{}`}}},
		{name: "full_unsupported_container", replies: []nilProbeReply{{stdout: `{"format":{"format_name":"matroska"}}`}}},
		{name: "full_invalid_stream_shape", replies: []nilProbeReply{{stdout: `{"format":{"format_name":"mpegts","duration":"60"},"streams":[null]}`}}},
		{name: "full_no_duration_or_pcr", replies: []nilProbeReply{{stdout: `{"format":{"format_name":"mpegts"}}`}}},
		{name: "sample_extraction_fails", replies: []nilProbeReply{{stdout: nilProbeFullJSON}}, zeroInput: true},
		{name: "sample_exit", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {stdout: nilProbeFullJSON, exitCode: 1}}},
		{name: "sample_empty_json", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {}}},
		{name: "sample_invalid_json", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {stdout: `{"streams":`}}},
		{name: "sample_invalid_stream_shape", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {stdout: `{"streams":[null]}`}}},
		// runFFprobe は起動エラー/キャンセルも既に (nil, nil) にしている。
		// 本修正ではこの既存分類を変えず、誤成功番組を作らないことだけを要求する。
		{name: "full_process_error", replies: []nilProbeReply{{err: processFailure}}},
		{name: "sample_process_error", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {err: processFailure}}},
		{name: "full_canceled", replies: []nilProbeReply{{err: context.Canceled}}, canceled: true},
		{name: "sample_canceled", replies: []nilProbeReply{{stdout: nilProbeFullJSON}, {err: context.Canceled}}, canceled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// panic を FAIL として記録し、残りの失敗経路と source 保護も最後まで検証する。
			defer func() {
				if caught := recover(); caught != nil {
					t.Errorf("unplayable probe panicked instead of returning (nil, nil): %v", caught)
				}
			}()
			data := nilProbePacket()
			if tc.zeroInput {
				data = make([]byte, 188)
			}
			path := nilProbeSource(t, data)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			runner := &nilProbeRunner{t: t, path: path, data: data, ctx: ctx, replies: tc.replies}
			analyzer := &Analyzer{FFprobePath: "synthetic-ffprobe-only", Runner: runner, ProgramAnalyzer: nilProbeProgramTrap{t}, Logger: testLogger(t)}
			program, err := analyzer.Analyze(ctx, path)
			if program != nil || err != nil {
				t.Fatalf("unplayable probe must return (nil, nil): program=%#v err=%v", program, err)
			}
			if runner.calls != len(tc.replies) {
				t.Fatalf("ffprobe calls=%d want=%d", runner.calls, len(tc.replies))
			}
			if tc.canceled && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("caller cancellation was changed")
			}
		})
	}
}

// TestAnalyzerNilProbeValidResultsStillRejectMissingStreams は有効な probe オブジェクトを
// nil guard と混同せず、既存の codec・scramble・stream 判定を維持することを検証する。
func TestAnalyzerNilProbeValidResultsStillRejectMissingStreams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		full   string
		sample string
	}{
		{"unsupported_video", strings.ReplaceAll(nilProbeFullJSON, `"h264"`, `"vp9"`), nilProbeFullJSON},
		{"unsupported_audio", nilProbeFullJSON, strings.ReplaceAll(nilProbeFullJSON, `"aac"`, `"ac3"`)},
		{"unsupported_packet_size", strings.ReplaceAll(nilProbeFullJSON, `"188"`, `"192"`), nilProbeFullJSON},
		{"scrambled_video", nilProbeFullJSON, `{"streams":[{"index":0,"codec_type":"video"},{"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000"}]}`},
		{"scrambled_audio", nilProbeFullJSON, `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","r_frame_rate":"30/1"},{"index":1,"codec_type":"audio"}]}`},
		{"absent_full_streams", `{"format":{"format_name":"mpegts","duration":"60"},"streams":[]}`, nilProbeFullJSON},
		{"absent_sample_streams", nilProbeFullJSON, `{"streams":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 十分なハッシュ対象サイズを持たせ、誤受理が「ファイルが小さい」経路で隠れないようにする。
			data := bytes.Repeat(nilProbePacket(), 24000)
			path := nilProbeSource(t, data)
			ctx := context.Background()
			runner := &nilProbeRunner{t: t, path: path, data: data, ctx: ctx, replies: []nilProbeReply{{stdout: tc.full}, {stdout: tc.sample}}}
			analyzer := &Analyzer{FFprobePath: "synthetic-ffprobe-only", Runner: runner, ProgramAnalyzer: nilProbeProgramTrap{t}, Logger: testLogger(t)}
			program, err := analyzer.Analyze(ctx, path)
			if program != nil || err != nil || runner.calls != 2 {
				t.Fatalf("existing rejection changed: program=%#v err=%v calls=%d", program, err, runner.calls)
			}
		})
	}
}

// nilProbeDBSnapshot は全行・全列を比較し、失敗時に許可される status/updated_at だけを除外する。
// 時計だけの比較では番組情報や識別子の誤更新を見逃すため、保存済みメタデータ全体を保護する。
func nilProbeDBSnapshot(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		row := []any{}
		for i, name := range columns {
			if table == "recorded_videos" && (name == "status" || name == "updated_at") {
				continue
			}
			if raw, ok := values[i].([]byte); ok {
				values[i] = string(raw)
			}
			row = append(row, values[i])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestAnalyzerNilProbeServiceAnalysisFailed は実 Analyzer と実 Service を一時 SQLite で接続する。
// 既存番組・既知/未知の時計を保護し、新規ファイルから誤成功レコードを作らないことを検証する。
func TestAnalyzerNilProbeServiceAnalysisFailed(t *testing.T) {
	for _, stage := range []string{"full", "sample"} {
		for _, state := range []string{"new", "existing_unknown_clock", "existing_known_clock"} {
			t.Run(stage+"/"+state, func(t *testing.T) {
				defer func() {
					if caught := recover(); caught != nil {
						t.Errorf("failed probe panicked before Service could protect DB: %v", caught)
					}
				}()
				store := openTestDB(t)
				data := nilProbePacket()
				path := nilProbeSource(t, data)
				ctx := context.Background()
				if state != "new" {
					program := buildTestProgram(path, 3600)
					if state == "existing_known_clock" {
						start, end := program.StartTime.Add(-time.Minute), program.EndTime.Add(time.Minute)
						program.Video.RecordingStartTime, program.Video.RecordingEndTime = &start, &end
						program.RecordingStartMargin, program.RecordingEndMargin = 60, 60
					}
					if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
						t.Fatal(err)
					}
				}
				beforePrograms := nilProbeDBSnapshot(t, store.DB, "recorded_programs")
				beforeVideos := nilProbeDBSnapshot(t, store.DB, "recorded_videos")
				beforeChannels := nilProbeDBSnapshot(t, store.DB, "channels")
				replies := []nilProbeReply{{exitCode: 1}}
				if stage == "sample" {
					replies = []nilProbeReply{{stdout: nilProbeFullJSON}, {exitCode: 1}}
				}
				runner := &nilProbeRunner{t: t, path: path, data: data, ctx: ctx, replies: replies}
				analyzer := &Analyzer{FFprobePath: "synthetic-ffprobe-only", Runner: runner, ProgramAnalyzer: nilProbeProgramTrap{t}, Logger: testLogger(t)}
				service := &Service{Store: store, Analyzer: analyzer, Logger: testLogger(t)}
				if err := service.ProcessRecordedFile(ctx, path, ProcessOptions{ForceUpdate: true}); err != nil {
					t.Fatal(err)
				}
				if runner.calls != len(replies) {
					t.Fatalf("probe calls=%d want=%d", runner.calls, len(replies))
				}
				if !reflect.DeepEqual(beforePrograms, nilProbeDBSnapshot(t, store.DB, "recorded_programs")) ||
					!reflect.DeepEqual(beforeVideos, nilProbeDBSnapshot(t, store.DB, "recorded_videos")) ||
					!reflect.DeepEqual(beforeChannels, nilProbeDBSnapshot(t, store.DB, "channels")) {
					t.Fatal("failed probe changed saved metadata, clocks, identifiers, or row counts")
				}
				if state != "new" {
					var status string
					if err := store.DB.QueryRow("SELECT status FROM recorded_videos").Scan(&status); err != nil {
						t.Fatal(err)
					}
					if status != "AnalysisFailed" {
						t.Fatalf("status=%q want AnalysisFailed", status)
					}
				}
				t.Logf("DB metadata/clocks/row counts unchanged; stage=%s state=%s; existing status=AnalysisFailed or no new rows", stage, state)
			})
		}
	}
}
