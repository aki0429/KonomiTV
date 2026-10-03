// Package captures はキャプチャ画像 (サーバー設定で指定されたフォルダ内の JPEG / PNG) を扱う。
//
// 移植元: server/app/routers/CapturesRouter.py
package captures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// 画像のデコードのために登録する
	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/draw"

	"github.com/aki0429/KonomiTV/server-go/internal/exif"
)

// PythonFloat は Pydantic v2 と同じ形式 (整数値にも ".0" を付与する) で出力される浮動小数点数。
// パッケージ単体で利用できるよう、API 層の pydanticFloat64 とは別に定義している。
type PythonFloat float64

// MarshalJSON は Pydantic v2 互換の浮動小数点数文字列を出力する。
func (value PythonFloat) MarshalJSON() ([]byte, error) {
	text := strconv.FormatFloat(float64(value), 'g', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return []byte(text), nil
}

// PageSize はキャプチャ一覧 API の 1 ページあたりの表示件数。
// グリッドの列数 (デスクトップ: 4 列、スマホ横: 3 列、スマホ縦: 2 列) の公倍数にすることで、
// 最終行が中途半端な列数にならないようにする (4, 3, 2 の最小公倍数は 12) 。
const PageSize = 36

// ThumbnailMaxSize はサムネイル画像の最大サイズ (長辺のピクセル数) 。
const ThumbnailMaxSize = 400

// captureExtensions はキャプチャ画像として認識する拡張子。
var captureExtensions = []string{".jpg", ".jpeg", ".png"}

// Metadata は EXIF XPComment に格納されたキャプチャメタデータ (server/app/schemas.py の CaptureMetadata 相当) 。
type Metadata struct {
	// CapturedAt はキャプチャの撮影時刻 (ISO8601 フォーマット) 。
	CapturedAt string `json:"captured_at"`
	// CapturedPlaybackPosition は番組開始時刻から換算したキャプチャ位置 (秒) 。
	CapturedPlaybackPosition PythonFloat `json:"captured_playback_position"`
	// NetworkID はチャンネルの network_id。
	NetworkID int `json:"network_id"`
	// ServiceID はチャンネルの service_id。
	ServiceID int `json:"service_id"`
	// EventID は番組の event_id。
	EventID int `json:"event_id"`
	// Title は番組名。
	Title string `json:"title"`
	// Description は番組概要。
	Description string `json:"description"`
	// StartTime は番組開始時刻 (ISO8601 フォーマット) 。
	StartTime string `json:"start_time"`
	// EndTime は番組終了時刻 (ISO8601 フォーマット) 。
	EndTime string `json:"end_time"`
	// Duration は番組長 (秒) 。
	Duration PythonFloat `json:"duration"`
	// CaptionText は字幕のテキスト (字幕が表示されていなかった場合は null) 。
	CaptionText *string `json:"caption_text"`
	// IsCaptionComposited はキャプチャに字幕が合成されているかどうか。
	IsCaptionComposited bool `json:"is_caption_composited"`
	// IsCommentComposited はキャプチャにコメントが合成されているかどうか。
	IsCommentComposited bool `json:"is_comment_composited"`
}

// Capture はキャプチャ画像のメタデータ (server/app/schemas.py の Capture 相当) 。
type Capture struct {
	// Filename はファイル名 (拡張子含む) 。
	Filename string
	// FileSize はファイルサイズ (バイト) 。
	FileSize int64
	// FileModifiedAt はファイルの最終更新日時 (= キャプチャ撮影日時の近似値) 。UTC で保持する。
	FileModifiedAt time.Time
	// MimeType は MIME タイプ (image/jpeg または image/png) 。
	MimeType string
	// ImageWidth は画像の幅 (ピクセル) 。
	ImageWidth int
	// ImageHeight は画像の高さ (ピクセル) 。
	ImageHeight int
	// Metadata は EXIF XPComment から抽出したキャプチャメタデータ (EXIF がない場合は nil) 。
	Metadata *Metadata
}

// File は保存先フォルダから収集したキャプチャ画像ファイル。
type File struct {
	// Filename はファイル名。
	Filename string
	// ModifiedAt はファイルの更新日時 (Unix 時間) 。
	ModifiedAt float64
	// Path はファイルのフルパス。
	Path string
}

// UploadFolders はサーバー設定からキャプチャの保存先フォルダのリストを取得する。
// 存在しないフォルダは除外して返す。
func UploadFolders(folders []string) []string {
	existing := []string{}
	for _, folder := range folders {
		if info, err := os.Stat(folder); err == nil && info.IsDir() {
			existing = append(existing, folder)
		}
	}
	return existing
}

// FindFile は指定されたファイル名のキャプチャ画像を保存先フォルダから検索する。
// ディレクトリトラバーサル対策を含む。見つからなかった場合は空文字を返す。
func FindFile(folders []string, filename string) string {
	for _, folder := range folders {
		// ディレクトリトラバーサル対策
		// ファイル名に ".." や絶対パスが含まれる場合は、保存先フォルダの外を指すため拒否する
		if !isSafeFilename(filename) {
			return ""
		}
		filepath_ := filepath.Join(folder, filename)
		if info, err := os.Stat(filepath_); err == nil && info.Mode().IsRegular() {
			return filepath_
		}
	}
	return ""
}

// isSafeFilename はファイル名が保存先フォルダ内に収まるかどうかを判定する。
// 移植元: Python 版の upload_folder.joinpath(filename).resolve().relative_to(upload_folder.resolve())
func isSafeFilename(filename string) bool {
	if filename == "" {
		return false
	}
	// ディレクトリ区切り文字を含むファイル名は受け付けない (サブディレクトリへのトラバーサルを防ぐ)
	if strings.ContainsAny(filename, "/\\") {
		return false
	}
	// 親ディレクトリ参照や現在ディレクトリ参照を拒否する
	if filename == "." || filename == ".." || strings.Contains(filename, "..") {
		return false
	}
	// Windows のドライブレターを拒否する
	if strings.Contains(filename, ":") {
		return false
	}
	return true
}

// ExtractCaptureInfo はキャプチャ画像ファイルからメタデータを抽出する。
// 画像の幅・高さと EXIF XPComment フィールドに格納された JSON メタデータを読み取る。
func ExtractCaptureInfo(path string) Capture {
	info, err := os.Stat(path)
	capture := Capture{Filename: filepath.Base(path), MimeType: mimeTypeForPath(path)}
	if err != nil {
		return capture
	}
	capture.FileSize = info.Size()
	// ファイルの更新日時は UTC で扱う (Python 版 datetime.fromtimestamp(mtime, tz=UTC) 相当)
	capture.FileModifiedAt = info.ModTime().UTC()

	// 画像を開いて幅・高さを取得する (デコードできない場合は 0 のまま)
	if file, err := os.Open(path); err == nil {
		if config, _, err := image.DecodeConfig(file); err == nil {
			capture.ImageWidth = config.Width
			capture.ImageHeight = config.Height
		}
		_ = file.Close()
	}

	// EXIF XPComment からメタデータを抽出する
	capture.Metadata = ExtractMetadataOnly(path)
	return capture
}

// ExtractMetadataOnly はキャプチャ画像ファイルから EXIF XPComment のメタデータのみを抽出する。
// 画像のピクセルデータはデコードしないため、検索・フィルタリング時の判定に使用できる。
func ExtractMetadataOnly(path string) *Metadata {
	info := exif.ReadFile(path)
	if info.XPComment == "" {
		return nil
	}
	var metadata Metadata
	if err := json.Unmarshal([]byte(info.XPComment), &metadata); err != nil {
		// JSON のパースに失敗した場合はメタデータなしとして扱う
		return nil
	}
	return &metadata
}

// CollectCaptureFiles はすべての保存先フォルダからキャプチャ画像ファイルを収集する。
// 同じファイル名が複数のフォルダに存在する場合は最初に見つかったものを優先する。
func CollectCaptureFiles(folders []string) []File {
	files := []File{}
	seen := map[string]bool{}
	for _, folder := range folders {
		entries, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			// キャプチャ画像の拡張子でないファイルはスキップする
			extension := strings.ToLower(filepath.Ext(entry.Name()))
			if !containsExtension(extension) {
				continue
			}
			// 同じファイル名が既に登録済みならスキップする (最初に見つかったフォルダを優先)
			if seen[entry.Name()] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			seen[entry.Name()] = true
			files = append(files, File{
				Filename:   entry.Name(),
				ModifiedAt: float64(info.ModTime().UnixNano()) / 1e9,
				Path:       filepath.Join(folder, entry.Name()),
			})
		}
	}
	return files
}

