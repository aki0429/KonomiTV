//go:build !windows

package edcbpipe

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// DialControl は Python の open_unix_connection と同じ Unix stream socket を開く。
func DialControl(ctx context.Context, path string) (Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func tryOpenStream(ctx context.Context, path string) (io.ReadCloser, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, fmt.Errorf("edcb: expected FIFO")
	}
	// flock は file の close まで保持する。別クライアントは次の index を試す。
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	// NONBLOCK open は writer 不在でも成功するため、最初のデータが届くまで待つ。
	// 通常の blocking open を使わず、writer 不在時も deadline/cancel で終了する。
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for ctx.Err() == nil {
		if _, err := unix.Poll(poll, 50); err != nil && err != unix.EINTR {
			return nil, err
		}
		if poll[0].Revents&unix.POLLIN != 0 {
			success = true
			return file, nil
		}
		if poll[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return nil, io.EOF
		}
	}
	return nil, ctx.Err()
}
