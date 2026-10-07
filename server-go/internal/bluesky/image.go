package bluesky

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	// 画像サイズの取得に必要なフォーマットを登録する
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// errWebPConversionUnsupported は WebP への再エンコードが必要な画像を Go 側で処理できない場合のエラー。
var errWebPConversionUnsupported = errors.New("webp conversion is not supported")

// prepareAndUploadImage はアップロードされた 1 枚の画像を Bluesky の画像 embed 要素へ変換する。
// Python 版 BlueskyAPI._prepareAndUploadImage() の移植。
//
// 注意: Python 版は blob 上限 (2MB) を超える画像を Pillow で WebP に再エンコードするが、
// Go 標準ライブラリ (および golang.org/x/image) には WebP エンコーダが存在しないため、
// 上限を超える画像は再エンコードせずエラーとして扱う (size <= 2MB の画像は Python 版と同一に処理される) 。
func (s *Service) prepareAndUploadImage(ctx context.Context, image ImageInput) (*EmbedImage, error) {
	imageBytes := image.Data

	// Bluesky の blob 上限を超える場合だけ WebP 変換を試し、通常のキャプチャは元の JPEG をそのままアップロードする
	if len(imageBytes) > maxImageBytes {
		converted, err := convertLargeImageToWebP(imageBytes)
		if err != nil {
			return nil, err
		}
		imageBytes = converted
	}
	if len(imageBytes) > maxImageBytes {
		return nil, errors.New("image size exceeds the Bluesky limit")
	}

	// Bluesky クライアントは投稿レコード上の aspectRatio を表示枠のヒントとして使うため、
	// blob 参照だけでなく最終的な画像データの縦横比も明示する
	imageWidth, imageHeight, err := getImageSize(imageBytes)
	if err != nil {
		return nil, err
	}

	var upload UploadBlobResult
	if err := s.client.uploadBlob(ctx, imageBytes, &upload); err != nil {
		return nil, err
	}

	return &EmbedImage{
		Alt:   "",
		Image: upload.Blob,
		AspectRatio: AspectRatio{
			Height: imageHeight,
			Width:  imageWidth,
			Type:   "app.bsky.embed.defs#aspectRatio",
		},
		Type: "app.bsky.embed.images#image",
	}, nil
}

// getImageSize は画像データから Bluesky の表示比率に使う幅と高さを取得する。
func getImageSize(imageBytes []byte) (int, int, error) {
	imageConfig, _, err := image.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		// DecodeConfig に対応していない画像はフルデコードを試みる
		decoded, _, decodeErr := image.Decode(bytes.NewReader(imageBytes))
		if decodeErr != nil {
			return 0, 0, fmt.Errorf("failed to get image size: %w", err)
		}
		bounds := decoded.Bounds()
		return bounds.Dx(), bounds.Dy(), nil
	}
	return imageConfig.Width, imageConfig.Height, nil
}

// convertLargeImageToWebP は 2MB を超える画像を WebP へ再エンコードする (Python 版相当) 。
// Go には WebP エンコーダが無いため、現状は未対応であることを明示するエラーを返す。
func convertLargeImageToWebP(_ []byte) ([]byte, error) {
	return nil, errWebPConversionUnsupported
}
