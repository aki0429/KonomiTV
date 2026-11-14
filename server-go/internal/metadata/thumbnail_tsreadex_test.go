package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestThumbnailTSReadExReference(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name      string
			Offsets   []float64
			ServiceID *int `json:"service_id"`
			Frames    []struct {
				Time        *float64
				Label       byte
				Kind        string
				DecodeError bool `json:"decode_error"`
			}
			Expected []byte
			Args     []string `json:"tsreadex_args"`
		}
	}
	data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_tsreadex_reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 14 {
		t.Fatalf("fixture cases=%d", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			params := ThumbnailRenderParams{FilePath: "recording.ts", ServiceID: tc.ServiceID}
			if got := thumbnailTSReadExArgs(params); !reflect.DeepEqual(got, tc.Args) {
				t.Errorf("arguments=%v want=%v", got, tc.Args)
			}
			if tc.Expected == nil {
				return
			} // 依存プロセスの起動失敗・期限は後続の実プロセス試験で確認する。
			selector := thumbnailIFrameSelector{offsets: tc.Offsets, directory: t.TempDir()}
			for _, frame := range tc.Frames {
				if frame.Kind != "I" || frame.DecodeError || frame.Time == nil {
					continue
				}
				if err := selector.accept(context.Background(), *frame.Time, thumbnailLabelFrame(frame.Label)); err != nil {
					t.Fatal(err)
				}
				if selector.next >= len(tc.Offsets) {
					break
				}
			}
			if err := selector.finish(context.Background()); err != nil {
				t.Fatal(err)
			}
			var got []byte
			for n := range tc.Expected {
				got = append(got, thumbnailFrameLabel(t, filepath.Join(selector.directory, frameFileName(n+1))))
			}
			if !reflect.DeepEqual(got, tc.Expected) {
				t.Errorf("selected=%v want=%v", got, tc.Expected)
			}
		})
	}
}

func TestThumbnailTSReadExFixtureDetectsPIDChanges(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_tsreadex_input.mpegts"))
	if err != nil {
		t.Fatal(err)
	}
	// 本家は先頭・25%・末尾8MiBの各位置から最初の PMT だけを見る。
	// 205KiB の最小動画では末尾位置が先頭と重複し、切り替えは検出対象にならない。
	if detectTSVideoStreamChanges(filepath.Join("testdata", "thumbnail_tsreadex_input.mpegts"), nil) {
		t.Fatal("small fixture no longer follows conservative source sampling")
	}
	// 時刻を変えず、二つの区間の間に規格上の null packet を挟んで別の採取位置を作る。
	// 中央付近の TS パケット境界に挿入し、PES パケットの中身を変更しない。
	boundary := (len(data) / 2 / 188) * 188
	packet := make([]byte, 188)
	for n := range packet {
		packet[n] = 0xff
	}
	copy(packet, []byte{0x47, 0x1f, 0xff, 0x10})
	path := filepath.Join(t.TempDir(), "sampled.ts")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data[:boundary]); err != nil {
		t.Fatal(err)
	}
	padding := make([]byte, 188*(10*1024*1024/188))
	for p := 0; p < len(padding); p += 188 {
		copy(padding[p:], packet)
	}
	if _, err := file.Write(padding); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data[boundary:]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !detectTSVideoStreamChanges(path, nil) {
		t.Fatal("sampled real PMT PID change was not detected")
	}
}

func thumbnailLabelFrame(label byte) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			img.SetNRGBA(x, y, color.NRGBA{R: label, G: label, B: label, A: 255})
		}
	}
	return img
}

func thumbnailFrameLabel(t *testing.T, path string) byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, a := img.At(0, 0).RGBA()
	if a != 65535 {
		t.Fatal("frame alpha is not opaque")
	}
	return byte(r >> 8)
}

type thumbnailPipeTestFrame struct {
	PTS   string
	Label byte
}
type thumbnailPipeTestConfig struct {
	Directory       string
	Frames          []thumbnailPipeTestFrame
	TimeBase        string
	Stall           bool
	ExitError       bool
	BadPNG          bool
	MissingMetadata bool
	LongDiagnostic  bool
}
type thumbnailPipeTestProcess struct {
	PID         int
	Args        []string
	LibraryPath string
}

