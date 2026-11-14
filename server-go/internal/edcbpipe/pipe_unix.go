//go:build !windows

package edcbpipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const defaultDirectory = "/var/local/edcb/"

func streamPath(directory string, port, processID, index int) string {
	return fmt.Sprintf("%sSendTSTCP_%d_%d_%d.fifo", directory, port, processID, index)
}

// OpenCommand は通常ファイルではなく UNIX ドメインソケットへ接続する。
func OpenCommand(ctx context.Context, path string) (CommandConn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func openStreamFile(ctx context.Context, path string) (io.ReadCloser, bool, error) {
	// writer がまだない FIFO の open もキャンセル可能にする。
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	closeError := func(err error) (io.ReadCloser, bool, error) {
		_ = unix.Close(fd)
		return nil, true, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return closeError(err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFIFO {
		return closeError(fmt.Errorf("edcb: not a FIFO: %s", path))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return closeError(err)
	}
	// reader 単独の FIFO は read が 0 を返す。writer 接続まで待ち、最初の 1 バイトは保持する。
	var first [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return closeError(err)
		}
		n, err := unix.Read(fd, first[:])
		if n > 0 || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			file := os.NewFile(uintptr(fd), path)
			return &fifoReader{file: file, first: first[:max(n, 0)]}, true, nil
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return closeError(err)
		}
		// 一度接続した writer が無出力で閉じた場合は EOF を返せる接続として採用する。
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
		if _, err := unix.Poll(poll, 0); err != nil && !errors.Is(err, unix.EINTR) {
			return closeError(err)
		}
		if poll[0].Revents&unix.POLLHUP != 0 {
			return &fifoReader{file: os.NewFile(uintptr(fd), path)}, true, nil
		}
		if err := waitContext(ctx, 10*time.Millisecond); err != nil {
			return closeError(err)
		}
	}
}

// fifoReader は接続確認で読んだバイトを落とさず、以後の読み取りは Go の poller に任せる。
type fifoReader struct {
	file  *os.File
	first []byte
}

func (r *fifoReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 && len(p) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	return r.file.Read(p)
}

func (r *fifoReader) Close() error { return r.file.Close() }
