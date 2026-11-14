package frameextract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type mediaReference struct {
	Offsets []float64 `json:"offsets"`
	Hashes  []string  `json:"rgb_sha256"`
}
type referenceFixture struct {
	Media  mediaReference `json:"media"`
	MP4    mediaReference `json:"mp4"`
	Resize []struct {
		Width, Height    int
		OutputWidth      int `json:"output_width"`
		OutputHeight     int `json:"output_height"`
		Pixels, Expected []byte
	} `json:"resize"`
}

func readReference(t *testing.T) referenceFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/seek_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture referenceFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestResizeMatchesOpenCVReference(t *testing.T) {
	for _, tc := range readReference(t).Resize {
		got, err := ResizeArea(&RGB{Width: tc.Width, Height: tc.Height, Pixels: tc.Pixels}, tc.OutputWidth, tc.OutputHeight)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Pixels, tc.Expected) {
			diff, maxDiff := 0, 0
			for i, v := range got.Pixels {
				if v != tc.Expected[i] {
					diff++
					maxDiff = max(maxDiff, abs(int(v)-int(tc.Expected[i])))
				}
			}
			t.Errorf("%dx%d -> %dx%d: %d different bytes, max difference=%d", tc.Width, tc.Height, tc.OutputWidth, tc.OutputHeight, diff, maxDiff)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestNativeExtractionMatchesPinnedSource(t *testing.T) {
	directory := os.Getenv("KONOMITV_TEST_FFMPEG_LIBRARIES")
	if directory == "" {
		t.Skip("KONOMITV_TEST_FFMPEG_LIBRARIES is not set")
	}
	library, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	fixture := readReference(t)
	for _, tc := range []struct {
		path, format string
		reference    mediaReference
		black        int
	}{
		{filepath.Join("..", "metadata", "testdata", "thumbnail_tsreadex_input.mpegts"), "MPEG-TS", fixture.Media, 4},
		{filepath.Join("testdata", "seek_input.mp4"), "MPEG-4", fixture.MP4, 0},
	} {
		t.Run(tc.format, func(t *testing.T) {
			result, err := library.Extract(context.Background(), Request{FilePath: tc.path, ContainerFormat: tc.format, Offsets: tc.reference.Offsets}, func(index int, frame *RGB) error {
				got := fmt.Sprintf("%x", sha256.Sum256(frame.Pixels))
				if got != tc.reference.Hashes[index] {
					t.Errorf("frame %d pts=%d hash=%s want=%s", index, frame.PTS, got, tc.reference.Hashes[index])
				}
				return nil
			})
			if err != nil || result.Frames != len(tc.reference.Offsets) || result.BlackFrames != tc.black {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}
