package metadata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recordedCommand は偽の CommandRunner が記録した 1 回分のコマンド実行。
type recordedCommand struct {
	name string
	args []string
	env  []string
}

// recordingRunner は呼び出されたコマンドを記録し、出力先ファイルを偽装して作る CommandRunner 。
// onCall を差し替えると特定の呼び出しだけ失敗させられる。
type recordingRunner struct {
	calls  []recordedCommand
	onCall func(index int, name string, args []string, env []string) error
}

// newRecordingRunner は出力先 (最後の引数) にダミーの WebP を書き出す偽の CommandRunner を作る。
func newRecordingRunner() *recordingRunner {
	runner := &recordingRunner{}
	runner.onCall = func(index int, name string, args []string, env []string) error {
		return os.WriteFile(args[len(args)-1], []byte("RIFF....WEBPVP8 "), 0o644)
	}
	return runner
}

// run は CommandRunner として使う。
func (r *recordingRunner) run(ctx context.Context, name string, args []string, env []string) error {
	r.calls = append(r.calls, recordedCommand{
		name: name,
		args: append([]string(nil), args...),
		env:  append([]string(nil), env...),
	})
	if r.onCall == nil {
		return nil
	}
	return r.onCall(len(r.calls)-1, name, args, env)
}

// argAfter は引数リスト中の key の次の値を返す (見つからない場合は空文字) 。
func argAfter(args []string, key string) string {
	for index, arg := range args {
		if arg == key && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

// testThumbnailRenderParams はテスト用の ThumbnailRenderParams を組み立てる。
// 出力先は存在しないサブディレクトリ以下に置き、ディレクトリ作成も同時に確認できるようにしている。
func testThumbnailRenderParams(directory string) ThumbnailRenderParams {
	return ThumbnailRenderParams{
		FilePath:    filepath.Join(directory, "recording.ts"),
		FileHash:    "0123456789abcdef",
		DurationSec: 30,
		Layout: ThumbnailLayout{
			BaseTileIntervalSec: 5,
			TileIntervalSec:     5,
			TileCols:            3,
			TileRows:            2,
			TotalTiles:          6,
			TileImageWidth:      TileWidth * 3,
			TileImageHeight:     TileHeight * 2,
		},
		CandidateOffsets:    []float64{0, 5, 10, 15, 20, 25},
		CandidateTimeRanges: [][2]float64{{7.5, 8.5}, {18, 21}},
		TilePath:            filepath.Join(directory, "thumbnails", "0123456789abcdef_tile.webp"),
		RepresentativePath:  filepath.Join(directory, "thumbnails", "0123456789abcdef.webp"),
	}
}

// TestFFMpegThumbnailRendererRender は正常系の呼び出しと引数の中身を検証する。
func TestFFMpegThumbnailRendererRender(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)
	// TotalTiles (6) より多い候補位置を渡し、超過分が無視されることを確認する
	params.CandidateOffsets = []float64{0, 5, 10, 15, 20, 25, 30, 35}

	runner := newRecordingRunner()
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	// タイル画像と代表サムネイルが作られていること
	for _, path := range []string{params.TilePath, params.RepresentativePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("output file %s was not created: %v", path, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("output file %s is empty", path)
		}
	}

	// フレーム抽出 6 回 + タイル画像 1 回 + 代表サムネイル 1 回
	if len(runner.calls) != 8 {
		t.Fatalf("RunCommand was called %d times, want 8", len(runner.calls))
	}
	for index := 0; index < 6; index++ {
		call := runner.calls[index]
		if call.name != "ffmpeg-test" {
			t.Errorf("frame %d: command name = %q, want %q", index, call.name, "ffmpeg-test")
		}
		wantOffset := strconv.FormatFloat(float64(index)*5, 'f', 3, 64)
		if got := argAfter(call.args, "-ss"); got != wantOffset {
			t.Errorf("frame %d: -ss = %q, want %q", index, got, wantOffset)
		}
		if got := argAfter(call.args, "-i"); got != params.FilePath {
			t.Errorf("frame %d: -i = %q, want %q", index, got, params.FilePath)
		}
		if got := argAfter(call.args, "-vf"); got != fmt.Sprintf("scale=%d:%d", TileWidth, TileHeight) {
			t.Errorf("frame %d: -vf = %q, want scale=320:180", index, got)
		}
		if got := argAfter(call.args, "-frames:v"); got != "1" {
			t.Errorf("frame %d: -frames:v = %q, want 1", index, got)
		}
	}

	// タイル画像: tile フィルタに列数・行数とタイル 1 フレームの解像度が入っていること
	tileArgs := runner.calls[6].args
	tileFilter := argAfter(tileArgs, "-vf")
	if !strings.Contains(tileFilter, fmt.Sprintf("tile=%dx%d", params.Layout.TileCols, params.Layout.TileRows)) {
		t.Errorf("tile filter = %q, want to contain tile=3x2", tileFilter)
	}
	if !strings.Contains(tileFilter, fmt.Sprintf("scale=%d:%d", TileWidth, TileHeight)) {
		t.Errorf("tile filter = %q, want to contain scale=320:180", tileFilter)
	}
	if got := argAfter(tileArgs, "-i"); !strings.Contains(got, ffmpegFrameNamePattern) {
		t.Errorf("tile input = %q, want to contain %q", got, ffmpegFrameNamePattern)
	}
	if got := tileArgs[len(tileArgs)-1]; got != params.TilePath {
		t.Errorf("tile output = %q, want %q", got, params.TilePath)
	}
	if got := argAfter(tileArgs, "-c:v"); got != "libwebp" {
		t.Errorf("tile codec = %q, want libwebp", got)
	}
	if got := argAfter(tileArgs, "-quality"); got != strconv.Itoa(webpQualityTile) {
		t.Errorf("tile quality = %q, want %d", got, webpQualityTile)
	}
	if got := argAfter(tileArgs, "-compression_level"); got != strconv.Itoa(webpCompressionLevel) {
		t.Errorf("tile compression_level = %q, want %d", got, webpCompressionLevel)
	}
	if got := argAfter(tileArgs, "-frames:v"); got != "1" {
		t.Errorf("tile frames:v = %q, want 1", got)
	}

	// 代表サムネイル: 候補区間の先頭の開始時刻を使い、480x270 で保存されること
	representativeArgs := runner.calls[7].args
	if got := argAfter(representativeArgs, "-ss"); got != "7.500" {
		t.Errorf("representative -ss = %q, want 7.500", got)
	}
	if got := argAfter(representativeArgs, "-vf"); got != fmt.Sprintf("scale=%d:%d", ScoringWidth, ScoringHeight) {
		t.Errorf("representative -vf = %q, want scale=480:270", got)
	}
	if got := argAfter(representativeArgs, "-quality"); got != strconv.Itoa(webpQualityRepresentative) {
		t.Errorf("representative quality = %q, want %d", got, webpQualityRepresentative)
	}
	if got := argAfter(representativeArgs, "-compression_level"); got != strconv.Itoa(webpCompressionLevel) {
		t.Errorf("representative compression_level = %q, want %d", got, webpCompressionLevel)
	}
	if got := representativeArgs[len(representativeArgs)-1]; got != params.RepresentativePath {
		t.Errorf("representative output = %q, want %q", got, params.RepresentativePath)
	}
}

// TestFFMpegThumbnailRendererRepresentativeFallback は候補区間が無い場合に最初の候補位置を使うことを検証する。
func TestFFMpegThumbnailRendererRepresentativeFallback(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)
	params.CandidateOffsets = []float64{3.5, 8.5}
	params.CandidateTimeRanges = nil

	runner := newRecordingRunner()
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("RunCommand was called %d times, want 4", len(runner.calls))
	}
	if got := argAfter(runner.calls[3].args, "-ss"); got != "3.500" {
		t.Errorf("representative -ss = %q, want 3.500", got)
	}
}

