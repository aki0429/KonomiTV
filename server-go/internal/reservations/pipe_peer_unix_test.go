//go:build !windows

package reservations

import (
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func pipePeer(t *testing.T, handler func(io.ReadWriteCloser)) *pipeTransport {
	t.Helper()
	dir := t.TempDir() + string(filepath.Separator)
	listener, err := net.Listen("unix", dir+"control")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			handler(conn)
		}
	}()
	return &pipeTransport{Path: dir, Name: "control", Timeout: 2 * time.Second}
}
