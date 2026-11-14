package metadata

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestThumbnailWorkerNativeProcess(t *testing.T) {
	worker, libraries := os.Getenv("KONOMITV_TEST_THUMBNAIL_WORKER"), os.Getenv("KONOMITV_TEST_FFMPEG_LIBRARIES")
	if worker == "" || libraries == "" {
		t.Skip("native thumbnail worker and libraries are not set")
	}
	renderer := FFMpegThumbnailRenderer{FrameWorkerPath: worker, FrameLibraryPath: libraries, Logger: testLogger(t)}
	directory := t.TempDir()
	params := ThumbnailRenderParams{FilePath: filepath.Join("testdata", "thumbnail_tsreadex_input.mpegts"), ContainerFormat: "MPEG-TS"}
	if err := renderer.extractWorkerFrames(context.Background(), params, []float64{0, 6}, directory); err != nil {
		t.Fatal(err)
	}
	for index, red := range []uint32{253 * 257, 0} {
		file, err := os.Open(filepath.Join(directory, frameFileName(index+1)))
		if err != nil {
			t.Fatal(err)
		}
		frame, err := png.Decode(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, a := frame.At(0, 0).RGBA()
		if r != red || g != 0 || b != 0 || a != 65535 || frame.Bounds().Dx() != 480 || frame.Bounds().Dy() != 270 {
			t.Fatalf("frame %d bounds=%v RGBA=%v", index, frame.Bounds(), []uint32{r, g, b, a})
		}
	}
	params.FilePath = filepath.Join(directory, "missing.ts")
	if err := renderer.extractWorkerFrames(context.Background(), params, []float64{0}, directory); err == nil {
		t.Fatal("missing input succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := renderer.extractWorkerFrames(ctx, params, []float64{0}, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}
