package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestThumbnailTSReadExWithActualPIDChanges は二つの実 MPEG-2 TS の映像 PID を変えて連結する。
// 前処理で後半の映像も復元し、本家の直前 I フレーム・末尾補完を適用できることを検査する。
func TestThumbnailTSReadExWithActualPIDChanges(t *testing.T) {
	ffmpeg, tsreadex := os.Getenv("KONOMITV_TEST_FFMPEG"), os.Getenv("KONOMITV_TEST_TSREADEX")
	if ffmpeg == "" || tsreadex == "" {
		t.Skip("KONOMITV_TEST_FFMPEG / KONOMITV_TEST_TSREADEX are not set")
	}
	directory := t.TempDir()
	input := filepath.Join(directory, "PID changing recording.ts")
	joined, err := os.ReadFile(filepath.Join("testdata", "thumbnail_tsreadex_input.mpegts"))
	if err != nil {
		t.Fatal(err)
	}
	var reference struct {
		InputSHA string `json:"input_sha256"`
		Offsets  []float64
		Colors   []string `json:"expected_colors"`
		Pixels   [][3]int `json:"pixel_bgr"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_tsreadex_media_reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	if len(reference.Offsets) != 7 || len(reference.Colors) != 7 || len(reference.Pixels) != 7 {
		t.Fatal("invalid native media reference")
	}
	hash := sha256.Sum256(joined)
	if hex.EncodeToString(hash[:]) != reference.InputSHA {
		t.Fatal("native source fixture changed")
	}
	if err := os.WriteFile(input, joined, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := 101
	params := ThumbnailRenderParams{FilePath: input, ContainerFormat: "MPEG-TS", HasVideoStreamChanges: true, ServiceID: &id}
	renderer := FFMpegThumbnailRenderer{FFmpegPath: ffmpeg, TSReadExPath: tsreadex, LibraryPath: filepath.Dir(ffmpeg), Logger: testLogger(t)}
	frames := filepath.Join(directory, "frames")
	if err := os.Mkdir(frames, 0700); err != nil {
		t.Fatal(err)
	}
	offsets := reference.Offsets
	if err := renderer.extractTSReadExFrames(ctx, params, offsets, frames); err != nil {
		t.Fatal(err)
	}
	for n := range offsets {
		file, err := os.Open(filepath.Join(frames, frameFileName(n+1)))
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != ScoringWidth || img.Bounds().Dy() != ScoringHeight {
			t.Fatalf("frame %d has dimensions %v", n, img.Bounds())
		}
		r, g, b, _ := img.At(100, 100).RGBA()
		wantBlue := reference.Colors[n] == "blue"
		if wantBlue && b <= r || !wantBlue && r <= b {
			t.Errorf("candidate %.3f sec color r=%d b=%d want blue=%v", offsets[n], r, b, wantBlue)
		}
		for c, value := range [3]int{int(b >> 8), int(g >> 8), int(r >> 8)} {
			if thumbnailAbs(value-reference.Pixels[n][c]) > 2 {
				t.Errorf("candidate %.3f channel%d=%d want=%d (PyAV / FFmpeg rounding tolerance2)", offsets[n], c, value, reference.Pixels[n][c])
			}
		}
	}
	// 共通の採点とタイル生成へも接続できる。
	renderParams := testThumbnailRenderParams(directory)
	renderParams.FilePath = input
	renderParams.ContainerFormat = "MPEG-TS"
	renderParams.HasVideoStreamChanges = true
	renderParams.ServiceID = &id
	renderParams.CandidateOffsets = []float64{0, 2, 5, 6, 8, 11}
	renderParams.CandidateTimeRanges = [][2]float64{{0, 12}}
	if err := renderer.Render(ctx, renderParams); err != nil {
		t.Fatal(err)
	}
	assertWebPFile(t, renderParams.TilePath, renderParams.Layout.TileImageWidth, renderParams.Layout.TileImageHeight)
	assertWebPFile(t, renderParams.RepresentativePath, ScoringWidth, ScoringHeight)
	t.Logf("Native source comparison: %d candidates, PID 256 -> 512, service 101", len(offsets))
}

func TestThumbnailTSReadExIsLimitedToChangedTransportStreams(t *testing.T) {
	params := testThumbnailRenderParams(t.TempDir())
	params.ContainerFormat = "MPEG-4"
	params.HasVideoStreamChanges = true
	runner := newRecordingRunner()
	renderer := FFMpegThumbnailRenderer{RunCommand: runner.run, TSReadExPath: "unavailable-tsreadex", Logger: testLogger(t)}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("MP4 incorrectly used tsreadex: %v", err)
	}
	params.ContainerFormat = "MPEG-TS"
	params.HasVideoStreamChanges = false
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatalf("ordinary TS incorrectly used tsreadex: %v", err)
	}
}
