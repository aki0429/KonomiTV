package edcbpipe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/testutil/edcbipc"
)

func TestLocalStreamBytesAndEOF(t *testing.T) {
	directory := edcbipc.Directory(t)
	accept := edcbipc.StreamListener(t, edcbipc.StreamPath(directory, 17, 1234, 0))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reader, err := OpenStream(ctx, 1234, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// 接続に使った ctx の終了で、確立済みのストリームは切断されない。
	cancel()
	writer, err := accept()
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x47, 0x40, 0, 0x10, 0xff}, 15000)
	done := make(chan error, 1)
	go func() {
		for _, part := range [][]byte{want[:1], want[1:139], want[139:]} {
			if _, err := writer.Write(part); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("stream bytes changed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if n, err := reader.Read(make([]byte, 188)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after writer close: %d, %v", n, err)
	}
}

func TestLocalStreamCloseInterruptsRead(t *testing.T) {
	directory := edcbipc.Directory(t)
	accept := edcbipc.StreamListener(t, edcbipc.StreamPath(directory, 0, 4567, 0))
	reader, err := OpenStream(context.Background(), 4567, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := accept()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	done := make(chan error, 1)
	go func() { _, err := reader.Read(make([]byte, 188)); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Read returned before Close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	_ = reader.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed reader succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt Read")
	}
}

func TestLocalStreamWaitsForEndpoint(t *testing.T) {
	directory := edcbipc.Directory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		reader io.ReadCloser
		err    error
	}
	done := make(chan result, 1)
	go func() {
		reader, err := OpenStream(ctx, 6543, directory)
		done <- result{reader, err}
	}()
	select {
	case value := <-done:
		t.Fatalf("missing endpoint returned early: %v", value.err)
	case <-time.After(40 * time.Millisecond):
	}
	accept := edcbipc.StreamListener(t, edcbipc.StreamPath(directory, 2, 6543, 0))
	value := <-done
	if value.err != nil {
		t.Fatal(value.err)
	}
	defer value.reader.Close()
	writer, err := accept()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
}

func TestLocalStreamMissingAndCancelled(t *testing.T) {
	for _, duration := range []time.Duration{0, 40 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), duration)
		start := time.Now()
		reader, err := OpenStream(ctx, 7654, edcbipc.Directory(t))
		cancel()
		if reader != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("missing endpoint: %v, %v", reader, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("deadline was ignored")
		}
	}
}
