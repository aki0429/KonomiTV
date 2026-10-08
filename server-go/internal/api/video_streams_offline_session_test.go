package api

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// TestPG4SessionBackedOfflineStream は本番配線 (通常視聴と同じ録画セッション) で
// KTVODLP 全体を生成し、Python と同じく全セグメントを順に格納して終端件数を書き、
// 応答完了後に保存専用セッションと実行枠を解放することを検証する。
func TestPG4SessionBackedOfflineStream(t *testing.T) {
	s, id := setupVideoStreamTest(t)
	input := filepath.Join(t.TempDir(), "synthetic.ts")
	if err := os.WriteFile(input, bytes.Repeat([]byte{0x47}, 188), 0o600); err != nil {
		t.Fatal(err)
	}
	// 13 秒の録画は複数セグメントになる。入力位置は segment_map で与え、実ファイル解析を不要にする。
	segmentMap := "["
	for i := 0; i < 8; i++ {
		if i > 0 {
			segmentMap += ","
		}
		segmentMap += fmt.Sprintf(`{"sequence_index":%d,"source_file_position":0,"source_start_dts":%d}`, i, 900000+i*540540)
	}
	segmentMap += "]"
	if _, err := s.db.Exec("UPDATE recorded_videos SET file_path=?, duration=13, segment_map=? WHERE recorded_program_id=?", input, segmentMap, id); err != nil {
		t.Fatal(err)
	}
	s.SetVideoSegmentEncoderFactory(func(session *videostream.Session) videostream.SegmentEncoder {
		return &apiFakeEncoder{session: session, mode: "ok"}
	})
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(fmt.Sprintf("%s/api/streams/video/%d/720p/offline-stream", h.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !bytes.HasPrefix(body, []byte("KTVODLP\n")) {
		t.Fatalf("HTTP=%d body=%q", response.StatusCode, body)
	}
	reader := bytes.NewReader(body[8:])
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(int64(length), io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	count := uint32(0)
	totalMilliseconds := uint32(0)
	for {
		var first uint32
		if err := binary.Read(reader, binary.BigEndian, &first); err != nil {
			t.Fatalf("truncated stream after %d segments: %v", count, err)
		}
		if first == 0xffffffff {
			var terminal uint32
			if err := binary.Read(reader, binary.BigEndian, &terminal); err != nil {
				t.Fatal(err)
			}
			if terminal != count || reader.Len() != 0 {
				t.Fatalf("terminal count=%d segments=%d remaining=%d", terminal, count, reader.Len())
			}
			break
		}
		var rest [2]uint32
		if err := binary.Read(reader, binary.BigEndian, &rest); err != nil {
			t.Fatal(err)
		}
		if first != count {
			t.Fatalf("sequence %d want %d", first, count)
		}
		data := make([]byte, rest[1])
		if _, err := io.ReadFull(reader, data); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, []byte{0x47, 1, 2, 3}) {
			t.Fatalf("segment %d data %v", count, data)
		}
		totalMilliseconds += rest[0]
		count++
	}
	if count < 2 {
		t.Fatalf("13s recording produced %d segments", count)
	}
	if totalMilliseconds < 12990 || totalMilliseconds > 13010 {
		t.Fatalf("segment durations sum to %d ms, want about 13000", totalMilliseconds)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.videoStreams.Count() != 0 || len(s.offlineSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("offline session/slot leaked: sessions=%d slots=%d", s.videoStreams.Count(), len(s.offlineSlots))
		}
		time.Sleep(time.Millisecond)
	}
}
