package metadata

import (
	"math"
	"path/filepath"
	"strconv"
)

// サムネイル生成の設定 (ThumbnailGenerator) 。
const (
	// タイル化の設定
	BaseDurationMin      = 30    // 基準となる動画の長さ (分)
	BaseIntervalSec      = 5.0   // 基準となる間隔 (秒)
	MaxIntervalSec       = 30.0  // 最大間隔 (秒)
	ScoringWidth         = 480   // スコアリング・代表サムネイル選定時の解像度 (幅)
	ScoringHeight        = 270   // 同 (高さ)
	TileWidth            = 320   // タイル化時の 1 フレーム解像度 (幅)
	TileHeight           = 180   // 同 (高さ)
	WebPMaxSize          = 16383 // WebP の最大サイズ制限 (px)
	ThumbnailInfoVersion = 1     // サムネイル情報のバージョン
)

// ThumbnailLayout はタイルレイアウトの計算結果。
type ThumbnailLayout struct {
	// BaseTileIntervalSec は動画長に応じたタイル化間隔。
	BaseTileIntervalSec float64
	// TileIntervalSec は WebP の制限に収まるように調整された最終的な間隔。
	TileIntervalSec float64
	// TileCols はタイル画像の列数。
	TileCols int
	// TileRows はタイル画像の行数。
	TileRows int
	// TotalTiles はタイル数。
	TotalTiles int
	// TileImageWidth はタイル画像全体の幅。
	TileImageWidth int
	// TileImageHeight はタイル画像全体の高さ。
	TileImageHeight int
}

// CalculateThumbnailLayout は動画長からタイルの間隔とレイアウトを計算する。
// 移植元: ThumbnailGenerator.__calculateBaseTileInterval() / __calculateTileLayout()
func CalculateThumbnailLayout(durationSec float64) ThumbnailLayout {
	baseInterval := calculateBaseTileInterval(durationSec)

	maxCols := max(1, WebPMaxSize/TileWidth)
	maxRows := max(1, WebPMaxSize/TileHeight)
	maxTiles := maxCols * maxRows

	expectedTiles := int(math.Ceil(durationSec / baseInterval))
	tileInterval := baseInterval
	if expectedTiles > maxTiles {
		adjusted := math.Ceil((durationSec/float64(maxTiles))*10) / 10
		tileInterval = math.Max(baseInterval, adjusted)
	}

	totalTiles := int(math.Ceil(durationSec / tileInterval))
	if totalTiles < 1 {
		totalTiles = 1
	}
	tileRows := int(math.Ceil(float64(totalTiles) / float64(maxCols)))
	if tileRows > maxRows {
		totalTiles = maxCols * maxRows
		tileInterval = math.Ceil((durationSec/float64(totalTiles))*10) / 10
		tileRows = maxRows
	}

	return ThumbnailLayout{
		BaseTileIntervalSec: baseInterval,
		TileIntervalSec:     tileInterval,
		TileCols:            maxCols,
		TileRows:            tileRows,
		TotalTiles:          totalTiles,
		TileImageWidth:      TileWidth * maxCols,
		TileImageHeight:     TileHeight * tileRows,
	}
}

// calculateBaseTileInterval は動画長に応じたタイル化間隔を計算する。
// 移植元: ThumbnailGenerator.__calculateBaseTileInterval()
func calculateBaseTileInterval(durationSec float64) float64 {
	durationMin := int(durationSec / 60)
	if durationMin <= BaseDurationMin {
		return BaseIntervalSec
	}
	durationRatio := float64(durationMin) / float64(BaseDurationMin)
	// Python の round(x, 1) と同じく小数点以下 1 桁で丸める
	interval := math.Min(MaxIntervalSec, roundToDigits(BaseIntervalSec*durationRatio/math.Log2(1+durationRatio/2), 1))
	return interval
}

// roundToDigits は value を小数点以下 digits 桁で丸める (Python の round() 相当) 。
func roundToDigits(value float64, digits int) float64 {
	formatted := strconv.FormatFloat(value, 'f', digits, 64)
	parsed, err := strconv.ParseFloat(formatted, 64)
	if err != nil {
		return value
	}
	return parsed
}

// CalculateCandidateOffsets は動画長と tile_interval_sec から各候補フレームの抽出開始位置を返す。
// 移植元: ThumbnailGenerator.__calculateCandidateOffsets()
func CalculateCandidateOffsets(durationSec float64, layout ThumbnailLayout) []float64 {
	offsets := make([]float64, 0, layout.TotalTiles)
	for index := range layout.TotalTiles {
		offset := float64(index) * layout.TileIntervalSec
		if offset+0.01 > durationSec {
			offset = math.Max(0, durationSec-0.02)
		}
		offsets = append(offsets, offset)
	}
	return offsets
}

