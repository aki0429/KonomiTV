// Package edcbipc はテスト専用の独立した EDCB 形式 IPC エンドポイントを作る。
package edcbipc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"
)

var sequence atomic.Uint64

func Directory(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\KonomiTV-test-%d-%d-`, os.Getpid(), sequence.Add(1))
}

func StreamPath(directory string, port, processID, _ int) string {
	return fmt.Sprintf("%sSendTSTCP_%d_%d", directory, port, processID)
}

func CommandListener(t testing.TB, path string) func() (io.ReadWriteCloser, error) {
	t.Helper()
	return pipeListener(t, path)
}

func StreamListener(t testing.TB, path string) func() (io.ReadWriteCloser, error) {
	t.Helper()
	return pipeListener(t, path)
}

func pipeListener(t testing.TB, path string) func() (io.ReadWriteCloser, error) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 65536, 65536, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() {
		_ = windows.CancelIoEx(handle, nil)
		_ = file.Close()
	})
	return func() (io.ReadWriteCloser, error) {
		event, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			return nil, err
		}
		defer windows.CloseHandle(event)
		overlapped := windows.Overlapped{HEvent: event}
		err = windows.ConnectNamedPipe(handle, &overlapped)
		if errors.Is(err, windows.ERROR_IO_PENDING) {
			var transferred uint32
			err = windows.GetOverlappedResult(handle, &overlapped, &transferred, true)
		}
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return nil, err
		}
		return &serverPipe{File: file, handle: handle}, nil
	}
}

// Windows のサーバー側 Close は未読データを捨てるため、テスト送信の完了を待って閉じる。
type serverPipe struct {
	*os.File
	handle windows.Handle
}

func (p *serverPipe) Close() error {
	_ = windows.FlushFileBuffers(p.handle)
	return p.File.Close()
}
