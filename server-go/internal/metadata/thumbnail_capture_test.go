package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/image/webp"
)

func TestThumbnailCapturePreservesRGBAndPreviousOutput(t *testing.T) {
	img := image.NewNRGBA(image.Rect(10, 20, 12, 22))
	for y := 20; y < 22; y++ {
		for x := 10; x < 12; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 30, B: 10, A: 0})
		}
	}
	output := filepath.Join(t.TempDir(), "representative.webp")
	if err := os.WriteFile(output, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	renderer := &FFMpegThumbnailRenderer{Logger: testLogger(t), RunCommand: func(ctx context.Context, name string, args, env []string) error {
		for index, arg := range args {
			if arg == "-i" {
				file, err := os.Open(args[index+1])
				if err != nil {
					return err
				}
				defer file.Close()
				decoded, err := png.Decode(file)
				if err != nil {
					return err
				}
				red, green, blue, alpha := decoded.At(0, 0).RGBA()
				if red != 200*257 || green != 30*257 || blue != 10*257 || alpha != 65535 {
					return fmt.Errorf("RGB or transparency changed")
				}
			}
		}
		return fmt.Errorf("intentional encoder error")
	}}
	if err := renderer.SaveRepresentativeFromImage(context.Background(), img, output); err == nil || !strings.Contains(err.Error(), "intentional encoder error") {
		t.Fatalf("unexpected encoding result: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "previous" {
		t.Fatalf("previous output=%q %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left: %v %v", entries, err)
	}
}

func TestThumbnailCaptureWithRealFFmpeg(t *testing.T) {
	ffmpeg := os.Getenv("KONOMITV_TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("KONOMITV_TEST_FFMPEG is not set")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 220, G: 40, B: 20, A: 255})
		}
	}
	output := filepath.Join(t.TempDir(), "representative.webp")
	renderer := &FFMpegThumbnailRenderer{FFmpegPath: ffmpeg, LibraryPath: filepath.Dir(ffmpeg), Logger: testLogger(t)}
	if err := renderer.SaveRepresentativeFromImage(context.Background(), img, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != ScoringWidth || decoded.Bounds().Dy() != ScoringHeight {
		t.Fatalf("bounds=%v", decoded.Bounds())
	}
	r, g, b, _ := decoded.At(240, 135).RGBA()
	if r < 180*257 || g > 70*257 || b > 50*257 {
		t.Fatalf("unexpected image color %d,%d,%d", r, g, b)
	}
}

func TestThumbnailInfoRevisionAndChangedHashGuard(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	ctx := context.Background()
	program := buildTestProgram("/recorded/source.ts", 3600)
	if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
		t.Fatal(err)
	}
	copy := buildTestProgram("/recorded/copy.ts", 3600)
	if _, err := store.SaveRecordedMetadata(ctx, copy, 0); err != nil {
		t.Fatal(err)
	}
	info := BuildThumbnailInfo(CalculateThumbnailLayout(3600))
	const updates = 10
	var workers sync.WaitGroup
	for range updates {
		workers.Go(func() {
			if err := store.UpdateThumbnailInfo(ctx, program.Video.FilePath, program.Video.FileHash, info); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	check := func(path string, want int) {
		t.Helper()
		var raw string
		if err := store.DB.QueryRow(`SELECT thumbnail_info FROM recorded_videos WHERE file_path=?`, path).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var got ThumbnailInfo
		if err := json.Unmarshal([]byte(raw), &got); err != nil || got.Representative.Revision != want {
			t.Fatalf("info=%s err=%v", raw, err)
		}
	}
	check(program.Video.FilePath, updates)
	check(copy.Video.FilePath, updates)
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET file_hash='changed' WHERE file_path=?`, program.Video.FilePath); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateThumbnailInfo(ctx, program.Video.FilePath, program.Video.FileHash, info); err != nil {
		t.Fatal(err)
	}
	check(program.Video.FilePath, updates)
	check(copy.Video.FilePath, updates)
}
