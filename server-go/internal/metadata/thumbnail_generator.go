package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// ThumbnailRenderParams はサムネイル描画に必要な入力。
// フレーム抽出・エンコードは FFmpeg、採点・顔検出は Go の Renderer 実装で行う。
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
	// ServiceID は前処理で出力するサービス。nil の場合は tsreadex の自動選択を使う。
	ServiceID *int

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
	return g.generateAndSave(ctx, filePath, fileHash, durationSec)
}

// generateAndSave は選択した内容ハッシュを固定し、移動中に別の録画の画像名を再取得しない。
func (g *ThumbnailGenerator) generateAndSave(ctx context.Context, filePath, fileHash string, durationSec float64) error {
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
	if g.Store != nil {
		// 取り込み済みの番組情報を使い、録画余白とジャンルを代表画像の選定に反映する。
		// 移動・再解析で別のファイルへ差し替わった場合は画像を公開しない。
		var genres string
		var startMargin, endMargin float64
		var serviceID sql.NullInt64
		if err := g.Store.DB.QueryRowContext(ctx, `SELECT rp.recording_start_margin,rp.recording_end_margin,rp.genres,rv.container_format,rv.has_video_stream_changes,rp.service_id
			FROM recorded_videos rv JOIN recorded_programs rp ON rp.id=rv.recorded_program_id
			WHERE rv.file_path=? AND rv.file_hash=?`, filePath, fileHash).Scan(&startMargin, &endMargin, &genres, &params.ContainerFormat, &params.HasVideoStreamChanges, &serviceID); err != nil {
			return fmt.Errorf("load thumbnail program metadata: %w", err)
		}
		var values []Genre
		if err := json.Unmarshal([]byte(genres), &values); err != nil {
			return fmt.Errorf("decode thumbnail program genres: %w", err)
		}
		params.CandidateTimeRanges = CandidateTimeRanges(durationSec, startMargin, endMargin)
		params.FaceDetectionMode = DetermineFaceDetectionMode(values)
		if serviceID.Valid {
			id := int(serviceID.Int64)
			params.ServiceID = &id
		}
	}
	if g.Renderer == nil {
		return fmt.Errorf("thumbnail renderer is not implemented")
	}
	if err := g.Renderer.Render(ctx, params); err != nil {
		return fmt.Errorf("failed to render thumbnails: %w", err)
	}

	// thumbnail_info を DB に保存する
	if g.Store != nil {
		if err := g.Store.UpdateThumbnailInfo(ctx, filePath, fileHash, BuildThumbnailInfo(layout)); err != nil {
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
