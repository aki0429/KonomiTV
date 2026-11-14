package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// catalogJPEG は本家 CaptureCompositor と同じ BYTE 型の XPComment を合成する。
func catalogJPEG(t *testing.T, metadata any, shade byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: 100, B: 200, A: 255})
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, img, nil); err != nil {
		t.Fatal(err)
	}
	if metadata == nil {
		return output.Bytes()
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	units := utf16.Encode([]rune(string(raw)))
	comment := make([]byte, len(units)*2)
	for i, value := range units {
		binary.LittleEndian.PutUint16(comment[i*2:], value)
	}
	tiff := make([]byte, 26+len(comment))
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], 8)
	binary.LittleEndian.PutUint16(tiff[8:], 1)
	binary.LittleEndian.PutUint16(tiff[10:], 40092)
	binary.LittleEndian.PutUint16(tiff[12:], 1)
	binary.LittleEndian.PutUint32(tiff[14:], uint32(len(comment)))
	binary.LittleEndian.PutUint32(tiff[18:], 26)
	copy(tiff[26:], comment)
	segment := append([]byte("Exif\x00\x00"), tiff...)
	header := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(segment)+2))
	result := append([]byte{}, output.Bytes()[:2]...)
	result = append(result, header...)
	result = append(result, segment...)
	return append(result, output.Bytes()[2:]...)
}

func catalogMetadataFixture() map[string]any {
	return map[string]any{"captured_at": "2026-11-14T10:00:10+09:00", "captured_playback_position": 10.0, "network_id": 4, "service_id": 101, "event_id": 123, "start_time": "2026-11-14T10:00:00+09:00", "end_time": "2026-11-14T11:00:00+09:00", "duration": 3600.0, "title": "合成番組", "description": "テスト", "is_caption_composited": true}
}

func newCatalogTest(t *testing.T) (*captureCatalog, *Server, string, *time.Time) {
	t.Helper()
	s, _ := newTestServer(t, "")
	s.writeDB.SetMaxOpenConns(1)
	if _, err := s.writeDB.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := database.EnsureSchema(context.Background(), s.writeDB); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, constants.JST)
	catalog := &captureCatalog{db: s.db, writeDB: s.writeDB, logger: s.logger, folders: []string{folder}, now: func() time.Time { return now }}
	s.captureCatalog = catalog
	s.config.Capture.UploadFolders = []string{folder}
	return catalog, s, folder, &now
}

func TestCatalogImageMetadata(t *testing.T) {
	modified := time.Date(2026, 11, 14, 15, 0, 0, 0, constants.JST)
	for _, test := range []struct {
		name   string
		edit   func(map[string]any)
		status string
	}{
		{"current", func(map[string]any) {}, "Parsed"},
		{"legacy", func(m map[string]any) {
			m["capture_time"] = m["captured_at"]
			delete(m, "captured_at")
			delete(m, "captured_playback_position")
		}, "Parsed"},
		{"unknown duration", func(m map[string]any) { m["duration"] = nil }, "Parsed"},
		{"unknown ids", func(m map[string]any) { m["network_id"] = -1 }, "Parsed"},
		{"range", func(m map[string]any) { m["network_id"] = 65536 }, "Invalid"},
		{"nonfinite", func(m map[string]any) { m["captured_playback_position"] = "NaN" }, "Invalid"},
		{"datetime overflow", func(m map[string]any) { m["captured_playback_position"] = 1e12 }, "Invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			meta := catalogMetadataFixture()
			test.edit(meta)
			info, err := readCatalogImage(catalogJPEG(t, meta, 10), "sample.jpg", modified)
			if err != nil || info.MetadataStatus != test.status {
				t.Fatalf("status=%+v err=%v", info, err)
			}
			if test.status == "Parsed" && info.Metadata.PlaybackPosition != 10 {
				t.Fatalf("position=%v", info.Metadata.PlaybackPosition)
			}
			if test.name == "unknown duration" && info.Metadata.EndTime != nil {
				t.Fatal("undecided program retained an end time")
			}
		})
	}
	data := catalogJPEG(t, nil, 10)
	info, err := readCatalogImage(data, "Capture_20261113-100000.jpg", modified)
	if err != nil || info.MetadataStatus != "NoMetadata" || database.FormatJSONTime(info.CapturedAt) != "2026-11-13T10:00:00+09:00" {
		t.Fatalf("filename fallback=%+v %v", info, err)
	}
	info, err = readCatalogImage(data, "Capture_20261114-030000.jpg", modified)
	if err != nil || !info.CapturedAt.Equal(modified) {
		t.Fatal("12-hour filename notation replaced modification time")
	}
	if _, err := readCatalogImage(data[:len(data)/2], "broken.jpg", modified); err == nil {
		t.Fatal("truncated JPEG was accepted")
	}
}

