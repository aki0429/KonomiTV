//go:build windows

package edcbpipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestWindowsPipeStream は実 EDCB を使わず OS の名前付きパイプで受信と read 解除を検証する。
func TestWindowsPipeStream(t *testing.T) {
	dir := fmt.Sprintf(`\\.\pipe\konomitv-isolated-%d-%d-`, os.Getpid(), time.Now().UnixNano())
	name, _ := windows.UTF16PtrFromString(streamNames(dir, 4, 9000)[0])
	handle, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_OUTBOUND, windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	ready := make(chan error, 1)
	go func() {
		err := windows.ConnectNamedPipe(handle, nil)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			ready <- err
			return
		}
		var written uint32
		ready <- windows.WriteFile(handle, []byte("synthetic MPEG-TS"), &written, nil)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, 9000, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	data := make([]byte, len("synthetic MPEG-TS"))
	if _, err := io.ReadFull(stream, data); err != nil || string(data) != "synthetic MPEG-TS" {
		t.Fatalf("read=%q err=%v", data, err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); blocked <- err }()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("closed stream returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel pipe Read")
	}
}

func TestWindowsPipeMissingCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stream, err := OpenStream(ctx, 9000, `\\.\pipe\konomitv-missing-`); stream != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("stream=%v err=%v", stream, err)
	}
}
