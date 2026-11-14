package metadata

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// SaveRepresentativeFromImage はキャプチャを自動生成と同じ寸法・画質の代表画像にする。
// 移植元: VideosRouter.VideoThumbnailUpdateAPI() の SaveThumbnail()。
func (r *FFMpegThumbnailRenderer) SaveRepresentativeFromImage(ctx context.Context, source image.Image, output string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureThumbnailsDir(filepath.Dir(output)); err != nil {
		return err
	}
	// 出力と同じファイルシステムの一時領域を使い、失敗時には既存画像を保持する。
	work, err := os.MkdirTemp(filepath.Dir(output), ".capture-thumbnail-")
	if err != nil {
		return fmt.Errorf("failed to prepare thumbnail output: %w", err)
	}
	defer os.RemoveAll(work)
	// OpenCV IMREAD_COLOR と同様に透過度を捨て、RGB を 8 bit の不透明画像に揃える。
	bounds := source.Bounds()
	rgb := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for x := 0; x < bounds.Dx(); x++ {
			value := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			value.A = 255
			rgb.SetNRGBA(x, y, value)
		}
	}
	input := filepath.Join(work, "source.png")
	file, err := os.Create(input)
	if err != nil {
		return err
	}
	err = png.Encode(file, rgb)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	temporary := filepath.Join(work, "representative.webp")
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input,
		"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d:flags=area", ScoringWidth, ScoringHeight),
		"-c:v", "libwebp", "-quality", fmt.Sprint(webpQualityRepresentative),
		"-compression_level", fmt.Sprint(webpCompressionLevel), temporary}
	if err := r.runCommand(ctx, args); err != nil {
		return fmt.Errorf("failed to encode capture thumbnail: %w", err)
	}
	if info, err := os.Stat(temporary); err != nil || info.Size() == 0 {
		return fmt.Errorf("capture thumbnail output is missing or empty")
	}
	if err := os.Rename(temporary, output); err != nil {
		return fmt.Errorf("failed to replace thumbnail: %w", err)
	}
	return nil
}
