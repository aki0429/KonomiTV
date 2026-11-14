package metadata

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const thumbnailTSReadExTimeout = 600 * time.Second

func thumbnailTSReadExArgs(params ThumbnailRenderParams) []string {
	serviceID := -1
	if params.ServiceID != nil {
		serviceID = *params.ServiceID
	}
	// 本家と再生経路に合わせ、サービス選択・PID 固定・欠落ストリーム補完を適用する。
	return []string{"-x", "18/38/39", "-n", strconv.Itoa(serviceID), "-a", "13", "-b", "7", "-c", "5", "-u", "1", "-d", "9", params.FilePath}
}

// thumbnailIFrameSelector は時刻をまたいだ時に直前の I フレームを採用する。
// 本家は候補と同時刻の I フレームでも直前を選ぶ (先頭に直前がなければ現在を使う)。
type thumbnailIFrameSelector struct {
	offsets   []float64
	directory string
	next      int
	first     float64
	started   bool
	previous  image.Image
}

func (s *thumbnailIFrameSelector) accept(ctx context.Context, seconds float64, frame image.Image) error {
	if !s.started {
		s.started = true
		s.first = seconds
	}
	relative := seconds - s.first
	for s.next < len(s.offsets) && relative+.001 >= s.offsets[s.next] {
		chosen := s.previous
		if chosen == nil {
			chosen = frame
		}
		if err := s.save(ctx, chosen); err != nil {
			return err
		}
	}
	s.previous = frame
	return nil
}

func (s *thumbnailIFrameSelector) save(ctx context.Context, frame image.Image) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(s.directory, frameFileName(s.next+1))
	if frame == nil {
		if err := writeBlackThumbnailFrame(path); err != nil {
			return fmt.Errorf("write missing I-frame: %w", err)
		}
	} else {
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create extracted I-frame: %w", err)
		}
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		err = encoder.Encode(file, frame)
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("encode extracted I-frame: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close extracted I-frame: %w", closeErr)
		}
	}
	s.next++
	return nil
}

func (s *thumbnailIFrameSelector) finish(ctx context.Context) error {
	for s.next < len(s.offsets) {
		if err := s.save(ctx, s.previous); err != nil {
			return err
		}
	}
	return nil
}

type thumbnailFrameTimestamp struct {
	seconds float64
	valid   bool
}
type thumbnailFrameLogResult struct {
	diagnostic string
	err        error
}

var thumbnailShowInfoTimeBase = regexp.MustCompile(`config in time_base:\s*(-?\d+)/(\d+)`)
var thumbnailShowInfoPTS = regexp.MustCompile(`\bn:\s*\d+\s+pts:\s*(\S+)`)

// readThumbnailFrameTimestamps は showinfo の整数 PTS と time_base を組み合わせる。
// pts_time の表示桁では長い録画の 1 ms 境界を保持できないため使用しない。
func readThumbnailFrameTimestamps(ctx context.Context, input io.Reader, timestamps chan<- thumbnailFrameTimestamp) thumbnailFrameLogResult {
	defer close(timestamps)
	reader := bufio.NewReaderSize(input, 8192)
	diagnostic := &limitedWriter{limit: ffmpegStderrLimit}
	numerator, denominator := int64(0), int64(0)
	for {
		first, more, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return thumbnailFrameLogResult{diagnostic.String(), err}
		}
		// 長い SEI 等の診断で送信側を詰まらせない。先頭 8 KiB だけ保持し、残りは読み捨てる。
		line := string(first)
		for more {
			if err := ctx.Err(); err != nil {
				return thumbnailFrameLogResult{diagnostic.String(), err}
			}
			_, more, err = reader.ReadLine()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				return thumbnailFrameLogResult{diagnostic.String(), err}
			}
		}
		if strings.Contains(line, "showinfo") {
			if parts := thumbnailShowInfoTimeBase.FindStringSubmatch(line); parts != nil {
				numerator, _ = strconv.ParseInt(parts[1], 10, 64)
				denominator, _ = strconv.ParseInt(parts[2], 10, 64)
			}
			if parts := thumbnailShowInfoPTS.FindStringSubmatch(line); parts != nil {
				stamp := thumbnailFrameTimestamp{}
				pts, err := strconv.ParseInt(parts[1], 10, 64)
				if err == nil && (numerator <= 0 || denominator <= 0) {
					return thumbnailFrameLogResult{diagnostic.String(), fmt.Errorf("thumbnail frame time base is missing")}
				}
				if err == nil && numerator > 0 && denominator > 0 {
					stamp.seconds = float64(pts) * float64(numerator) / float64(denominator)
					stamp.valid = !math.IsInf(stamp.seconds, 0) && !math.IsNaN(stamp.seconds)
				}
				select {
				case timestamps <- stamp:
				case <-ctx.Done():
					return thumbnailFrameLogResult{diagnostic.String(), ctx.Err()}
				}
			}
		} else {
			_, _ = diagnostic.Write([]byte(line + "\n"))
		}
	}
}

