//go:build !windows

package edcbpipe

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/testutil/edcbipc"
	"golang.org/x/sys/unix"
)

func TestFIFOIndexLockAndPortPriority(t *testing.T) {
	directory := edcbipc.Directory(t)
	writers := make([]*os.File, 3)
	for i, pair := range [][2]int{{0, 0}, {0, 1}, {1, 0}} {
		accept := edcbipc.StreamListener(t, streamPath(directory, pair[0], 123, pair[1]))
		writer, err := accept()
		if err != nil {
			t.Fatal(err)
		}
		writers[i] = writer.(*os.File)
		if _, err := writer.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, writer := range writers[:2] {
		if err := unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if reader, err := OpenStream(ctx, 123, directory); reader != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("both indices locked must not advance to port 1: %v, %v", reader, err)
	}
	if err := unix.Flock(int(writers[1].Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenStream(context.Background(), 123, directory)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte{255}
	if _, err := io.ReadFull(reader, data); err != nil || data[0] != 1 {
		t.Fatalf("expected index 1 and preserved prefetched byte, got %v, %v", data, err)
	}
	_ = reader.Close()
	// Close で flock も解放され、同じ index を次のタスクが取得できる。
	if err := unix.Flock(int(writers[1].Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("reader lock leaked: %v", err)
	}
}

func TestFIFOMissingIndexZeroSkipsIndexOne(t *testing.T) {
	directory := edcbipc.Directory(t)
	edcbipc.StreamListener(t, streamPath(directory, 0, 321, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	if reader, err := OpenStream(ctx, 321, directory); reader != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must not open index 1 without index 0: %v, %v", reader, err)
	}
}

func TestFIFOWithoutWriterCancellation(t *testing.T) {
	directory := edcbipc.Directory(t)
	path := streamPath(directory, 0, 999, 0)
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	if reader, err := OpenStream(ctx, 999, directory); reader != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer wait: %v, %v", reader, err)
	}
	writer, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("cancelled opener leaked the lock: %v", err)
	}
}
