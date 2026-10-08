package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// offlineKeepAliveInterval は保存中の録画セッションを維持する周期 (Python: 5 秒) 。
// セッションタイムアウト (10 秒) より短く、長い 1 セグメントのエンコード中も破棄されない。
const offlineKeepAliveInterval = 5 * time.Second

// SetVideoOfflineEncoderFactory は保存専用エンコーダーを起動前に設定する。
// HTTP リクエスト処理中の変更はサポートしない。
func (s *Server) SetVideoOfflineEncoderFactory(factory videostream.OfflineEncoderFactory) {
	s.offlineFactory = factory
}

// newSessionOfflineEncoder は通常視聴と独立した録画視聴セッションを作り、
// その仮想プレイリストの全セグメントを先頭から順に取り出す保存用エンコーダーを返す。
// 移植元: VideoStreamsRouter.VideoOfflineStreamAPI (VideoStream + getVirtualPlaylist + getSegment)
func (s *Server) newSessionOfflineEncoder(ctx context.Context, p videostream.OfflineParams) (videostream.OfflineEncoder, error) {
	// 応答開始前に入力の存在を確認し、開始できない保存を 500 として返す。
	if _, err := os.Stat(p.Program.Video.FilePath); err != nil {
		return nil, fmt.Errorf("open offline input: %w", err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	segmentMap, err := videostream.LoadSegmentMap(ctx, s.db, p.Program.Video.ID)
	if err != nil {
		s.logger.Warn("[VideoOfflineStreamAPI] Failed to load segment_map.", "error", err)
	}
	session, err := s.videoStreams.Get(videostream.SessionParams{
		SessionID:       "offline-" + hex.EncodeToString(random),
		Program:         p.Program,
		SegmentMap:      segmentMap,
		Quality:         p.Quality,
		EncodingOptions: p.EncodingOptions,
		Encoder:         p.Encoder,
		WriteDB:         s.writeDB,
	}, true)
	if err != nil {
		return nil, err
	}
	// 仮想プレイリスト生成で全セグメント情報を初期化する。
	session.GetVirtualPlaylist("offline", "primary")
	encoder := &sessionOfflineEncoder{session: session, stop: make(chan struct{}), stopped: make(chan struct{})}
	go encoder.keepAlive()
	return encoder, nil
}

// sessionOfflineEncoder は録画視聴セッションのセグメントを順に返し、Close でセッションを破棄する。
type sessionOfflineEncoder struct {
	session *videostream.Session
	next    int
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func (e *sessionOfflineEncoder) keepAlive() {
	defer close(e.stopped)
	ticker := time.NewTicker(offlineKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.session.KeepAlive()
		}
	}
}

func (e *sessionOfflineEncoder) Next(ctx context.Context) (videostream.OfflineSegment, error) {
	segments := e.session.Segments()
	if e.next >= len(segments) {
		return videostream.OfflineSegment{}, io.EOF
	}
	segment := segments[e.next]
	data, err := e.session.GetSegment(ctx, segment.SequenceIndex, "primary")
	if err != nil {
		return videostream.OfflineSegment{}, fmt.Errorf("offline segment generation failed. [sequence: %d]: %w", segment.SequenceIndex, err)
	}
	if len(data) == 0 {
		return videostream.OfflineSegment{}, fmt.Errorf("offline segment generation failed. [sequence: %d]", segment.SequenceIndex)
	}
	e.next++
	return videostream.OfflineSegment{DurationSeconds: segment.DurationSeconds, Data: data}, nil
}

// Close は維持処理を止めてからセッションとエンコーダーを破棄する。複数回呼んでも一度だけ実行する。
func (e *sessionOfflineEncoder) Close() error {
	e.once.Do(func() {
		close(e.stop)
		<-e.stopped
		e.session.Destroy()
	})
	return nil
}

// offlineResponseWriter は最初の書込みでのみバイナリ応答を開始し、開始状態を所有する。
type offlineResponseWriter struct {
	http.ResponseWriter
	started *bool
}

func (w *offlineResponseWriter) Write(data []byte) (int, error) {
	if !*w.started {
		*w.started = true
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	return w.ResponseWriter.Write(data)
}
func (w *offlineResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