// TestFFMpegThumbnailRendererDefaultFFmpegPath は FFmpegPath 未設定時に PATH 上の ffmpeg を使うことを検証する。
func TestFFMpegThumbnailRendererDefaultFFmpegPath(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)

	runner := newRecordingRunner()
	renderer := &FFMpegThumbnailRenderer{RunCommand: runner.run}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("RunCommand was not called")
	}
	if got := runner.calls[0].name; got != "ffmpeg" {
		t.Errorf("command name = %q, want %q", got, "ffmpeg")
	}
}

// TestFFMpegThumbnailRendererRenderCreatesOutputDirectory は出力ディレクトリが無い場合に作成されることを検証する。
func TestFFMpegThumbnailRendererRenderCreatesOutputDirectory(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)
	params.TilePath = filepath.Join(directory, "data", "thumbnails", "nested", "0123456789abcdef_tile.webp")
	params.RepresentativePath = filepath.Join(directory, "data", "other", "0123456789abcdef.webp")

	if _, err := os.Stat(filepath.Join(directory, "data")); !os.IsNotExist(err) {
		t.Fatalf("output directory already exists before rendering (err = %v)", err)
	}

	runner := newRecordingRunner()
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	for _, directoryPath := range []string{
		filepath.Join(directory, "data", "thumbnails", "nested"),
		filepath.Join(directory, "data", "other"),
	} {
		info, err := os.Stat(directoryPath)
		if err != nil {
			t.Errorf("output directory %s was not created: %v", directoryPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", directoryPath)
		}
	}
	for _, path := range []string{params.TilePath, params.RepresentativePath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("output file %s was not created: %v", path, err)
		}
	}
}

