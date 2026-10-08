package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// finiteOfflineEncoder は有限の合成セグメントを返し、終了の所有権を検証する。
type finiteOfflineEncoder struct {
	index  int
	closed atomic.Int32
}

func (e *finiteOfflineEncoder) Next(ctx context.Context) (videostream.OfflineSegment, error) {
	if e.index == 2 {
		return videostream.OfflineSegment{}, io.EOF
	}
	data := bytes.Repeat([]byte{byte(0x47 + e.index)}, 188)
	segment := videostream.OfflineSegment{DurationSeconds: []float64{6.006, 0.0005}[e.index], Data: data}
	e.index++
	return segment, nil
}
func (e *finiteOfflineEncoder) Close() error { e.closed.Add(1); return nil }

// blockedOfflineEncoder はキャンセル待ちを実際の HTTP 切断で解除する。
type blockedOfflineEncoder struct {
	closed  chan struct{}
	started chan struct{}
}

func (e *blockedOfflineEncoder) Next(ctx context.Context) (videostream.OfflineSegment, error) {
	close(e.started)
	<-ctx.Done()
	return videostream.OfflineSegment{}, ctx.Err()
}
func (e *blockedOfflineEncoder) Close() error { close(e.closed); return nil }

// TestPG4BodySlotsDisconnect は三枠の上限、待機キャンセル、切断後の再取得を固定する。
func TestPG4BodySlotsDisconnect(t *testing.T) {
	s, _ := setupVideoStreamTest(t)
	entered := make(chan *blockedOfflineEncoder, 8)
	s.SetVideoOfflineEncoderFactory(func(ctx context.Context, p videostream.OfflineParams) (videostream.OfflineEncoder, error) {
		e := &blockedOfflineEncoder{closed: make(chan struct{}), started: make(chan struct{})}
		entered <- e
		return e, nil
	})
	handler := s.Handler()
	contexts := make([]context.CancelFunc, 0, 4)
	finished := make([]chan struct{}, 0, 4)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, cancel)
		done := make(chan struct{})
		finished = append(finished, done)
		go func() {
			defer close(done)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/streams/video/1/720p/offline-stream", nil).WithContext(ctx))
		}()
	}
	defer func() {
		for _, cancel := range contexts {
			cancel()
		}
	}()
	encoders := make([]*blockedOfflineEncoder, 0, 3)
	for i := 0; i < 3; i++ {
		select {
		case e := <-entered:
			encoders = append(encoders, e)
		case <-time.After(2 * time.Second):
			t.Fatal("three slots did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("fourth encoder started without a free slot")
	case <-time.After(100 * time.Millisecond):
	}
	for _, cancel := range contexts {
		cancel()
	}
	for _, done := range finished {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancel leaked handler")
		}
	}
	for _, e := range encoders {
		select {
		case <-e.closed:
		default:
			t.Fatal("encoder was not closed")
		}
	}
	// 全解放後、別の実 HTTP 接続がメタデータを受信して切断できる。
	h := httptest.NewServer(handler)
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URL+"/api/streams/video/1/720p/offline-stream", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	e := <-entered
	response.Body.Close()
	cancel()
	select {
	case <-e.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP disconnect leaked encoder")
	}
}

// TestPG4BodyLegacyProxy は正常要求だけを旧保存機能へ転送し、検証エラーはローカルに保つ。
func TestPG4BodyLegacyProxy(t *testing.T) {
	s, _ := setupVideoStreamTest(t)
	var calls atomic.Int32
	s.proxy = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RequestURI() != "/api/streams/video/1/720p/offline-stream?session_id=legacy" {
			t.Error("request changed")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("legacy-synthetic-body"))
	})
	recorder := videoStreamRequest_(t, s, http.MethodGet, "/api/streams/video/1/720p/offline-stream?session_id=legacy")
	if recorder.Code != 200 || recorder.Body.String() != "legacy-synthetic-body" || calls.Load() != 1 {
		t.Fatalf("valid request stopped legacy save: %d %q calls=%d", recorder.Code, recorder.Body.String(), calls.Load())
	}
	assertPG4Error(t, s, http.MethodGet, "/api/streams/video/9999/720p/offline-stream", 404, "Specified video_id was not found")
	if calls.Load() != 1 {
		t.Fatal("invalid request reached proxy")
	}
}

// failingOfflineEncoder は本文開始後の例外を再現する。
type failingOfflineEncoder struct {
	panicValue bool
	closed     atomic.Int32
}

func (e *failingOfflineEncoder) Next(context.Context) (videostream.OfflineSegment, error) {
	if e.panicValue {
		panic("synthetic encoder panic")
	}
	return videostream.OfflineSegment{}, fmt.Errorf("synthetic failure")
}
func (e *failingOfflineEncoder) Close() error { e.closed.Add(1); return nil }

