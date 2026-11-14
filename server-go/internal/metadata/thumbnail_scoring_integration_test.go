package metadata

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type thumbnailParamsRecorder struct{ params ThumbnailRenderParams }

func (r *thumbnailParamsRecorder) Render(_ context.Context, p ThumbnailRenderParams) error {
	r.params = p
	return nil
}

func TestThumbnailGeneratorUsesStoredProgramMetadata(t *testing.T) {
	store := openTestDB(t)
	program := buildTestProgram(filepath.Join(t.TempDir(), "recording.ts"), 3600)
	program.RecordingStartMargin, program.RecordingEndMargin = 60, 30
	program.Genres = []Genre{{Major: "アニメ・特撮", Middle: "国内アニメ"}}
	program.Video.HasVideoStreamChanges = true
	if _, err := store.SaveRecordedMetadata(context.Background(), program, 0); err != nil {
		t.Fatal(err)
	}
	renderer := &thumbnailParamsRecorder{}
	g := ThumbnailGenerator{Store: store, ThumbnailsDir: t.TempDir(), Renderer: renderer}
	if err := g.GenerateAndSave(context.Background(), program.Video.FilePath, 3600); err != nil {
		t.Fatal(err)
	}
	if renderer.params.FaceDetectionMode != "Anime" || !renderer.params.HasVideoStreamChanges || renderer.params.ContainerFormat != "MPEG-TS" || !reflect.DeepEqual(renderer.params.CandidateTimeRanges, CandidateTimeRanges(3600, 60, 30)) {
		t.Fatalf("unexpected render params: %+v", renderer.params)
	}
	if err := g.generateAndSave(context.Background(), program.Video.FilePath, "different-hash", 3600); err == nil {
		t.Fatal("stale content hash accepted")
	}
}

// TestThumbnailRendererSelectsScoredFrame は採点・顔優先の結果が実際のエンコード入力になることを確認する。
func TestThumbnailRendererSelectsScoredFrame(t *testing.T) {
	for _, mode := range []string{"", "Human"} {
		t.Run(mode, func(t *testing.T) {
			params := testThumbnailRenderParams(t.TempDir())
			params.CandidateOffsets = []float64{0, 5, 10}
			params.CandidateTimeRanges = [][2]float64{{0, 10}}
			params.FaceDetectionMode = mode
			runner := newRecordingRunner()
			selected := ""
			runner.onCall = func(index int, _ string, args, env []string) error {
				output := args[len(args)-1]
				if strings.HasSuffix(output, ".png") {
					data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_scoring", []string{"solid-127", "noise", "face-100"}[index]+".png"))
					if err != nil {
						return err
					}
					return os.WriteFile(output, data, 0600)
				}
				if output == params.RepresentativePath {
					selected = filepath.Base(argAfter(args, "-i"))
				}
				return writeFakeThumbnailOutput(output)
			}
			if err := (&FFMpegThumbnailRenderer{RunCommand: runner.run, Logger: testLogger(t)}).Render(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "Human" {
				want = 3
			}
			if selected != frameFileName(want) {
				t.Fatalf("selected=%s want=%s", selected, frameFileName(want))
			}
		})
	}
}

func TestThumbnailRendererScoresRealDecodedFrames(t *testing.T) {
	ffmpeg := os.Getenv("KONOMITV_TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("KONOMITV_TEST_FFMPEG is not set")
	}
	directory := t.TempDir()
	for i, name := range []string{"solid-127", "noise", "face-160"} {
		data, err := os.ReadFile(filepath.Join("testdata", "thumbnail_scoring", name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "input_"+strconv.Itoa(i+1)+".png"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(directory, "source.ts")
	if err := ExecCommandRunner(context.Background(), ffmpeg, []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-framerate", "1/3", "-i", filepath.Join(directory, "input_%d.png"), "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-r", "15", "-g", "15", "-pix_fmt", "yuv420p", "-f", "mpegts", source}, nil); err != nil {
		t.Fatal(err)
	}
	params := testThumbnailRenderParams(directory)
	params.FilePath = source
	params.DurationSec = 9
	params.Layout.TileIntervalSec = 3
	params.CandidateOffsets = []float64{0, 3, 6}
	params.CandidateTimeRanges = [][2]float64{{0, 8}}
	params.FaceDetectionMode = "Human"
	selected := ""
	renderer := FFMpegThumbnailRenderer{FFmpegPath: ffmpeg, Logger: testLogger(t), RunCommand: func(ctx context.Context, name string, args, env []string) error {
		if args[len(args)-1] == params.RepresentativePath {
			selected = filepath.Base(argAfter(args, "-i"))
		}
		return ExecCommandRunner(ctx, name, args, env)
	}}
	if err := renderer.Render(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if selected != "frame_00003.png" {
		t.Fatalf("selected=%s want face frame", selected)
	}
	assertWebPFile(t, params.RepresentativePath, ScoringWidth, ScoringHeight)
	assertWebPFile(t, params.TilePath, params.Layout.TileImageWidth, params.Layout.TileImageHeight)
}
