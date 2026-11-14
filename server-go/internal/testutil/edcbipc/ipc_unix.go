//go:build !windows

// Package edcbipc はテスト専用の独立した EDCB 形式 IPC エンドポイントを作る。
package edcbipc

import (
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func Directory(t testing.TB) string {
	t.Helper()
	return t.TempDir() + "/"
}

func StreamPath(directory string, port, processID, index int) string {
	return fmt.Sprintf("%sSendTSTCP_%d_%d_%d.fifo", directory, port, processID, index)
}

func CommandListener(t testing.TB, path string) func() (io.ReadWriteCloser, error) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return func() (io.ReadWriteCloser, error) { return listener.Accept() }
}

func StreamListener(t testing.TB, path string) func() (io.ReadWriteCloser, error) {
	t.Helper()
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// テスト writer が reader の接続待ちでブロックしないよう双方向で開く。
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return func() (io.ReadWriteCloser, error) { return file, nil }
}