// TestFFMpegThumbnailRendererRenderCanceledContext は ctx がキャンセル済みの場合に何もせずエラーを返すことを検証する。
func TestFFMpegThumbnailRendererRenderCanceledContext(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := newRecordingRunner()
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	err := renderer.Render(ctx, params)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Render error = %v, want an error wrapping context.Canceled", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("RunCommand was called %d times, want 0", len(runner.calls))
	}
	for _, path := range []string{params.TilePath, params.RepresentativePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("output file %s was created after cancellation", path)
		}
	}
}

// TestFFMpegThumbnailRendererRenderStopsWhenCanceledMidway は処理途中のキャンセルで速やかに終了することを検証する。
func TestFFMpegThumbnailRendererRenderStopsWhenCanceledMidway(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := newRecordingRunner()
	// 2 回目のフレーム抽出の途中でキャンセルする
	runner.onCall = func(index int, name string, args []string, env []string) error {
		if index == 1 {
			cancel()
		}
		return os.WriteFile(args[len(args)-1], []byte("RIFF....WEBPVP8 "), 0o644)
	}

	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	err := renderer.Render(ctx, params)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Render error = %v, want an error wrapping context.Canceled", err)
	}
	if len(runner.calls) != 2 {
		t.Errorf("RunCommand was called %d times, want 2", len(runner.calls))
	}
	for _, path := range []string{params.TilePath, params.RepresentativePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("output file %s was created after cancellation", path)
		}
	}
}

// TestFFMpegThumbnailRendererRenderWrapsCommandError は各段階の失敗が段階の分かるエラーとして返ることを検証する。
func TestFFMpegThumbnailRendererRenderWrapsCommandError(t *testing.T) {
	sentinel := errors.New("ffmpeg failed")
	cases := []struct {
		name        string
		fail        func(params ThumbnailRenderParams, args []string) bool
		wantMessage string
		wantCalls   int
	}{
		{
			name:        "フレーム抽出の失敗",
			fail:        func(params ThumbnailRenderParams, args []string) bool { return true },
			wantMessage: "failed to extract frame",
			wantCalls:   1,
		},
		{
			name: "タイル画像生成の失敗",
			fail: func(params ThumbnailRenderParams, args []string) bool {
				return args[len(args)-1] == params.TilePath
			},
			wantMessage: "failed to generate tile image",
			wantCalls:   7,
		},
		{
			name: "代表サムネイル生成の失敗",
			fail: func(params ThumbnailRenderParams, args []string) bool {
				return args[len(args)-1] == params.RepresentativePath
			},
			wantMessage: "failed to generate representative thumbnail",
			wantCalls:   8,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			params := testThumbnailRenderParams(directory)
			runner := newRecordingRunner()
			runner.onCall = func(index int, name string, args []string, env []string) error {
				if testCase.fail(params, args) {
					return sentinel
				}
				return os.WriteFile(args[len(args)-1], []byte("RIFF....WEBPVP8 "), 0o644)
			}

			renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
			err := renderer.Render(context.Background(), params)
			if !errors.Is(err, sentinel) {
				t.Fatalf("Render error = %v, want an error wrapping %v", err, sentinel)
			}
			if !strings.Contains(err.Error(), testCase.wantMessage) {
				t.Errorf("Render error = %q, want to contain %q", err.Error(), testCase.wantMessage)
			}
			if len(runner.calls) != testCase.wantCalls {
				t.Errorf("RunCommand was called %d times, want %d", len(runner.calls), testCase.wantCalls)
			}
		})
	}
}