// GenerateThumbnail は画像を長辺 ThumbnailMaxSize に縮小した JPEG 画像を生成する。
// 移植元: Python 版 CaptureImageAPI() の thumbnail=true の処理 (ImageOps.exif_transpose + thumbnail + JPEG quality=80)
func GenerateThumbnail(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open the image: %w", err)
	}
	defer func() { _ = file.Close() }()

	img, _, err := image.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the image: %w", err)
	}

	// EXIF の回転情報を適用する
	img = applyOrientation(img, exif.ReadFile(path).Orientation)

	// 長辺を ThumbnailMaxSize に縮小する
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	newWidth, newHeight := thumbnailSize(width, height, ThumbnailMaxSize)
	if newWidth != width || newHeight != height {
		// LANCZOS 相当の高品質な縮小を行う (CatmullRom は LANCZOS に近い品質)
		resized := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
		draw.CatmullRom.Scale(resized, resized.Bounds(), img, bounds, draw.Over, nil)
		img = resized
	}

	// JPEG としてバイトデータに変換する
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("failed to encode the image: %w", err)
	}
	return buffer.Bytes(), nil
}

// thumbnailSize は縮小後のサイズを求める。
// 移植元: Pillow の Image.thumbnail() (縮小後の縦横比が元の画像の縦横比に最も近くなる整数を選ぶ) 。
func thumbnailSize(width int, height int, maxSize int) (int, int) {
	// 指定されたサイズは Pillow と同じく小数点以下を切り捨てる
	x, y := maxSize, maxSize
	if x >= width && y >= height {
		return width, height
	}
	aspect := float64(width) / float64(height)
	roundAspect := func(number float64, key func(int) float64) int {
		floorValue := int(math.Floor(number))
		ceilValue := int(math.Ceil(number))
		result := floorValue
		if key(ceilValue) < key(floorValue) {
			result = ceilValue
		}
		if result < 1 {
			result = 1
		}
		return result
	}
	if float64(x)/float64(y) >= aspect {
		x = roundAspect(float64(y)*aspect, func(n int) float64 {
			return math.Abs(aspect - float64(n)/float64(y))
		})
	} else {
		y = roundAspect(float64(x)/aspect, func(n int) float64 {
			if n == 0 {
				return 0
			}
			return math.Abs(aspect - float64(x)/float64(n))
		})
	}
	return x, y
}

