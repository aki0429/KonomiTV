package metadata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// サムネイル生成の設定 (FFMpegThumbnailRenderer) 。
// 移植元: ThumbnailGenerator.WEBP_QUALITY_REPRESENTATIVE / WEBP_QUALITY_TILE / WEBP_COMPRESSION
const (
	// webpQualityRepresentative は代表サムネイルの WebP 品質 (0-100) 。
	webpQualityRepresentative = 80
	// webpQualityTile はシークバー用タイル画像の WebP 品質 (0-100) 。
	webpQualityTile = 71
	// webpCompressionLevel は WebP の圧縮レベル (0-6) 。
	webpCompressionLevel = 6
	// ffmpegFrameNamePattern は一時フレーム画像の連番ファイル名パターン (FFmpeg の image2 書式) 。
	ffmpegFrameNamePattern = "frame_%05d.png"
	// ffmpegStderrLimit はエラーメッセージに含める FFmpeg の標準エラー出力の最大バイト数。
	ffmpegStderrLimit = 4096
)

// CommandRunner は外部コマンドの実行を抽象化する。
// テストでは実プロセスを起動しないフェイクに差し替える。
type CommandRunner func(ctx context.Context, name string, args []string, env []string) error

// ExecCommandRunner は os/exec でコマンドを実行する既定の CommandRunner 。
// コマンドが異常終了した場合は標準エラー出力の先頭を添えてエラーを返す。
func ExecCommandRunner(ctx context.Context, name string, args []string, env []string) error {
	command := exec.CommandContext(ctx, name, args...)
	// env が nil の場合は親プロセスの環境変数をそのまま継承させる
	if env != nil {
		command.Env = env
	}
	stderr := &limitedWriter{limit: ffmpegStderrLimit}
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		// ctx のキャンセルでプロセスが kill された場合、終了コードではなくキャンセルをエラーとして返す
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("command was canceled: %w", ctxErr)
		}
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

// FFMpegThumbnailRenderer は FFmpeg を呼び出して実際にサムネイル画像を生成する ThumbnailRenderer 実装。
// 移植元: ThumbnailGenerator._generateAndSaveThumbnails() 以降の画像生成部分
// (Python 版の PyAV / OpenCV による処理を、Go 版では FFmpeg の CLI に置き換える) 。
//
// タイル画像は各候補位置から 1 フレームずつ抽出した 320x180 の画像を、tile フィルタで
// TileCols x TileRows の格子に並べて 1 枚の WebP として保存する。
// 代表サムネイルは候補区間の開始時刻から抽出した 1 フレームを 480x270 の WebP として保存する。
//
// HasVideoStreamChanges / ContainerFormat / FaceDetectionMode は Python 版の
// tsreadex 経由の抽出とフレームスコアリングで使うパラメータのため、この実装では使用しない。
type FFMpegThumbnailRenderer struct {
	// FFmpegPath は FFmpeg 実行ファイルのパス (空の場合は PATH 上の "ffmpeg" を使う) 。
	FFmpegPath string
	// LibraryPath は FFmpeg の共有ライブラリがあるディレクトリ。
	// 空でない場合のみ LD_LIBRARY_PATH として子プロセスに渡す (KonomiTV 同梱の ffmpeg.elf 用) 。
	LibraryPath string
	// RunCommand はコマンド実行の実装 (nil の場合は ExecCommandRunner を使う) 。
	RunCommand CommandRunner
	// Logger はログ出力先。
	Logger *slog.Logger
}

