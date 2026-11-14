package bluesky

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"strings"
)

// ImageConverter は大きい添付画像を Bluesky の上限内へ変換する。
type ImageConverter func(context.Context, []byte) ([]byte, error)

type webPAttempt struct{ Width, Height, Quality int }

// webPAttempts は本家と同じく、解像度を維持した画質調整を先に試す。
// 小さい元画像は拡大しない。各縮小も元画像から行い、縮小の繰り返しによる劣化を避ける。
func webPAttempts(width, height int) []webPAttempt {
	longEdge := max(width, height)
	steps := make([]webPAttempt, 0, 30)
	for _, limit := range []int{longEdge, 1920, 1600, 1280, 1024, 800} {
		w, h := width, height
		if longEdge > limit {
			ratio := float64(limit) / float64(longEdge)
			w = max(1, int(float64(width)*ratio))
			h = max(1, int(float64(height)*ratio))
		}
		for _, quality := range []int{85, 80, 70, 60, 50} {
			steps = append(steps, webPAttempt{w, h, quality})
		}
	}
	return steps
}

// FFMpegImageConverter は同梱 FFmpeg の libwebp で Pillow 相当の画像変換を実行する。
// 画像の変換だけを担当し、アカウントや送信先にはアクセスしない。
type FFMpegImageConverter struct {
	// FFmpegPath は実行ファイル。空の場合は PATH の ffmpeg を使う。
	FFmpegPath string
	// LibraryPath は同梱 Linux 実行ファイルが必要とする共有ライブラリの場所。
	LibraryPath string
	// encode は画質・寸法の試行を実プロセスから分離する試験用の差し替え先。
	encode func(context.Context, []byte, webPAttempt) ([]byte, error)
}

// Convert は本家 BlueskyAPI.ConvertLargeImageToWebP と同じ順で変換を試す。
func (c *FFMpegImageConverter) Convert(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode large image: %w", err)
	}
	// Pillow の DecompressionBombError と同じ最大画素数を適用する。
	if config.Width <= 0 || config.Height <= 0 || config.Width > 178956970/config.Height {
		return nil, fmt.Errorf("image exceeds decoded pixel limit")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode large image: %w", err)
	}
	if frame, ok := decoded.(*image.Paletted); format == "gif" && ok && frame.Bounds() != image.Rect(0, 0, config.Width, config.Height) {
		// GIF の先頭フレームが部分画像でも、Pillow と同じ論理画面上の位置を保持する。
		canvas := image.NewPaletted(image.Rect(0, 0, config.Width, config.Height), frame.Palette)
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Src)
		decoded = canvas
	}
	// Pillow の RGB / RGBA へ揃え、透過度を保持した 8 bit PNG をエンコーダーへ渡す。
	bounds := decoded.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	gray16, isGray16 := decoded.(*image.Gray16)
	for y := 0; y < bounds.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < bounds.Dx(); x++ {
			value := color.NRGBAModel.Convert(decoded.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			if isGray16 {
				// Pillow の I;16 -> RGBA は整数値を 0..255 へ切り詰める。
				v := uint8(min(255, gray16.Gray16At(bounds.Min.X+x, bounds.Min.Y+y).Y))
				value = color.NRGBA{R: v, G: v, B: v, A: 255}
			}
			rgba.SetNRGBA(x, y, value)
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, rgba); err != nil {
		return nil, err
	}
	encode := c.encode
	if encode == nil {
		encode = c.encodeWebP
	}
	for _, attempt := range webPAttempts(bounds.Dx(), bounds.Dy()) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		encoded, err := encode(ctx, input.Bytes(), attempt)
		if err != nil {
			return nil, err
		}
		if len(encoded) == 0 {
			return nil, fmt.Errorf("WebP encoder returned an empty image")
		}
		if len(encoded) <= maxImageBytes {
			return encoded, nil
		}
	}
	return nil, fmt.Errorf("image size exceeds the Bluesky limit after WebP conversion")
}

func (c *FFMpegImageConverter) encodeWebP(ctx context.Context, input []byte, attempt webPAttempt) ([]byte, error) {
	name := c.FFmpegPath
	if name == "" {
		name = "ffmpeg"
	}
	command := exec.CommandContext(ctx, name, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-threads", "1", "-filter_threads", "1", "-f", "image2pipe", "-i", "pipe:0", "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", attempt.Width, attempt.Height),
		"-pix_fmt", "bgra", "-c:v", "libwebp", "-lossless", "0", "-quality", fmt.Sprint(attempt.Quality),
		"-compression_level", "6", "-threads", "1", "-f", "webp", "pipe:1")
	if c.LibraryPath != "" {
		environment := []string{}
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "LD_LIBRARY_PATH=") {
				environment = append(environment, value)
			}
		}
		command.Env = append(environment, "LD_LIBRARY_PATH="+c.LibraryPath)
	}
	command.Stdin = bytes.NewReader(input)
	var output, stderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 4096 {
			message = message[:4096]
		}
		return nil, fmt.Errorf("failed to convert image to WebP: %w: %s", err, message)
	}
	return output.Bytes(), nil
}
