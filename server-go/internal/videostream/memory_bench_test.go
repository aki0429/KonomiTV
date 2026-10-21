package videostream

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

type discardFeedWriter struct{ io.Writer }

func (discardFeedWriter) Close() error { return nil }

// BenchmarkRecordingFeeder は同じ 32 MiB 入力と書き込み先で本番 feeder の確保量を測定する。
func BenchmarkRecordingFeeder(b *testing.B) {
	file, err := os.Create(filepath.Join(b.TempDir(), "synthetic.ts"))
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	const size = 32 << 20
	if err := file.Truncate(size); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			b.Fatal(err)
		}
		task := NewVideoEncodingTask(&fakeSession{}, EncodingTaskOptions{})
		state := &encodingAttempt{task: task, feedDone: make(chan struct{}), feedWriter: discardFeedWriter{io.Discard}}
		task.startFeeder(state, file, nil, nil, &fakeSegment{}, &keyFrameQueue{})
		<-state.feedDone
	}
}
