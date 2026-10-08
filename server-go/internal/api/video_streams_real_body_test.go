package api

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// TestPG4BodyRealFFmpegHTTP は録画を使わず lavfi 合成入力を実 FFmpeg で保存する。
func TestPG4BodyRealFFmpegHTTP(t *testing.T) {
	ffmpeg := os.Getenv("PG4_FFMPEG")
	tsreadex := os.Getenv("PG4_TSREADEX")
	if ffmpeg == "" || tsreadex == "" {
		t.Skip("PG4_FFMPEG and PG4_TSREADEX are required for real synthetic encoding")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "synthetic.ts")
	command := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30000/1001", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "13", "-c:v", "libx264", "-preset", "ultrafast", "-g", "90", "-c:a", "aac", "-mpegts_service_id", "1024", "-f", "mpegts", input)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic fixture: %v %s", err, out)
	}
	s, id := setupVideoStreamTest(t)
	s.config.General.Encoder = "FFmpeg"
	if _, err := s.db.Exec("UPDATE recorded_videos SET file_path=?,duration=13,video_codec='H.264',video_scan_type='Progressive',video_resolution_width=320,video_resolution_height=180 WHERE recorded_program_id=?", input, id); err != nil {
		t.Fatal(err)
	}
	// 本番と同じ配線: 通常視聴のセッションエンコーダーを登録すると保存もそれを使う。
	s.SetVideoSegmentEncoderFactory(videostream.NewSessionSegmentEncoderFactory(videostream.EncodingTaskOptions{LibraryPath: func(name string) string {
		if name == "tsreadex" {
			return tsreadex
		}
		return ffmpeg
	}}))
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Get(fmt.Sprintf("%s/api/streams/video/%d/240p/offline-stream", h.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !bytes.HasPrefix(body, []byte("KTVODLP\n")) {
		t.Fatalf("HTTP=%d body=%q", response.StatusCode, body)
	}
	reader := bytes.NewReader(body[8:])
	var length uint32
	binary.Read(reader, binary.BigEndian, &length)
	metadata := make([]byte, length)
	io.ReadFull(reader, metadata)
	count := uint32(0)
	for {
		var sequence uint32
		if err := binary.Read(reader, binary.BigEndian, &sequence); err != nil {
			t.Fatalf("missing terminal: %v", err)
		}
		if sequence == 0xffffffff {
			var terminalCount uint32
			binary.Read(reader, binary.BigEndian, &terminalCount)
			if terminalCount != count || count != 3 || reader.Len() != 0 {
				t.Fatalf("count=%d terminal=%d remain=%d", count, terminalCount, reader.Len())
			}
			break
		}
		var header [2]uint32
		if err := binary.Read(reader, binary.BigEndian, &header); err != nil {
			t.Fatal(err)
		}
		if sequence != count || header[0] == 0 || header[1] == 0 || header[1] > 128<<20 {
			t.Fatalf("header %d %v", sequence, header)
		}
		data := make([]byte, header[1])
		if _, err := io.ReadFull(reader, data); err != nil {
			t.Fatal(err)
		}
		segment := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", count))
		if err := os.WriteFile(segment, data, 0600); err != nil {
			t.Fatal(err)
		}
		decode := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", segment, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-")
		if out, err := decode.CombinedOutput(); err != nil {
			t.Fatalf("segment %d not decodable: %v %s", count, err, out)
		}
		count++
	}
	if s.videoStreams.Count() != 0 {
		t.Fatal("offline created shared HLS session")
	}
}
