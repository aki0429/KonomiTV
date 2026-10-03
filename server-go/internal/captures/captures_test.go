package captures

import (
	"bytes"
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/exif"
)

// expectations はフィクスチャの期待値 (tools/generate_capture_fixture.py が生成する) 。
type expectation struct {
	MimeType        string    `json:"mime_type"`
	ImageWidth      int       `json:"image_width"`
	ImageHeight     int       `json:"image_height"`
	Orientation     int       `json:"orientation"`
	CaptureMetadata *Metadata `json:"capture_metadata"`
}

// loadExpectations は期待値を読み込む。
func loadExpectations(t *testing.T) map[string]expectation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]expectation{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestExtractCaptureInfo はキャプチャ画像のメタデータ抽出が Python 版と一致することを検証する。
func TestExtractCaptureInfo(t *testing.T) {
	expectations := loadExpectations(t)
	for filename, expected := range expectations {
		path := filepath.Join("testdata", filename)
		capture := ExtractCaptureInfo(path)
		if capture.Filename != filename {
			t.Errorf("%s: Filename = %q", filename, capture.Filename)
		}
		if capture.MimeType != expected.MimeType {
			t.Errorf("%s: MimeType = %q, want %q", filename, capture.MimeType, expected.MimeType)
		}
		if capture.ImageWidth != expected.ImageWidth || capture.ImageHeight != expected.ImageHeight {
			t.Errorf(
				"%s: size = %dx%d, want %dx%d",
				filename, capture.ImageWidth, capture.ImageHeight, expected.ImageWidth, expected.ImageHeight,
			)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if capture.FileSize != info.Size() {
			t.Errorf("%s: FileSize = %d, want %d", filename, capture.FileSize, info.Size())
		}
		if !capture.FileModifiedAt.Equal(info.ModTime().UTC()) {
			t.Errorf("%s: FileModifiedAt = %v, want %v", filename, capture.FileModifiedAt, info.ModTime().UTC())
		}
		if expected.CaptureMetadata == nil {
			if capture.Metadata != nil {
				t.Errorf("%s: Metadata = %+v, want nil", filename, capture.Metadata)
			}
			continue
		}
		if capture.Metadata == nil {
			t.Fatalf("%s: Metadata = nil, want %+v", filename, expected.CaptureMetadata)
		}
		actual, err := json.Marshal(capture.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		expectedJSON, err := json.Marshal(expected.CaptureMetadata)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(expectedJSON) {
			t.Errorf("%s: Metadata = %s, want %s", filename, actual, expectedJSON)
		}
	}
}

// TestExtractMetadataOnly は EXIF メタデータのみの抽出を検証する。
func TestExtractMetadataOnly(t *testing.T) {
	expectations := loadExpectations(t)
	for filename, expected := range expectations {
		path := filepath.Join("testdata", filename)
		metadata := ExtractMetadataOnly(path)
		if expected.CaptureMetadata == nil {
			if metadata != nil {
				t.Errorf("%s: Metadata = %+v, want nil", filename, metadata)
			}
			continue
		}
		if metadata == nil {
			t.Fatalf("%s: Metadata = nil", filename)
		}
		if metadata.Title != expected.CaptureMetadata.Title {
			t.Errorf("%s: Title = %q, want %q", filename, metadata.Title, expected.CaptureMetadata.Title)
		}
		if metadata.NetworkID != expected.CaptureMetadata.NetworkID || metadata.ServiceID != expected.CaptureMetadata.ServiceID {
			t.Errorf("%s: NID/SID = %d/%d", filename, metadata.NetworkID, metadata.ServiceID)
		}
		if metadata.CapturedPlaybackPosition != expected.CaptureMetadata.CapturedPlaybackPosition {
			t.Errorf("%s: CapturedPlaybackPosition = %v", filename, metadata.CapturedPlaybackPosition)
		}
		if metadata.CaptionText == nil || expected.CaptureMetadata.CaptionText == nil || *metadata.CaptionText != *expected.CaptureMetadata.CaptionText {
			t.Errorf("%s: CaptionText = %v", filename, metadata.CaptionText)
		}
		if metadata.IsCaptionComposited != expected.CaptureMetadata.IsCaptionComposited {
			t.Errorf("%s: IsCaptionComposited = %v", filename, metadata.IsCaptionComposited)
		}
		if metadata.IsCommentComposited != expected.CaptureMetadata.IsCommentComposited {
			t.Errorf("%s: IsCommentComposited = %v", filename, metadata.IsCommentComposited)
		}
	}
}

// TestReadOrientation は EXIF の回転情報の読み取りを検証する。
func TestReadOrientation(t *testing.T) {
	expectations := loadExpectations(t)
	for filename, expected := range expectations {
		info := exif.ReadFile(filepath.Join("testdata", filename))
		if int(info.Orientation) != expected.Orientation {
			t.Errorf("%s: Orientation = %d, want %d", filename, info.Orientation, expected.Orientation)
		}
	}
}

// TestGenerateThumbnail はサムネイル生成 (縮小 + 回転 + JPEG 変換) を検証する。
func TestGenerateThumbnail(t *testing.T) {
	// Orientation=6 (90 度回転) の JPEG は、回転後の長辺が 1200 になる
	data, err := GenerateThumbnail(filepath.Join("testdata", "capture_exif.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(newBytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg", format)
	}
	// 元画像は 1200x800 で Orientation=6 のため、回転後は 800x1200 になり長辺が 400 に縮小される
	// (Pillow の thumbnail() と同じく 267x400 になる)
	if config.Width != 267 || config.Height != 400 {
		t.Errorf("size = %dx%d, want 267x400", config.Width, config.Height)
	}

	// EXIF なしの PNG は長辺 400 に縮小される
	data, err = GenerateThumbnail(filepath.Join("testdata", "capture_no_exif.png"))
	if err != nil {
		t.Fatal(err)
	}
	config, _, err = image.DecodeConfig(newBytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 400 || config.Height != 300 {
		t.Errorf("size = %dx%d, want 400x300", config.Width, config.Height)
	}
}

// TestFindFile はファイル検索とディレクトリトラバーサル対策を検証する。
func TestFindFile(t *testing.T) {
	folder := t.TempDir()
	writeTestFile(t, filepath.Join(folder, "capture.jpg"), "jpeg")

	folders := []string{folder, filepath.Join(folder, "not-exists")}
	if path := FindFile(folders, "capture.jpg"); path != filepath.Join(folder, "capture.jpg") {
		t.Errorf("FindFile = %q", path)
	}
	if path := FindFile(folders, "not-found.jpg"); path != "" {
		t.Errorf("FindFile = %q, want empty", path)
	}
	// ディレクトリトラバーサルは拒否する
	for _, filename := range []string{"../capture.jpg", "..\\capture.jpg", "sub/capture.jpg", ".", "..", "C:capture.jpg"} {
		if path := FindFile(folders, filename); path != "" {
			t.Errorf("FindFile(%q) = %q, want empty", filename, path)
		}
	}
}

// TestCollectCaptureFiles は保存先フォルダのスキャンを検証する。
func TestCollectCaptureFiles(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeTestFile(t, filepath.Join(first, "a.jpg"), "jpeg")
	writeTestFile(t, filepath.Join(first, "b.PNG"), "png")
	writeTestFile(t, filepath.Join(first, "c.txt"), "text")
	writeTestFile(t, filepath.Join(second, "a.jpg"), "jpeg")
	writeTestFile(t, filepath.Join(second, "d.jpeg"), "jpeg")
	if err := os.Mkdir(filepath.Join(first, "subdir.jpg"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := CollectCaptureFiles([]string{first, second})
	if len(files) != 3 {
		t.Fatalf("len(files) = %d, want 3: %+v", len(files), files)
	}
	// 同じファイル名は最初に見つかったフォルダを優先する
	if files[0].Filename != "a.jpg" || filepath.Dir(files[0].Path) != first {
		t.Errorf("files[0] = %+v", files[0])
	}
	names := map[string]bool{}
	for _, file := range files {
		names[file.Filename] = true
	}
	if !names["b.PNG"] || !names["d.jpeg"] {
		t.Errorf("names = %v", names)
	}
}

// TestUploadFolders は存在するフォルダのみが返ることを検証する。
func TestUploadFolders(t *testing.T) {
	existing := t.TempDir()
	folders := UploadFolders([]string{existing, filepath.Join(existing, "not-exists")})
	if len(folders) != 1 || folders[0] != existing {
		t.Errorf("folders = %v", folders)
	}
}

// TestMimeTypeForFilename は拡張子からの MIME タイプ判定を検証する。
func TestMimeTypeForFilename(t *testing.T) {
	cases := map[string]string{
		"capture.jpg":  "image/jpeg",
		"capture.JPEG": "image/jpeg",
		"capture.png":  "image/png",
		"capture.PNG":  "image/png",
	}
	for filename, expected := range cases {
		if actual := MimeTypeForFilename(filename); actual != expected {
			t.Errorf("MimeTypeForFilename(%q) = %q, want %q", filename, actual, expected)
		}
	}
}

// writeTestFile はテスト用のファイルを作成する。
func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newBytesReader はバイト列を読み取る io.Reader を返す。
func newBytesReader(data []byte) *bytes.Reader {
	return bytes.NewReader(data)
}
