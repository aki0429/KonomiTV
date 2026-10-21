//go:build windows

package edcbpipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// DialControl は期限内に制御パイプを開く。overlapped I/O により Read/Write を Close で解除できる。
func DialControl(ctx context.Context, path string) (Conn, error) {
	for {
		file, err := openPipe(path, windows.GENERIC_READ|windows.GENERIC_WRITE)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryOpenStream(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openPipe(path, windows.GENERIC_READ)
}

func openPipe(path string, access uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, access, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	kind, err := windows.GetFileType(handle)
	if err != nil || kind != windows.FILE_TYPE_PIPE {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("edcb: expected named pipe")
	}
	return os.NewFile(uintptr(handle), path), nil
}