// CandidateTimeRanges は番組の 23~26% と 60~70% の時間範囲を代表サムネ候補区間として返す。
// 移植元: ThumbnailGenerator.fromRecordedProgram()
func CandidateTimeRanges(durationSec float64, recordingStartMargin float64, recordingEndMargin float64) [][2]float64 {
	startTime := recordingStartMargin
	endTime := durationSec - recordingEndMargin
	totalTime := endTime - startTime
	return [][2]float64{
		{startTime + totalTime*0.23, startTime + totalTime*0.26},
		{startTime + totalTime*0.60, startTime + totalTime*0.70},
	}
}

// DetermineFaceDetectionMode は番組のジャンル情報から顔検出モードを決定する。
// 移植元: ThumbnailGenerator.fromRecordedProgram() の DetermineFaceDetectionMode()
func DetermineFaceDetectionMode(genres []Genre) string {
	if len(genres) == 0 {
		return ""
	}
	hasAnime := false
	hasLiveAction := false
	for _, genre := range genres {
		if isAnimeGenre(genre) {
			hasAnime = true
		}
		if isLiveActionGenre(genre) {
			hasLiveAction = true
		}
	}
	if hasAnime && !hasLiveAction {
		return "Anime"
	}
	if hasLiveAction {
		return "Human"
	}
	return ""
}

// isAnimeGenre は指定されたジャンルが確実にアニメとして扱うべきジャンルかを判定する。
func isAnimeGenre(genre Genre) bool {
	if genre.Major == "アニメ・特撮" && genre.Middle != "特撮" {
		return true
	}
	if genre.Major == "映画" && genre.Middle == "アニメ" {
		return true
	}
	return false
}

// isLiveActionGenre は指定されたジャンルが確実に実写人物が重要なジャンルかを判定する。
func isLiveActionGenre(genre Genre) bool {
	switch genre.Major {
	case "ニュース・報道", "情報・ワイドショー", "ドラマ", "劇場・公演":
		return true
	case "バラエティ":
		return genre.Middle != "お笑い・コメディ"
	case "アニメ・特撮":
		return genre.Middle == "特撮"
	case "映画":
		return genre.Middle != "アニメ"
	case "ドキュメンタリー・教養":
		switch genre.Middle {
		case "社会・時事", "歴史・紀行", "インタビュー・討論":
			return true
		}
	}
	return false
}

// ThumbnailInfo は thumbnail_info カラムに保存するサムネイル情報 (schemas.ThumbnailInfo) 。
type ThumbnailInfo struct {
	Version        int `json:"version"`
	Representative struct {
		Format string `json:"format"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"representative"`
	Tile struct {
		Format      string  `json:"format"`
		ImageWidth  int     `json:"image_width"`
		ImageHeight int     `json:"image_height"`
		TileWidth   int     `json:"tile_width"`
		TileHeight  int     `json:"tile_height"`
		TotalTiles  int     `json:"total_tiles"`
		ColumnCount int     `json:"column_count"`
		RowCount    int     `json:"row_count"`
		IntervalSec float64 `json:"interval_sec"`
	} `json:"tile"`
}

// BuildThumbnailInfo は生成済みサムネイルの情報を組み立てる。
// 移植元: ThumbnailGenerator.__saveThumbnailInfoToDB()
func BuildThumbnailInfo(layout ThumbnailLayout) ThumbnailInfo {
	info := ThumbnailInfo{Version: ThumbnailInfoVersion}
	info.Representative.Format = "WebP"
	info.Representative.Width = ScoringWidth
	info.Representative.Height = ScoringHeight
	info.Tile.Format = "WebP"
	info.Tile.ImageWidth = layout.TileImageWidth
	info.Tile.ImageHeight = layout.TileImageHeight
	info.Tile.TileWidth = TileWidth
	info.Tile.TileHeight = TileHeight
	info.Tile.TotalTiles = layout.TotalTiles
	info.Tile.ColumnCount = layout.TileCols
	info.Tile.RowCount = layout.TileRows
	info.Tile.IntervalSec = layout.TileIntervalSec
	return info
}

// ThumbnailPaths は録画ファイルのハッシュからサムネイルの出力先パスを返す。
// 移植元: ThumbnailGenerator.__init__() のシークバー用タイル/代表サムネイルのパス
func ThumbnailPaths(thumbnailsDir string, fileHash string) (tilePath string, representativePath string) {
	return filepath.Join(thumbnailsDir, fileHash+"_tile.webp"), filepath.Join(thumbnailsDir, fileHash+".webp")
}
