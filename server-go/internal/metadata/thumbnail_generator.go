package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// ThumbnailRenderParams はサムネイル描画に必要な入力。
// 実際のフレーム抽出 (PyAV) と画像処理 (OpenCV) は Go 標準ライブラリでは再現できないため、
// Renderer 実装に委譲する (テストではフェイクを差し込む) 。
type ThumbnailRenderParams struct {
	// FilePath は録画ファイルのパス。
	FilePath string
	// FileHash は録画ファイルのハッシュ (出力ファイル名に使う) 。
	FileHash string
	// DurationSec は動画の長さ (秒) 。
	DurationSec float64
	// Layout はタイルレイアウト。
	Layout ThumbnailLayout
	// CandidateOffsets は各候補フレームの抽出開始位置 (秒) 。
	CandidateOffsets []float64
	// CandidateTimeRanges は代表サムネイル候補の時間範囲。
	CandidateTimeRanges [][2]float64
	// FaceDetectionMode は顔検出モード ("Human" / "Anime" / "") 。
	FaceDetectionMode string
	// HasVideoStreamChanges は映像ストリームが途中で変化するかどうか。
	HasVideoStreamChanges bool
	// ContainerFormat はコンテナ形式 ("MPEG-TS" / "MPEG-4") 。
	ContainerFormat string

	// TilePath はシークバー用タイル画像の出力先。
	TilePath string
	// RepresentativePath は代表サムネイル画像の出力先。
	RepresentativePath string
}

// ThumbnailRenderer はサムネイル画像 (タイル画像・代表サムネイル) を実際に生成する。
// 実運用では FFmpeg / PyAV 相当の処理を呼び出す実装を差し込む。
type ThumbnailRenderer interface {
	Render(ctx context.Context, params ThumbnailRenderParams) error
}

// ThumbnailGenerator はプレイヤーのシークバー用サムネイルタイル画像と代表サムネイルを生成する。
// 移植元: ThumbnailGenerator
//
// レイアウト計算・候補位置計算・thumbnail_info の生成・DB 保存は Python 版と同一の挙動を再現する。
// 実際のフレーム抽出と画像エンコードは Renderer に委譲する (未指定の場合は何も生成しない) 。
type ThumbnailGenerator struct {
	// Store は thumbnail_info の保存先。
	Store *Store
	// ThumbnailsDir はサムネイル画像ディレクトリ。
	ThumbnailsDir string
	// Renderer は画像生成の実装 (nil の場合は画像を生成しない) 。
	Renderer ThumbnailRenderer
	// Logger はログ出力先。
	Logger *slog.Logger
}

// logger は slog.Logger を返す。
func (g *ThumbnailGenerator) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// GenerateAndSave はサムネイルタイル画像と代表サムネイルを生成し、thumbnail_info を DB に保存する。
// 移植元: ThumbnailGenerator.generateAndSave() / fromRecordedProgram()
func (g *ThumbnailGenerator) GenerateAndSave(ctx context.Context, filePath string, durationSec float64) error {
	if durationSec <= 0 {
		return fmt.Errorf("invalid duration: %f", durationSec)
	}
	fileHash, err := g.fileHashFor(filePath)
	if err != nil {
		return err
	}
	layout := CalculateThumbnailLayout(durationSec)
	candidateOffsets := CalculateCandidateOffsets(durationSec, layout)
	candidateRanges := CandidateTimeRanges(durationSec, 0, 0)
	tilePath, representativePath := ThumbnailPaths(g.ThumbnailsDir, fileHash)

	params := ThumbnailRenderParams{
		FilePath:            filePath,
		FileHash:            fileHash,
		DurationSec:         durationSec,
		Layout:              layout,
		CandidateOffsets:    candidateOffsets,
		CandidateTimeRanges: candidateRanges,
		TilePath:            tilePath,
		RepresentativePath:  representativePath,
	}
	if g.Renderer == nil {
		return fmt.Errorf("thumbnail renderer is not implemented")
	}
	if err := g.Renderer.Render(ctx, params); err != nil {
		return fmt.Errorf("failed to render thumbnails: %w", err)
	}

	// thumbnail_info を DB に保存する
	if g.Store != nil {
		if err := g.Store.UpdateThumbnailInfo(ctx, filePath, BuildThumbnailInfo(layout)); err != nil {
			return err
		}
	}
	return nil
}

// fileHashFor は録画ファイルのハッシュを DB から取得する (見つからない場合は空文字を返さない) 。
func (g *ThumbnailGenerator) fileHashFor(filePath string) (string, error) {
	if g.Store == nil {
		return "", fmt.Errorf("store is not configured")
	}
	rows, err := g.Store.FindVideoSummariesByPaths(context.Background(), []string{filePath})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || rows[0].FileHash == "" {
		return "", fmt.Errorf("recorded video not found for %s", filePath)
	}
	return rows[0].FileHash, nil
}

// ensureThumbnailsDir はサムネイルディレクトリが無ければ作成する。
func ensureThumbnailsDir(directory string) error {
	if directory == "" {
		return fmt.Errorf("thumbnails directory is empty")
	}
	if _, err := os.Stat(directory); err == nil {
		return nil
	}
	return os.MkdirAll(directory, 0o755)
}

// TileDimensions はタイル画像全体の寸法を返す (デバッグ・検証用) 。
func TileDimensions(layout ThumbnailLayout) (int, int) {
	return layout.TileImageWidth, layout.TileImageHeight
}