// TestFFMpegThumbnailRendererRenderDetectsMissingOutput はコマンド成功時も出力ファイルの有無を確認することを検証する。
func TestFFMpegThumbnailRendererRenderDetectsMissingOutput(t *testing.T) {
	directory := t.TempDir()
	params := testThumbnailRenderParams(directory)

	runner := &recordingRunner{
		onCall: func(index int, name string, args []string, env []string) error { return nil },
	}
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
	err := renderer.Render(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "failed to extract frame") {
		t.Fatalf("Render error = %v, want an error about the frame extraction stage", err)
	}
}

// TestFFMpegThumbnailRendererCommandEnv は LibraryPath が LD_LIBRARY_PATH として子プロセスに渡ることを検証する。
func TestFFMpegThumbnailRendererCommandEnv(t *testing.T) {
	t.Run("LibraryPath が設定されている場合", func(t *testing.T) {
		directory := t.TempDir()
		params := testThumbnailRenderParams(directory)
		// 既存の LD_LIBRARY_PATH は上書きされること
		t.Setenv("LD_LIBRARY_PATH", "/usr/lib/old")

		runner := newRecordingRunner()
		renderer := &FFMpegThumbnailRenderer{
			FFmpegPath:  "ffmpeg-test",
			LibraryPath: "/opt/KonomiTV/server/thirdparty/FFmpeg",
			RunCommand:  runner.run,
		}
		if err := renderer.Render(context.Background(), params); err != nil {
			t.Fatalf("Render returned error: %v", err)
		}
		if len(runner.calls) == 0 {
			t.Fatal("RunCommand was not called")
		}
		for index, call := range runner.calls {
			found := 0
			for _, entry := range call.env {
				if !strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
					continue
				}
				found++
				if entry != "LD_LIBRARY_PATH=/opt/KonomiTV/server/thirdparty/FFmpeg" {
					t.Errorf("call %d: %s", index, entry)
				}
			}
			if found != 1 {
				t.Errorf("call %d: LD_LIBRARY_PATH entries = %d, want 1", index, found)
			}
		}
	})

	t.Run("LibraryPath が設定されていない場合", func(t *testing.T) {
		directory := t.TempDir()
		params := testThumbnailRenderParams(directory)
		t.Setenv("LD_LIBRARY_PATH", "/usr/lib/old")

		runner := newRecordingRunner()
		renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
		if err := renderer.Render(context.Background(), params); err != nil {
			t.Fatalf("Render returned error: %v", err)
		}
		if len(runner.calls) == 0 {
			t.Fatal("RunCommand was not called")
		}
		for index, call := range runner.calls {
			// 親プロセスの環境変数をそのまま継承させるため、env は空 (nil) になる
			if len(call.env) != 0 {
				t.Errorf("call %d: env = %v, want nil", index, call.env)
			}
		}
	})
}

// TestFFMpegThumbnailRendererRenderRejectsInvalidParams は不正なパラメータを弾くことを検証する。
func TestFFMpegThumbnailRendererRenderRejectsInvalidParams(t *testing.T) {
	cases := map[string]func(params *ThumbnailRenderParams){
		"ファイルパスが空":  func(params *ThumbnailRenderParams) { params.FilePath = "" },
		"タイルの出力先が空": func(params *ThumbnailRenderParams) { params.TilePath = "" },
		"代表サムネイルが空": func(params *ThumbnailRenderParams) { params.RepresentativePath = "" },
		"タイルの列数が 0": func(params *ThumbnailRenderParams) { params.Layout.TileCols = 0 },
		"候補位置が空":    func(params *ThumbnailRenderParams) { params.CandidateOffsets = nil },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			params := testThumbnailRenderParams(directory)
			modify(&params)

			runner := newRecordingRunner()
			renderer := &FFMpegThumbnailRenderer{FFmpegPath: "ffmpeg-test", RunCommand: runner.run}
			if err := renderer.Render(context.Background(), params); err == nil {
				t.Fatal("Render returned nil, want an error")
			}
			if len(runner.calls) != 0 {
				t.Errorf("RunCommand was called %d times, want 0", len(runner.calls))
			}
		})
	}
}

