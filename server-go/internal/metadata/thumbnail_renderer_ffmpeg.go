package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"math/rand/v2"
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
// 採点用に 480x270 で抽出した同じフレームを使い、候補区間の最高得点を代表画像にする。
//
// 映像ストリームが変化する MPEG-TS は tsreadex 経由で順次デコードする。
type FFMpegThumbnailRenderer struct {
	// FFmpegPath は FFmpeg 実行ファイルのパス (空の場合は PATH 上の "ffmpeg" を使う) 。
	FFmpegPath string
	// TSReadExPath は MPEG-TS の前処理ツール。空の場合は PATH 上の "tsreadex" を使う。
	TSReadExPath string
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

	// 2. 採点・代表画像用に 480x270 の一時フレームを作る。タイル化時に 320x180 に縮小する。
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

	var scores []thumbnailFrameScore
	var scoreIndices []int
	sequential := params.HasVideoStreamChanges && params.ContainerFormat == "MPEG-TS"
	if sequential {
		if err := r.extractTSReadExFrames(ctx, params, offsets, temporaryDirectory); err != nil {
			return fmt.Errorf("failed to extract frames through tsreadex: %w", err)
		}
	}
	for index, offset := range offsets {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("thumbnail rendering was canceled: %w", err)
		}
		if offset < 0 {
			offset = 0
		}
		framePath := filepath.Join(temporaryDirectory, frameFileName(index+1))
		options := []string{
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:%d", ScoringWidth, ScoringHeight),
			"-f", "image2",
			"-start_number", strconv.Itoa(index + 1),
		}
		if !sequential {
			if err := r.extractFrame(ctx, params.FilePath, offset, framePath, options); err != nil {
				if errors.Is(err, errThumbnailFrameUnavailable) {
					// 本家と同じく、読めなかった候補だけを黒画像にして他の候補を使う。
					if err := writeBlackThumbnailFrame(framePath); err != nil {
						return err
					}
					r.logger().Warn("Thumbnail frame unavailable; using black frame.", "offset", offset)
				} else {
					return fmt.Errorf("failed to extract frame %d/%d at %.3f sec: %w", index+1, len(offsets), offset, err)
				}
			}
		}
		// 本家の判定時刻は抽出オフセットではなくタイル上の idx * interval。
		if !thumbnailTimeInCandidateRanges(float64(index)*params.Layout.TileIntervalSec, params.CandidateTimeRanges) {
			continue
		}
		frame, err := readThumbnailScoreFrame(framePath)
		if err != nil {
			return err
		}
		score, err := scoreThumbnailFrame(ctx, frame, params.FaceDetectionMode)
		if err != nil {
			return fmt.Errorf("failed to score thumbnail frame %d: %w", index+1, err)
		}
		scores = append(scores, score)
		scoreIndices = append(scoreIndices, index)
	}
	best := selectThumbnailFrame(scores, params.FaceDetectionMode)
	representativeIndex := 0
	if best >= 0 {
		representativeIndex = scoreIndices[best]
	} else {
		// 本家と同じく、区間内に候補がなければ抽出済みフレームから無作為に選ぶ。
		representativeIndex = rand.IntN(len(offsets))
		r.logger().Warn("No frames found in candidate intervals; selecting a random frame.")
	}
	representativeFrame := filepath.Join(temporaryDirectory, frameFileName(representativeIndex+1))

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

	// 4. 再シークせず、実際に採点したフレームをそのまま代表画像にする。
	representativeArgs := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", representativeFrame,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d", ScoringWidth, ScoringHeight),
		"-c:v", "libwebp",
		"-quality", strconv.Itoa(webpQualityRepresentative),
		"-compression_level", strconv.Itoa(webpCompressionLevel),
		params.RepresentativePath,
	}
	if err := r.runCommand(ctx, representativeArgs); err != nil {
		return fmt.Errorf("failed to generate representative thumbnail: %w", err)
	}
	if _, err := os.Stat(params.RepresentativePath); err != nil {
		return fmt.Errorf("failed to generate representative thumbnail: %w", err)
	}
	r.logger().Debug("Representative thumbnail selected.", "index", representativeIndex, "face_detection_mode", params.FaceDetectionMode)
	return nil
}

var errThumbnailFrameUnavailable = errors.New("thumbnail frame unavailable")

// extractFrame は高速シークで末尾の I フレームを通り過ぎた場合、前からデコードして指定時刻へ進む。
func (r *FFMpegThumbnailRenderer) extractFrame(ctx context.Context, input string, offset float64, output string, options []string) error {
	prefix := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	starts := []float64{offset}
	if offset > 0 {
		starts = append(starts, max(0, offset-10))
		if offset > 10 {
			starts = append(starts, 0)
		}
	}
	for attempt, start := range starts {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := append([]string{}, prefix...)
		if attempt == 0 {
			args = append(args, "-ss", formatSeconds(offset), "-i", input)
		} else {
			if start > 0 {
				args = append(args, "-ss", formatSeconds(start))
			}
			args = append(args, "-i", input, "-ss", formatSeconds(offset-start))
		}
		args = append(args, options...)
		args = append(args, output)
		if err := r.runCommand(ctx, args); err != nil {
			return err
		}
		if info, err := os.Stat(output); err == nil && info.Size() > 0 {
			return nil
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return errThumbnailFrameUnavailable
}

func writeBlackThumbnailFrame(path string) error {
	frame := image.NewNRGBA(image.Rect(0, 0, ScoringWidth, ScoringHeight))
	for index := 3; index < len(frame.Pix); index += 4 {
		frame.Pix[index] = 255
	}
	var data bytes.Buffer
	if err := png.Encode(&data, frame); err != nil {
		return err
	}
	return os.WriteFile(path, data.Bytes(), 0600)
}

func thumbnailTimeInCandidateRanges(sec float64, ranges [][2]float64) bool {
	for _, r := range ranges {
		if r[0] <= sec && sec <= r[1] {
			return true
		}
	}
	return false
}

func readThumbnailScoreFrame(path string) (thumbnailScoreImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return thumbnailScoreImage{}, fmt.Errorf("open thumbnail frame: %w", err)
	}
	defer func() { _ = f.Close() }()
	img, err := png.Decode(f)
	if err != nil {
		return thumbnailScoreImage{}, fmt.Errorf("decode thumbnail frame: %w", err)
	}
	return newThumbnailScoreImage(img), nil
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