// TestPG4BodyFailureRelease は初期化失敗・nil・panic・本文失敗・metadata 上限を分離する。
func TestPG4BodyFailureRelease(t *testing.T) {
	for _, kind := range []string{"initialize", "nil", "stream", "panic", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := setupVideoStreamTest(t)
			encoder := &failingOfflineEncoder{panicValue: kind == "panic"}
			if kind == "metadata" {
				if _, err := s.db.Exec("UPDATE recorded_videos SET file_hash=?", string(bytes.Repeat([]byte{'a'}, 1<<20))); err != nil {
					t.Fatal(err)
				}
			}
			s.SetVideoOfflineEncoderFactory(func(context.Context, videostream.OfflineParams) (videostream.OfflineEncoder, error) {
				if kind == "initialize" {
					return nil, fmt.Errorf("start failed")
				}
				if kind == "nil" {
					return nil, nil
				}
				return encoder, nil
			})
			recorder := videoStreamRequest_(t, s, http.MethodGet, "/api/streams/video/1/720p/offline-stream")
			if kind == "stream" || kind == "panic" {
				if recorder.Code != 200 || !bytes.HasPrefix(recorder.Body.Bytes(), []byte("KTVODLP\n")) || bytes.Contains(recorder.Body.Bytes(), []byte("Internal Server Error")) {
					t.Fatalf("started response polluted: %d %q", recorder.Code, recorder.Body.String())
				}
			} else {
				if recorder.Code != 500 || !bytes.Contains(recorder.Body.Bytes(), []byte("detail")) {
					t.Fatalf("pre-start failure %d %q", recorder.Code, recorder.Body.String())
				}
			}
			if kind != "initialize" && kind != "nil" && encoder.closed.Load() != 1 {
				t.Fatalf("close count %d", encoder.closed.Load())
			}
			if len(s.offlineSlots) != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

// TestPG4BodyProductionWiring は通常視聴 factory 登録時に保存専用工場も接続する。
func TestPG4BodyProductionWiring(t *testing.T) {
	s, _ := setupVideoStreamTest(t)
	s.SetVideoSegmentEncoderFactory(videostream.NewSessionSegmentEncoderFactory(videostream.EncodingTaskOptions{}))
	recorder := videoStreamRequest_(t, s, http.MethodGet, "/api/streams/video/1/720p/offline-stream")
	if recorder.Code != 500 || recorder.Body.String() != "{\"detail\":\"Failed to initialize offline stream\"}\n" {
		t.Fatalf("native factory not wired: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPG4BodyFiniteHTTP は実 HTTP 応答全体をクライアントと同じ順序で復元する。
func TestPG4BodyFiniteHTTP(t *testing.T) {
	s, id := setupVideoStreamTest(t)
	encoder := &finiteOfflineEncoder{}
	var calls atomic.Int32
	s.SetVideoOfflineEncoderFactory(func(ctx context.Context, p videostream.OfflineParams) (videostream.OfflineEncoder, error) {
		calls.Add(1)
		if p.Program.Program.ID != id || p.Quality != "720p-hevc" || !p.EncodingOptions.Is24fpsModeEnabled {
			return nil, fmt.Errorf("unexpected params: %+v", p)
		}
		return encoder, nil
	})
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	quality := "720p-hevc-10bit-24fps"
	response, err := client.Get(fmt.Sprintf("%s/api/streams/video/%d/%s/offline-stream", h.URL, id, quality))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status %d body %s", response.StatusCode, body)
	}
	for key, want := range map[string]string{"Content-Type": "application/octet-stream", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := response.Header.Get(key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
	reader := bytes.NewReader(body)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != "KTVODLP\n" {
		t.Fatalf("magic %q %v", magic, err)
	}
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		t.Fatal(err)
	}
	metadata := make([]byte, length)
	if _, err := io.ReadFull(reader, metadata); err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 4 || values["video_id"] != float64(id) || values["file_hash"] != "hash" || values["quality"] != quality || values["duration_seconds"] != float64(3600) {
		t.Fatalf("metadata %s", metadata)
	}
	for i, duration := range []uint32{6006, 1} {
		var header [3]uint32
		if err := binary.Read(reader, binary.BigEndian, &header); err != nil {
			t.Fatal(err)
		}
		if header != [3]uint32{uint32(i), duration, 188} {
			t.Fatalf("segment header %v", header)
		}
		data := make([]byte, 188)
		if _, err := io.ReadFull(reader, data); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, bytes.Repeat([]byte{byte(0x47 + i)}, 188)) {
			t.Fatal("segment data changed")
		}
	}
	var terminal [2]uint32
	if err := binary.Read(reader, binary.BigEndian, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != [2]uint32{0xffffffff, 2} || reader.Len() != 0 {
		t.Fatalf("terminal %v remaining %d", terminal, reader.Len())
	}
	deadline := time.After(2 * time.Second)
	for encoder.closed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("encoder not released")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if encoder.closed.Load() != 1 || calls.Load() != 1 || s.videoStreams.Count() != 0 {
		t.Fatalf("close=%d calls=%d sessions=%d", encoder.closed.Load(), calls.Load(), s.videoStreams.Count())
	}
}