// TestMain は実際の子プロセスを使用してパイプ・終了待ち・キャンセルも検査する。
func TestMain(m *testing.M) {
	if raw := os.Getenv("KONOMITV_TEST_THUMBNAIL_PIPE"); raw != "" {
		var config thumbnailPipeTestConfig
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			panic(err)
		}
		thumbnailPipeTestHelper(config)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func thumbnailPipeTestHelper(config thumbnailPipeTestConfig) {
	role := "decoder"
	if len(os.Args) > 1 && os.Args[1] == "-x" {
		role = "tsreadex"
	}
	data, _ := json.Marshal(thumbnailPipeTestProcess{os.Getpid(), os.Args[1:], os.Getenv("LD_LIBRARY_PATH")})
	if err := os.WriteFile(filepath.Join(config.Directory, role+".json"), data, 0600); err != nil {
		panic(err)
	}
	if role == "tsreadex" {
		_, _ = io.WriteString(os.Stdout, "ts-stream")
		if config.Stall {
			for {
				time.Sleep(time.Hour)
			}
		}
		return
	}
	input := make([]byte, 9)
	if _, err := io.ReadFull(os.Stdin, input); err != nil || string(input) != "ts-stream" {
		os.Exit(5)
	}
	if config.Stall {
		for {
			time.Sleep(time.Hour)
		}
	}
	if config.ExitError {
		_, _ = io.WriteString(os.Stderr, "synthetic decoder failure\n")
		os.Exit(3)
	}
	if config.LongDiagnostic {
		_, _ = io.WriteString(os.Stderr, strings.Repeat("X", 300000)+"\n")
	}
	timeBase := config.TimeBase
	if timeBase == "" {
		timeBase = "1/90000"
	}
	_, _ = fmt.Fprintf(os.Stderr, "[Parsed_showinfo_3 @ helper] config in time_base: %s, frame_rate: 30/1\n", timeBase)
	for n, frame := range config.Frames {
		if !config.MissingMetadata {
			_, _ = fmt.Fprintf(os.Stderr, "[Parsed_showinfo_3 @ helper] n: %3d pts: %s pts_time:0 pos:-1 fmt:rgb24\n", n, frame.PTS)
		}
		if config.BadPNG {
			_, _ = io.WriteString(os.Stdout, "broken png")
			return
		}
		if err := png.Encode(os.Stdout, thumbnailLabelFrame(frame.Label)); err != nil {
			return
		}
	}
}

func setupThumbnailPipeTest(t *testing.T, config thumbnailPipeTestConfig) (*FFMpegThumbnailRenderer, string) {
	t.Helper()
	config.Directory = t.TempDir()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_TEST_THUMBNAIL_PIPE", string(data))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &FFMpegThumbnailRenderer{FFmpegPath: executable, TSReadExPath: executable, LibraryPath: filepath.Join(config.Directory, "libraries"), Logger: testLogger(t)}, config.Directory
}

func readThumbnailPipeTestProcess(t *testing.T, directory, role string) thumbnailPipeTestProcess {
	t.Helper()
	var value thumbnailPipeTestProcess
	data, err := os.ReadFile(filepath.Join(directory, role+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertThumbnailPipeTestProcessesStopped(t *testing.T, directory string) {
	t.Helper()
	for _, role := range []string{"tsreadex", "decoder"} {
		p := readThumbnailPipeTestProcess(t, directory, role)
		if runtime.GOOS != "windows" {
			process, err := os.FindProcess(p.PID)
			if err == nil {
				err = process.Signal(syscall.Signal(0))
				_ = process.Release()
			}
			if err == nil {
				t.Errorf("%s child PID %d remains alive", role, p.PID)
			}
		}
	}
}

func TestThumbnailTSReadExProcessPipeline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   thumbnailPipeTestConfig
		offsets  []float64
		expected []byte
	}{
		{"exact-tail", thumbnailPipeTestConfig{Frames: []thumbnailPipeTestFrame{{"900000000", 10}, {"900090000", 20}, {"900180000", 30}}}, []float64{0, 1, 2, 5}, []byte{10, 10, 20, 30}},
		{"first-frame-stop", thumbnailPipeTestConfig{Frames: []thumbnailPipeTestFrame{{"900000000", 10}, {"900090000", 20}}}, []float64{0}, []byte{10}},
		{"no-time", thumbnailPipeTestConfig{Frames: []thumbnailPipeTestFrame{{"NOPTS", 90}, {"900000000", 10}, {"900090000", 20}}}, []float64{0, 1, 2}, []byte{10, 10, 20}},
		{"no-frames", thumbnailPipeTestConfig{}, []float64{0, 1, 2}, []byte{0, 0, 0}},
		{"long-diagnostics", thumbnailPipeTestConfig{LongDiagnostic: true, Frames: []thumbnailPipeTestFrame{{"900000000", 10}}}, []float64{0, 1}, []byte{10, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, directory := setupThumbnailPipeTest(t, tc.config)
			frames := t.TempDir()
			id := 101
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			params := ThumbnailRenderParams{FilePath: "input recording.ts", ServiceID: &id}
			if err := r.extractTSReadExFrames(ctx, params, tc.offsets, frames); err != nil {
				t.Fatal(err)
			}
			for n, want := range tc.expected {
				if got := thumbnailFrameLabel(t, filepath.Join(frames, frameFileName(n+1))); got != want {
					t.Errorf("frame %d=%d want=%d", n, got, want)
				}
			}
			ts := readThumbnailPipeTestProcess(t, directory, "tsreadex")
			if !reflect.DeepEqual(ts.Args, thumbnailTSReadExArgs(params)) {
				t.Errorf("tsreadex arguments=%v", ts.Args)
			}
			decoder := readThumbnailPipeTestProcess(t, directory, "decoder")
			if decoder.LibraryPath != r.LibraryPath || argAfter(decoder.Args, "-max_error_rate") != "1" || argAfter(decoder.Args, "-skip_frame") != "nointra" || !strings.Contains(argAfter(decoder.Args, "-vf"), "eq(pict_type,I)") || !strings.Contains(argAfter(decoder.Args, "-vf"), "format=rgb24,scale=480:270:flags=area") {
				t.Errorf("decoder settings=%+v", decoder)
			}
			assertThumbnailPipeTestProcessesStopped(t, directory)
		})
	}
}

func TestThumbnailTSReadExProcessFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  thumbnailPipeTestConfig
		message string
	}{
		{"decoder-exit", thumbnailPipeTestConfig{ExitError: true}, "synthetic decoder failure"},
		{"broken-png", thumbnailPipeTestConfig{BadPNG: true, Frames: []thumbnailPipeTestFrame{{"0", 10}}}, "decode sequential thumbnail frame"},
		{"missing-metadata", thumbnailPipeTestConfig{MissingMetadata: true, Frames: []thumbnailPipeTestFrame{{"0", 10}}}, "no timestamp metadata"},
		{"missing-timebase", thumbnailPipeTestConfig{TimeBase: "0/0", Frames: []thumbnailPipeTestFrame{{"0", 10}}}, "time base is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, directory := setupThumbnailPipeTest(t, tc.config)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := r.extractTSReadExFrames(ctx, ThumbnailRenderParams{FilePath: "recording.ts"}, []float64{0, 1}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v want %s", err, tc.message)
			}
			assertThumbnailPipeTestProcessesStopped(t, directory)
		})
	}
}

