// Package edcbpipe は EDCB のローカル制御・放送波パイプを扱う。
package edcbpipe

import (
	"context"
	"fmt"
	"io"
	"time"
)

// CommandConn は期限とキャンセルを適用できる双方向の制御接続。
type CommandConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// OpenStream は EDCBUtil.openPipeStream() と同じ順番で SrvPipe を探索する。
// directory が空なら OS ごとの既定パスを使う。Linux の同時利用は flock で選ぶ。
// 接続後の Read は Close で中断できる。ctx は接続待ちにのみ適用する。
func OpenStream(ctx context.Context, processID int, directory string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if directory == "" {
		directory = defaultDirectory
	}
	wait := 100 * time.Millisecond
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("edcb: open stream pipe: %w (last attempt: %v)", err, lastErr)
		}
		for port := range 30 {
			opened := false
			for index := range 2 {
				path := streamPath(directory, port, processID, index)
				reader, exists, err := openStreamFile(ctx, path)
				opened = opened || exists
				if err == nil {
					if err := ctx.Err(); err != nil {
						_ = reader.Close()
						return nil, err
					}
					return reader, nil
				}
				lastErr = err
				// 存在しない index 0 を飛ばして index 1 は開かない。
				if !exists || ctx.Err() != nil {
					break
				}
			}
			// 既存ポートの両 index がロック中なら、別ポートを探さず再試行する。
			if opened || ctx.Err() != nil {
				break
			}
		}
		if err := waitContext(ctx, wait); err != nil {
			return nil, fmt.Errorf("edcb: open stream pipe: %w (last attempt: %v)", err, lastErr)
		}
		wait = min(wait+100*time.Millisecond, time.Second)
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
