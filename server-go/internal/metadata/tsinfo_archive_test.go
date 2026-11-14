package metadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type archiveProbeRunner struct{}

func (archiveProbeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error) {
	result, err := (recordingTimeProbeRunner{}).Run(ctx, name, args, stdin)
	if result != nil {
		result.Stdout = []byte(strings.ReplaceAll(string(result.Stdout), `"duration":"600"`, `"duration":"65"`))
	}
	return result, err
}

func TestAnalyzerMP4ArchiveAndRecentFallback(t *testing.T) {
	for _, name := range []string{"present", "no-eit", "absent"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "movie.mp4")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(4 << 20); err != nil {
				t.Fatal(err)
			}
			file.Close()
			if name != "absent" {
				data, err := os.ReadFile(filepath.Join("testdata", "psc", name+".psc"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "movie.psc"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			analyzer := Analyzer{Runner: archiveProbeRunner{}, ProgramAnalyzer: NewTSInfoProgramAnalyzer(nil, testLogger(t)), Logger: testLogger(t)}
			program, err := analyzer.Analyze(context.Background(), path)
			if err != nil || program == nil {
				t.Fatalf("recent MP4 unavailable: %v", err)
			}
			if name == "present" {
				if program.Title != "PRESENT" || program.Channel == nil || program.RecordingEndMargin != 5 || program.IsPartiallyRecorded {
					t.Fatalf("program=%+v", program)
				}
			} else if program.Title != "movie" || program.Channel != nil {
				t.Fatalf("fallback=%+v", program)
			}
			if name == "no-eit" && (program.Video.RecordingStartTime == nil || program.StartTime.Format(time.RFC3339) != "2026-10-07T10:00:00+09:00") {
				t.Fatalf("archive clock lost on fallback: %+v", program)
			}
			if name == "absent" && program.Video.RecordingStartTime != nil {
				t.Fatal("missing sidecar created recording clock")
			}
			if program.Video.FileSize != 4<<20 || program.Video.Duration != 65 || program.Video.ContainerFormat != "MPEG-4" {
				t.Fatalf("media identity changed: %+v", program.Video)
			}
		})
	}
}

func TestTSInfoArchiveMatchesPinnedSource(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name            string
			Duration        float64
			Hash            string `json:"virtual_ts_sha256"`
			RecordingStart  string `json:"recording_start"`
			RecordingEnd    string `json:"recording_end"`
			Title           *string
			Start, End      string
			ProgramDuration float64 `json:"program_duration"`
			EventID         int     `json:"event_id"`
			Channel         string
		}
	}
	data, err := os.ReadFile("testdata/psc/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", "psc", tc.Name+".psc"))
			if err != nil {
				t.Fatal(err)
			}
			archive, valid, err := tsinfoReadArchive(context.Background(), bytes.NewReader(input), tc.Duration)
			if err != nil || (!valid && tc.Name != "partial-trailer") {
				t.Fatalf("valid=%v error=%v", valid, err)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(archive.packets)); got != tc.Hash {
				t.Errorf("virtual TS hash=%s want=%s", got, tc.Hash)
			}
			root := t.TempDir()
			media := filepath.Join(root, "movie.mp4")
			if err := os.WriteFile(filepath.Join(root, "movie.psc"), input, 0600); err != nil {
				t.Fatal(err)
			}
			video := &RecordedVideo{FilePath: media, FileSize: 9999999, Duration: tc.Duration, ContainerFormat: "MPEG-4"}
			program, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgramContext(context.Background(), video, nil, nil)
			format := func(clock *time.Time) string {
				if clock == nil {
					return ""
				}
				return clock.Format(time.RFC3339Nano)
			}
			if format(video.RecordingStartTime) != tc.RecordingStart || format(video.RecordingEndTime) != tc.RecordingEnd {
				t.Fatalf("clock=%v/%v want=%s/%s", video.RecordingStartTime, video.RecordingEndTime, tc.RecordingStart, tc.RecordingEnd)
			}
			if tc.Title == nil {
				if ok || program != nil {
					t.Fatal("unavailable program must use filename fallback")
				}
				return
			}
			if !ok || program == nil {
				t.Fatal("program unavailable")
			}
			if program.Title != *tc.Title || program.StartTime.Format(time.RFC3339Nano) != tc.Start || program.EndTime.Format(time.RFC3339Nano) != tc.End || program.Duration != tc.ProgramDuration || program.Channel.ID != tc.Channel || *program.EventID != tc.EventID {
				t.Fatalf("program=%+v want=%+v", program, tc)
			}
			if program.Video.FilePath != media || program.Video.FileSize != 9999999 || program.Video.ContainerFormat != "MPEG-4" {
				t.Fatalf("virtual TS replaced real media identity: %+v", program.Video)
			}
		})
	}
}
