//go:build !windows

package edcbpipe

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestUnixFIFOStream は実 FIFO の index 選択・排他・受信・close を検証する。
func TestUnixFIFOStream(t *testing.T) {
	dir := t.TempDir() + string(filepath.Separator)
	paths := streamNames(dir, 3, 9000)
	for _, path := range paths {
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockedFD, err := unix.Open(paths[0], unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(lockedFD)
	if err := unix.Flock(lockedFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			fd, err := unix.Open(paths[1], unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				time.Sleep(time.Millisecond)
				continue
			}
			file := os.NewFile(uintptr(fd), paths[1])
			defer file.Close()
			_, err = file.Write([]byte("synthetic MPEG-TS"))
			writerDone <- err
			select {
			case <-release:
			case <-ctx.Done():
			}
			return
		}
		writerDone <- ctx.Err()
	}()
	stream, err := OpenStream(ctx, 9000, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	data := make([]byte, len("synthetic MPEG-TS"))
	if _, err := io.ReadFull(stream, data); err != nil || string(data) != "synthetic MPEG-TS" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); blocked <- err }()
	_ = stream.Close()
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("closed reader succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release FIFO Read")
	}
	close(release)
}

func TestUnixFIFOWithoutWriterDeadline(t *testing.T) {
	dir := t.TempDir() + string(filepath.Separator)
	if err := unix.Mkfifo(streamNames(dir, 0, 9000)[0], 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if stream, err := OpenStream(ctx, 9000, dir); stream != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream=%v err=%v", stream, err)
	}
}