func TestCatalogRegisterMoveOverwriteAndDelete(t *testing.T) {
	c, s, folder, _ := newCatalogTest(t)
	ctx := context.Background()
	data := catalogJPEG(t, catalogMetadataFixture(), 10)
	path := filepath.Join(folder, "one.jpg")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := c.register(ctx, path, nil, false)
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	copyPath := filepath.Join(folder, "copy.jpg")
	if err := os.WriteFile(copyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	copyID, err := c.register(ctx, copyPath, nil, false)
	if err != nil || copyID != id {
		t.Fatal("same content was not deduplicated")
	}
	user := createTestUser(t, s.db, "viewer", "password", false)
	if _, err := s.writeDB.Exec(`INSERT INTO capture_favorites(user_id,capture_id)VALUES(?,?)`, user, id); err != nil {
		t.Fatal(err)
	}
	movedDir := filepath.Join(folder, "moved")
	if err := os.Mkdir(movedDir, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(movedDir, "one.jpg")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if complete, err := c.runBatch(ctx); err != nil || !complete {
		t.Fatalf("scan=%v %v", complete, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM captures`).Scan(&count); err != nil || count != 1 {
		t.Fatal("move lost capture identity")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_files WHERE capture_id=?`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("move left a stale file: %d %v", count, err)
	}
	imageData, err := c.image(ctx, id, fmt.Sprintf("%x", sha256ForTest(data)))
	if err != nil || !bytes.Equal(imageData, data) {
		t.Fatalf("image mismatch: %v", err)
	}
	// 同じ場所に別画像を置いた場合、古いキャプチャを削除する操作で新画像を消してはならない。
	replaced := catalogJPEG(t, catalogMetadataFixture(), 80)
	if err := os.WriteFile(copyPath, replaced, 0600); err != nil {
		t.Fatal(err)
	}
	ok, err := c.delete(ctx, id)
	if err != nil || ok {
		t.Fatalf("replaced file deletion=%v %v", ok, err)
	}
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatal("replacement image was deleted")
	}
	if ok, err := c.delete(ctx, id); err != nil || !ok {
		t.Fatalf("delete=%v %v", ok, err)
	}
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatal("replacement image was deleted with the original")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_favorites WHERE capture_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("favorite did not cascade")
	}
}

func sha256ForTest(data []byte) []byte { sum := sha256.Sum256(data); return sum[:] }

func TestCatalogMissingRetentionAndUnreadableRoots(t *testing.T) {
	c, s, folder, now := newCatalogTest(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		path := filepath.Join(folder, fmt.Sprintf("%d.jpg", i))
		if err := os.WriteFile(path, catalogJPEG(t, nil, byte(i*30)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if complete, err := c.runBatch(ctx); err != nil || !complete {
		t.Fatal(err)
	}
	missing := filepath.Join(folder, "0.jpg")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if complete, err := c.runBatch(ctx); err != nil || !complete {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM capture_files WHERE file_path=?`, missing).Scan(&status); err != nil || status != "Missing" {
		t.Fatalf("missing status=%s %v", status, err)
	}
	*now = now.Add(8 * 24 * time.Hour)
	c.folders = append(c.folders, filepath.Join(folder, "unreadable-root"))
	if complete, err := c.runBatch(ctx); err != nil || complete {
		t.Fatalf("incomplete root scan=%v %v", complete, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_files WHERE file_path=?`, missing).Scan(&count); err != nil || count != 1 {
		t.Fatal("unreadable root caused expiry deletion")
	}
	c.folders = c.folders[:1]
	if complete, err := c.runBatch(ctx); err != nil || !complete {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_files WHERE file_path=?`, missing).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired missing file was retained")
	}
	if c.scanStatus().Running {
		t.Fatal("scan remained running")
	}
}