// MimeTypeForPath は拡張子から MIME タイプを判定する。
func mimeTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "image/png"
	}
}

// MimeTypeForFilename はファイル名から MIME タイプを判定する (外部から参照するための公開版) 。
func MimeTypeForFilename(filename string) string {
	return mimeTypeForPath(filename)
}

// containsExtension は拡張子がキャプチャ画像のものであるかを返す。
func containsExtension(extension string) bool {
	for _, candidate := range captureExtensions {
		if extension == candidate {
			return true
		}
	}
	return false
}

// applyOrientation は EXIF の回転情報に従って画像を変換する。
// 移植元: Pillow の ImageOps.exif_transpose()
func applyOrientation(img image.Image, orientation exif.Orientation) image.Image {
	switch orientation {
	case 2:
		return flipHorizontal(img)
	case 3:
		return rotate180(img)
	case 4:
		return flipVertical(img)
	case 5:
		return rotate90(flipHorizontal(img))
	case 6:
		return rotate90(img)
	case 7:
		return rotate270(flipHorizontal(img))
	case 8:
		return rotate270(img)
	default:
		return img
	}
}

// flipHorizontal は画像を左右反転する。
func flipHorizontal(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			result.Set(width-1-x, y, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return result
}

// flipVertical は画像を上下反転する。
func flipVertical(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			result.Set(x, height-1-y, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return result
}

// rotate90 は画像を時計回りに 90 度回転する。
func rotate90(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	result := image.NewRGBA(image.Rect(0, 0, height, width))
	for y := range height {
		for x := range width {
			result.Set(height-1-y, x, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return result
}

// rotate180 は画像を 180 度回転する。
func rotate180(img image.Image) image.Image {
	return flipVertical(flipHorizontal(img))
}

// rotate270 は画像を反時計回りに 90 度回転する。
func rotate270(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	result := image.NewRGBA(image.Rect(0, 0, height, width))
	for y := range height {
		for x := range width {
			result.Set(y, width-1-x, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return result
}