// Render はタイル画像と代表サムネイルを生成し、params の出力先に保存する。
func (r *FFMpegThumbnailRenderer) Render(ctx context.Context, params ThumbnailRenderParams) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("thumbnail rendering was canceled: %w", err)
	}
	if params.FilePath == "" {
		return fmt.Errorf("failed to render thumbnails: file path is empty")
	}
	if params.TilePath == "" || params.RepresentativePath == "" {
		return fmt.Errorf("failed to render thumbnails: output path is empty")
	}
	if params.Layout.TileCols <= 0 || params.Layout.TileRows <= 0 {
		return fmt.Errorf("failed to render thumbnails: invalid tile layout (cols=%d rows=%d)",
			params.Layout.TileCols, params.Layout.TileRows)
	}

	// 1. 出力ディレクトリが無ければ作成する
	for _, outputPath := range []string{params.TilePath, params.RepresentativePath} {
		if err := ensureThumbnailsDir(filepath.Dir(outputPath)); err != nil {
			return fmt.Errorf("failed to create thumbnail output directory for %s: %w", outputPath, err)
		}
	}

	// 2. 各候補位置から 1 フレームずつ抽出し、320x180 に縮小した一時ファイルを作る
	offsets := params.CandidateOffsets
	if params.Layout.TotalTiles > 0 && len(offsets) > params.Layout.TotalTiles {
		offsets = offsets[:params.Layout.TotalTiles]
	}
	if len(offsets) == 0 {
		return fmt.Errorf("failed to extract frames: no candidate offsets")
	}
	temporaryDirectory, err := os.MkdirTemp("", "konomitv-thumbnails-")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory for thumbnail rendering: %w", err)
	}
	// 一時ディレクトリは処理の成否にかかわらず必ず削除する
	defer func() {
		if err := os.RemoveAll(temporaryDirectory); err != nil {
			r.logger().Warn("Failed to remove temporary directory for thumbnail rendering.",
				"directory", temporaryDirectory, "error", err)
		}
	}()

	for index, offset := range offsets {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("thumbnail rendering was canceled: %w", err)
		}
		if offset < 0 {
			offset = 0
		}
		framePath := filepath.Join(temporaryDirectory, frameFileName(index+1))
		args := []string{
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-ss", formatSeconds(offset),
			"-i", params.FilePath,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:%d", TileWidth, TileHeight),
			"-f", "image2",
			"-start_number", strconv.Itoa(index + 1),
			framePath,
		}
		if err := r.runCommand(ctx, args); err != nil {
			return fmt.Errorf("failed to extract frame %d/%d at %.3f sec: %w", index+1, len(offsets), offset, err)
		}
		// FFmpeg が正常終了しても画像が出力されていない場合 (シーク位置が動画長を超えているなど) はエラーにする
		if _, err := os.Stat(framePath); err != nil {
			return fmt.Errorf("failed to extract frame %d/%d at %.3f sec: %w", index+1, len(offsets), offset, err)
		}
	}

	// 3. 抽出したフレームを格子状に並べ、タイル画像 1 枚の WebP として書き出す
	// フレームが格子の数に足りない場合、残りのマスは tile フィルタが黒で埋める
	tileFilter := fmt.Sprintf("scale=%d:%d,tile=%dx%d:color=black",
		TileWidth, TileHeight, params.Layout.TileCols, params.Layout.TileRows)
	tileArgs := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "image2",
		"-framerate", "1",
		"-start_number", "1",
		"-i", filepath.Join(temporaryDirectory, ffmpegFrameNamePattern),
		"-vf", tileFilter,
		"-frames:v", "1",
		"-c:v", "libwebp",
		"-quality", strconv.Itoa(webpQualityTile),
		"-compression_level", strconv.Itoa(webpCompressionLevel),
		params.TilePath,
	}
	if err := r.runCommand(ctx, tileArgs); err != nil {
		return fmt.Errorf("failed to generate tile image: %w", err)
	}
	if _, err := os.Stat(params.TilePath); err != nil {
		return fmt.Errorf("failed to generate tile image: %w", err)
	}

	// 4. 代表サムネイルの候補区間の開始時刻から 1 フレーム抽出し、480x270 の WebP として書き出す
	// 候補区間が無い場合は最初の候補位置を使う (移植元: ThumbnailGenerator の代表サムネイル選定)
	representativeSec := offsets[0]
	if len(params.CandidateTimeRanges) > 0 {
		representativeSec = params.CandidateTimeRanges[0][0]
	}
	if representativeSec < 0 {
		representativeSec = 0
	}
	representativeArgs := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-ss", formatSeconds(representativeSec),
		"-i", params.FilePath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d", ScoringWidth, ScoringHeight),
		"-c:v", "libwebp",
		"-quality", strconv.Itoa(webpQualityRepresentative),
		"-compression_level", strconv.Itoa(webpCompressionLevel),
		params.RepresentativePath,
	}
	if err := r.runCommand(ctx, representativeArgs); err != nil {
		return fmt.Errorf("failed to generate representative thumbnail at %.3f sec: %w", representativeSec, err)
	}
	if _, err := os.Stat(params.RepresentativePath); err != nil {
		return fmt.Errorf("failed to generate representative thumbnail at %.3f sec: %w", representativeSec, err)
	}
	return nil
}

// runCommand は FFmpeg を実行する (RunCommand が未設定の場合は ExecCommandRunner を使う) 。
func (r *FFMpegThumbnailRenderer) runCommand(ctx context.Context, args []string) error {
	runner := r.RunCommand
	if runner == nil {
		runner = ExecCommandRunner
	}
	name := r.FFmpegPath
	if name == "" {
		name = "ffmpeg"
	}
	r.logger().Debug("Running FFmpeg for thumbnail rendering.", "command", name+" "+strings.Join(args, " "))
	return runner(ctx, name, args, r.commandEnv())
}

// logger は slog.Logger を返す。
func (r *FFMpegThumbnailRenderer) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// commandEnv は子プロセスに渡す環境変数を返す。
// LibraryPath が設定されている場合のみ LD_LIBRARY_PATH を追加し、それ以外は nil を返して
// 親プロセスの環境変数をそのまま継承させる。
func (r *FFMpegThumbnailRenderer) commandEnv() []string {
	if r.LibraryPath == "" {
		return nil
	}
	environment := os.Environ()
	env := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "LD_LIBRARY_PATH="+r.LibraryPath)
}

// frameFileName は一時フレーム画像のファイル名を返す (index は 1 始まり) 。
func frameFileName(index int) string {
	return fmt.Sprintf(ffmpegFrameNamePattern, index)
}

// formatSeconds は FFmpeg の -ss に渡す時刻を秒単位の文字列にする。
func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}

// limitedWriter は先頭の limit バイトだけを保持する io.Writer 。
// FFmpeg の標準エラー出力をエラーメッセージ用に確保しつつ、出力が大量になってもメモリを圧迫しないようにする。
type limitedWriter struct {
	buffer bytes.Buffer
	limit  int
}

// Write は先頭の limit バイトまでを保持する (常に len(p) を返し、書き込み自体は成功扱いにする) 。
func (w *limitedWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - w.buffer.Len(); remaining > 0 {
		if len(p) > remaining {
			w.buffer.Write(p[:remaining])
		} else {
			w.buffer.Write(p)
		}
	}
	return len(p), nil
}

// String は保持している標準エラー出力を返す。
func (w *limitedWriter) String() string {
	return w.buffer.String()
}
