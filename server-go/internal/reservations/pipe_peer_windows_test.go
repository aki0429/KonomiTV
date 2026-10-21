//go:build windows

package reservations

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func pipePeer(t *testing.T, handler func(io.ReadWriteCloser)) *pipeTransport {
	t.Helper()
	dir := `\\.\pipe\`
	name := fmt.Sprintf("konomitv-control-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	path, _ := windows.UTF16PtrFromString(dir + name)
	handle, err := windows.CreateNamedPipe(path, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), dir+name)
	t.Cleanup(func() { _ = file.Close() })
	go func() {
		if err := windows.ConnectNamedPipe(handle, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return
		}
		handler(file)
	}()
	return &pipeTransport{Path: dir, Name: name, Timeout: 2 * time.Second}
}
