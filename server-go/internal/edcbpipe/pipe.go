// Package edcbpipe は EDCB のローカル制御通信と SendTSTCP ストリームを開く。
package edcbpipe

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"
)

// Conn は制御通信に必要な入出力と期限設定を提供する。
type Conn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// OpenStream は Python の EDCBUtil.openPipeStream と同じ順で 30 port を探索する。
// Linux は FIFO の排他ロックにより同時利用 index を選び、Windows は名前付きパイプを使う。
// dir は隔離テスト用。空文字なら Python と同じ既定ディレクトリを使用する。
func OpenStream(ctx context.Context, processID int, dir string) (io.ReadCloser, error) {
	if processID <= 0 {
		return nil, fmt.Errorf("edcb: invalid process id")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	wait := 100 * time.Millisecond
	var last error
	for ctx.Err() == nil {
		for port := range 30 {
			for _, path := range streamNames(dir, port, processID) {
				stream, err := tryOpenStream(ctx, path)
				if err == nil {
					return stream, nil
				}
				last = err
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		wait = min(wait+100*time.Millisecond, time.Second)
	}
	return nil, fmt.Errorf("edcb: open pipe stream: %w (last error: %v)", ctx.Err(), last)
}

func streamNames(dir string, port, processID int) []string {
	if runtime.GOOS == "windows" {
		if dir == "" {
			dir = `\\.\pipe\`
		}
		return []string{fmt.Sprintf("%sSendTSTCP_%d_%d", dir, port, processID)}
	}
	if dir == "" {
		dir = "/var/local/edcb/"
	}
	return []string{fmt.Sprintf("%sSendTSTCP_%d_%d_0.fifo", dir, port, processID), fmt.Sprintf("%sSendTSTCP_%d_%d_1.fifo", dir, port, processID)}
}
