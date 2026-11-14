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

const defaultDirectory = `\\.\pipe\`

func streamPath(directory string, port, processID, _ int) string {
	return fmt.Sprintf("%sSendTSTCP_%d_%d", directory, port, processID)
}

func openStreamFile(ctx context.Context, path string) (io.ReadCloser, bool, error) {
	file, err := openWindowsPipe(ctx, path, windows.GENERIC_READ)
	return file, false, err
}

// OpenCommand は CtrlCmd の名前付きパイプを開く。使用中のインスタンスは本家同様に再試行する。
func OpenCommand(ctx context.Context, path string) (CommandConn, error) {
	for {
		file, err := openWindowsPipe(ctx, path, windows.GENERIC_READ|windows.GENERIC_WRITE)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		if err := waitContext(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

func openWindowsPipe(ctx context.Context, path string, access uint32) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// OVERLAPPED を指定し Go の IOCP に登録する。同期ハンドルでは Close が Read 待ちを解除できない。
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	if kind, err := windows.GetFileType(handle); err != nil || kind != windows.FILE_TYPE_PIPE {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("edcb: not a named pipe: %s", path)
	}
	file := os.NewFile(uintptr(handle), path)
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
