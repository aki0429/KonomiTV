package metadata

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadBDAVFixture(t *testing.T, name string) []byte {
	t.Helper()
	compressed, err := os.ReadFile(filepath.Join("testdata", "bdav", name+".mpegts.gz"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAACCleanupMatchesPinnedSource(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name            string
			PacketSize      int `json:"packet_size"`
			Input, Expected []byte
		}
	}
	data, err := os.ReadFile("testdata/bdav/aac_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := clearLeadingPartialAACFrames(bytes.Clone(tc.Input), tc.PacketSize)
			if !bytes.Equal(got, tc.Expected) {
				t.Fatalf("AAC cleanup differs from pinned source")
			}
		})
	}
}

type m2tsStorageFailure struct {
	io.ReaderAt
	failure error
}

func (r m2tsStorageFailure) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	return n, errors.Join(err, r.failure)
}

func TestM2TSReaderPreservesBytesOffsetsAndErrors(t *testing.T) {
	var physical, normalized []byte
	for n := range 350 {
		packet := make([]byte, 188)
		for i := range packet {
			packet[i] = byte((i + n) % 251)
		}
		packet[0] = 0x47
		physical = append(physical, 0x80, 0, 0, byte(n))
		physical = append(physical, packet...)
		normalized = append(normalized, packet...)
	}
	reader := newM2TSReader(bytes.NewReader(physical), int64(len(physical)))
	var actual []byte
	buffer := make([]byte, 37)
	for {
		n, err := reader.Read(buffer)
		actual = append(actual, buffer[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(actual, normalized) {
		t.Fatal("transport headers were not removed exactly")
	}
	if _, err := reader.Seek(187, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buffer = make([]byte, 195)
	if _, err := io.ReadFull(reader, buffer); err != nil || !bytes.Equal(buffer, normalized[187:382]) {
		t.Fatalf("cross-packet seek: %v", err)
	}
	if _, err := reader.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	reader = newM2TSReader(bytes.NewReader(physical), 7*192+100)
	data, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(data, normalized[:7*188+96]) {
		t.Fatalf("partial effective end: %v", err)
	}
	failure := errors.New("storage error")
	reader = newM2TSReader(m2tsStorageFailure{bytes.NewReader(physical), failure}, int64(len(physical)))
	if _, err := reader.Read(make([]byte, 188)); !errors.Is(err, failure) {
		t.Fatalf("storage error lost: %v", err)
	}
}

func TestBDAVMatchesPinnedSource(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name                string
			PacketSize          int `json:"packet_size"`
			Duration            float64
			RecordingStart      string `json:"recording_start"`
			RecordingEnd        string `json:"recording_end"`
			Title               *string
			Start, End, Channel string
			ChannelNumber       string   `json:"channel_number"`
			ChannelTSID         int      `json:"channel_tsid"`
			ProgramDuration     float64  `json:"program_duration"`
			EventID             int      `json:"event_id"`
			IsFree              bool     `json:"is_free"`
			PCRDuration         *float64 `json:"pcr_duration"`
			ValidEnd            int64    `json:"valid_end"`
		}
	}
	data, err := os.ReadFile("testdata/bdav/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			input := loadBDAVFixture(t, tc.Name)
			path := filepath.Join(t.TempDir(), "recording.m2ts")
			if err := os.WriteFile(path, input, 0600); err != nil {
				t.Fatal(err)
			}
			size, err := detectTSPacketSize(path)
			if err != nil || size != tc.PacketSize {
				t.Fatalf("size=%d error=%v", size, err)
			}
			duration, end, ok := calculateTSFileDuration(path, 1<<20)
			if tc.PCRDuration == nil {
				if ok {
					t.Errorf("unexpected duration=%f", duration)
				}
			} else if !ok || duration != *tc.PCRDuration || end != tc.ValidEnd {
				t.Errorf("duration=%v end=%d valid=%v want=%v end=%d", duration, end, ok, *tc.PCRDuration, tc.ValidEnd)
			}
			video := &RecordedVideo{FilePath: path, FileSize: int64(len(input)), ContainerFormat: "MPEG-TS", Duration: tc.Duration}
			program, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgramContext(context.Background(), video, nil, nil)
			format := func(clock *time.Time) string {
				if clock == nil {
					return ""
				}
				return clock.Format(time.RFC3339Nano)
			}
			if format(video.RecordingStartTime) != tc.RecordingStart || format(video.RecordingEndTime) != tc.RecordingEnd {
				t.Errorf("clock=%v..%v want=%s..%s", video.RecordingStartTime, video.RecordingEndTime, tc.RecordingStart, tc.RecordingEnd)
			}
			if tc.Title == nil {
				if ok || program != nil {
					t.Fatal("program must be unavailable")
				}
				return
			}
			if !ok || program == nil {
				t.Fatal("program unavailable")
			}
			if program.Title != *tc.Title || program.StartTime.Format(time.RFC3339Nano) != tc.Start || program.EndTime.Format(time.RFC3339Nano) != tc.End || program.Duration != tc.ProgramDuration || *program.EventID != tc.EventID || program.Channel.ID != tc.Channel || program.Channel.ChannelNumber != tc.ChannelNumber || *program.Channel.TransportStreamID != tc.ChannelTSID || program.IsFree != tc.IsFree {
				t.Fatalf("program=%+v channel=%+v want=%+v", program, program.Channel, tc)
			}
			if program.Video.FileSize != int64(len(input)) || program.Video.FilePath != path {
				t.Fatal("normalized reader changed media identity")
			}
		})
	}
}