// extractTSReadExFrames は入力を一度だけ開き、二つの子プロセスをパイプで接続する。
// PNG と PTS を順に消費するため、動画全体のフレーム配列をメモリへ溜め込まない。
func (r *FFMpegThumbnailRenderer) extractTSReadExFrames(parent context.Context, params ThumbnailRenderParams, offsets []float64, directory string) error {
	ctx, cancel := context.WithTimeout(parent, thumbnailTSReadExTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(offsets) == 0 {
		return fmt.Errorf("no candidate offsets for sequential extraction")
	}
	tsPath := r.TSReadExPath
	if tsPath == "" {
		tsPath = "tsreadex"
	}
	ffmpegPath := r.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	ts := exec.CommandContext(ctx, tsPath, thumbnailTSReadExArgs(params)...)
	ts.Env = r.commandEnv()
	ts.WaitDelay = 5 * time.Second
	input, err := ts.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open tsreadex output: %w", err)
	}
	defer func() { _ = input.Close() }()
	if err := ts.Start(); err != nil {
		return fmt.Errorf("start tsreadex: %w", err)
	}
	defer func() { _ = ts.Process.Kill(); _ = input.Close(); _ = ts.Wait() }()
	// NONINTRA だけでは MPEG-2 の P フレームも出るため、フィルターでも I に限定する。
	// RGB 変換後に面積補間し、本家の PyAV RGB -> INTER_AREA の順序に合わせる。
	filter := fmt.Sprintf("select='eq(pict_type,I)',format=rgb24,scale=%d:%d:flags=area,showinfo=checksum=0", ScoringWidth, ScoringHeight)
	// 本家は InvalidDataError のパケットだけを飛ばす。FFmpeg 独自の失敗率集計で
	// 残った正常フレームまで失敗扱いにしないようにする (実際の致命的な終了は後で検査する)。
	decoderArgs := []string{
		"-hide_banner", "-loglevel", "info", "-nostats", "-nostdin",
		"-filter_threads", "1", "-copyts", "-max_error_rate", "1",
		"-skip_frame", "nointra", "-threads", "1", "-f", "mpegts", "-i", "pipe:0",
		"-map", "0:v:0", "-an", "-sn", "-dn", "-vf", filter,
		"-fps_mode", "passthrough", "-threads", "1", "-c:v", "png", "-f", "image2pipe", "pipe:1",
	}
	decoder := exec.CommandContext(ctx, ffmpegPath, decoderArgs...)
	decoder.Env = r.commandEnv()
	decoder.Stdin = input
	decoder.WaitDelay = 5 * time.Second
	output, err := decoder.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open thumbnail decoder output: %w", err)
	}
	defer func() { _ = output.Close() }()
	stderr, err := decoder.StderrPipe()
	if err != nil {
		return fmt.Errorf("open thumbnail decoder diagnostics: %w", err)
	}
	defer func() { _ = stderr.Close() }()
	if err := decoder.Start(); err != nil {
		return fmt.Errorf("start thumbnail decoder: %w", err)
	}
	// Stdin が *os.File なので子が直接所有する。親の複製を残すと上流の終了を妨げる。
	_ = input.Close()
	timestamps := make(chan thumbnailFrameTimestamp, 8)
	logsDone := make(chan thumbnailFrameLogResult, 1)
	go func() { logsDone <- readThumbnailFrameTimestamps(ctx, stderr, timestamps) }()
	waited, logsRead := false, false
	defer func() {
		// 下流を先に止め、上流の書き込み待ちを解除する。cancel で診断の送信待ちも解除する。
		_ = decoder.Process.Kill()
		cancel()
		_ = output.Close()
		_ = stderr.Close()
		if !waited {
			_ = decoder.Wait()
		}
		if !logsRead {
			<-logsDone
		}
	}()
	selector := thumbnailIFrameSelector{offsets: offsets, directory: directory}
	reader := bufio.NewReader(output)
	for selector.next < len(offsets) {
		// png.Decode は画像間の正常な EOF も UnexpectedEOF にするため、先に区別する。
		if _, err := reader.Peek(1); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read sequential thumbnail frame: %w", err)
		}
		frame, decodeErr := png.Decode(reader)
		if decodeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(decodeErr, io.EOF) {
				return fmt.Errorf("decode sequential thumbnail frame: %w", decodeErr)
			}
			break
		}
		var stamp thumbnailFrameTimestamp
		select {
		case value, ok := <-timestamps:
			if !ok {
				logs := <-logsDone
				logsRead = true
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if logs.err != nil {
					return fmt.Errorf("read thumbnail decoder diagnostics: %w", logs.err)
				}
				return fmt.Errorf("thumbnail frame has no timestamp metadata")
			}
			stamp = value
		case <-ctx.Done():
			return ctx.Err()
		}
		if stamp.valid {
			if err := selector.accept(ctx, stamp.seconds, frame); err != nil {
				return err
			}
		}
	}
	if selector.next < len(offsets) {
		// EOF 後も診断を最後まで読み、エンコード失敗を正常な末尾補完と混同しない。
		extra := 0
		for range timestamps {
			extra++
		}
		logs := <-logsDone
		logsRead = true
		err := decoder.Wait()
		waited = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("thumbnail decoder failed: %w: %s", err, strings.TrimSpace(logs.diagnostic))
		}
		if logs.err != nil {
			return fmt.Errorf("read thumbnail decoder diagnostics: %w", logs.err)
		}
		if extra != 0 {
			return fmt.Errorf("thumbnail decoder emitted timestamps without images")
		}
		if selector.previous == nil {
			r.logger().Warn("Sequential thumbnail extraction produced no I-frames; using black frames.", "expected", len(offsets), "file_path", params.FilePath)
		}
		if err := selector.finish(ctx); err != nil {
			return err
		}
	}
	r.logger().Info("Sequential thumbnail frames extracted through tsreadex.", "frames", selector.next, "file_path", params.FilePath)
	return nil
}