func TestThumbnailTSReadExCancellationStopsChildren(t *testing.T) {
	r, directory := setupThumbnailPipeTest(t, thumbnailPipeTestConfig{Stall: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.extractTSReadExFrames(ctx, ThumbnailRenderParams{FilePath: "recording.ts"}, []float64{0}, t.TempDir())
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, a := os.Stat(filepath.Join(directory, "tsreadex.json"))
		_, b := os.Stat(filepath.Join(directory, "decoder.json"))
		if a == nil && b == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pipeline helper processes did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled pipeline did not stop")
	}
	assertThumbnailPipeTestProcessesStopped(t, directory)
}

func TestThumbnailFrameTimestampPrecision(t *testing.T) {
	input := "[Parsed_showinfo_3 @ test] config in time_base: 1/90000, frame_rate: 30/1\n" +
		"[Parsed_showinfo_3 @ test] n: 0 pts: 900000000000 pts_time:1e+07 fmt:rgb24\n" +
		"[Parsed_showinfo_3 @ test] n: 1 pts: 900000000090 pts_time:1e+07 fmt:rgb24\n"
	stamps := make(chan thumbnailFrameTimestamp, 2)
	result := readThumbnailFrameTimestamps(context.Background(), strings.NewReader(input), stamps)
	if result.err != nil {
		t.Fatal(result.err)
	}
	first, second := <-stamps, <-stamps
	if !first.valid || !second.valid || second.seconds-first.seconds < .00099 || second.seconds-first.seconds > .00101 {
		t.Fatalf("timestamps=%+v/%+v", first, second)
	}
	if value := strconv.FormatFloat(first.seconds, 'f', 0, 64); value != "10000000" {
		t.Fatalf("first=%s", value)
	}
}

func TestThumbnailTSReadExDeadline(t *testing.T) {
	r, _ := setupThumbnailPipeTest(t, thumbnailPipeTestConfig{Stall: true})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := r.extractTSReadExFrames(ctx, ThumbnailRenderParams{FilePath: "recording.ts"}, []float64{0}, t.TempDir())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("deadline did not stop extraction")
	}
}

func TestThumbnailTSReadExStartFailures(t *testing.T) {
	for _, role := range []string{"tsreadex", "decoder"} {
		t.Run(role, func(t *testing.T) {
			r, _ := setupThumbnailPipeTest(t, thumbnailPipeTestConfig{Stall: true})
			missing := filepath.Join(t.TempDir(), "missing-executable")
			if role == "tsreadex" {
				r.TSReadExPath = missing
			} else {
				r.FFmpegPath = missing
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := r.extractTSReadExFrames(ctx, ThumbnailRenderParams{FilePath: "recording.ts"}, []float64{0}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "start ") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