// TestExecCommandRunnerReturnsErrorForMissingCommand は存在しないコマンドでエラーになることを検証する。
func TestExecCommandRunnerReturnsErrorForMissingCommand(t *testing.T) {
	err := ExecCommandRunner(context.Background(), "konomitv-nonexistent-command", nil, nil)
	if err == nil {
		t.Fatal("ExecCommandRunner returned nil, want an error")
	}
}

// TestExecCommandRunnerCanceledContext はキャンセル済みの ctx で context.Canceled が返ることを検証する。
func TestExecCommandRunnerCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ExecCommandRunner(ctx, "konomitv-nonexistent-command", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecCommandRunner error = %v, want an error wrapping context.Canceled", err)
	}
}

// TestFFMpegThumbnailRendererRenderWithRealFFmpeg は実際の FFmpeg でサムネイルを生成できることを検証する。
// 環境変数 KONOMITV_TEST_FFMPEG に FFmpeg 実行ファイルのパスを設定した場合のみ実行する。
func TestFFMpegThumbnailRendererRenderWithRealFFmpeg(t *testing.T) {
	ffmpegPath := os.Getenv("KONOMITV_TEST_FFMPEG")
	if ffmpegPath == "" {
		t.Skip("KONOMITV_TEST_FFMPEG is not set")
	}
	if _, err := os.Stat(ffmpegPath); err != nil {
		t.Skipf("FFmpeg was not found at %s: %v", ffmpegPath, err)
	}

	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.ts")
	// テスト用の映像 (640x360 / 15fps / 12 秒) を FFmpeg で生成する
	if err := ExecCommandRunner(context.Background(), ffmpegPath, []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15:duration=12",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "15", "-pix_fmt", "yuv420p",
		"-f", "mpegts", sourcePath,
	}, nil); err != nil {
		t.Fatalf("failed to generate a test video: %v", err)
	}

	params := testThumbnailRenderParams(directory)
	params.FilePath = sourcePath
	params.DurationSec = 12
	// 6 マスのうち 5 フレームだけを渡し、残り 1 マスが黒で埋められることも確認する
	params.CandidateOffsets = []float64{0, 2, 4, 6, 8}
	params.CandidateTimeRanges = [][2]float64{{3, 3.5}}
	params.TilePath = filepath.Join(directory, "thumbnails", "tile.webp")
	params.RepresentativePath = filepath.Join(directory, "thumbnails", "representative.webp")

	// RunCommand を設定せず、既定の os/exec ベースの実装を使う
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: ffmpegPath}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	// タイル画像は 3 列 x 2 行の 320x180 = 960x360 、代表サムネイルは 480x270 の WebP になること
	assertWebPFile(t, params.TilePath, params.Layout.TileImageWidth, params.Layout.TileImageHeight)
	assertWebPFile(t, params.RepresentativePath, ScoringWidth, ScoringHeight)
}

// assertWebPFile はファイルが WebP で、指定された寸法であることを検証する。
func assertWebPFile(t *testing.T, path string, width int, height int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("%s is not a WebP file (magic = %q)", path, data[:min(12, len(data))])
	}
	gotWidth, gotHeight, err := webpDimensions(data)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", path, err)
	}
	if gotWidth != width || gotHeight != height {
		t.Errorf("%s size = %dx%d, want %dx%d", path, gotWidth, gotHeight, width, height)
	}
}

// webpDimensions は WebP の VP8 / VP8L / VP8X チャンクから画像の寸法を読み取る。
func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 30 {
		return 0, 0, fmt.Errorf("data is too short (%d bytes)", len(data))
	}
	switch chunk := string(data[12:16]); chunk {
	case "VP8 ":
		payload := data[20:]
		// 3 バイトのフレームタグ + 3 バイトのスタートコード + 2 バイトの幅 + 2 バイトの高さ
		if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return 0, 0, fmt.Errorf("invalid VP8 frame header")
		}
		width := int(payload[6]) | int(payload[7]&0x3f)<<8
		height := int(payload[8]) | int(payload[9]&0x3f)<<8
		return width, height, nil
	case "VP8L":
		payload := data[20:]
		if len(payload) < 5 || payload[0] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L header")
		}
		bits := uint32(payload[1]) | uint32(payload[2])<<8 | uint32(payload[3])<<16 | uint32(payload[4])<<24
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8X":
		payload := data[20:]
		if len(payload) < 10 {
			return 0, 0, fmt.Errorf("invalid VP8X header")
		}
		width := int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
		height := int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
		return width + 1, height + 1, nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", chunk)
	}
}
