package metadata

import (
	"context"
	"os"
	"sync"
	"time"
)

// メタデータ解析は全 Service で一つまで。視聴エンコーダーの枠は消費しない。
var metadataIO = make(chan struct{}, 1)

func acquireMetadataIO(ctx context.Context, skipBusy bool) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if skipBusy {
		select {
		case metadataIO <- struct{}{}:
			return func() { <-metadataIO }, true, nil
		default:
			return nil, false, nil
		}
	}
	select {
	case metadataIO <- struct{}{}:
		return func() { <-metadataIO }, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// directoryReadGate は ReadDir(128) を全スキャン合計100回/秒以下へ平準化する。
// 秒間バイト数を保証するものではない。大量フォルダで metadata IOPS を独占しない。
var directoryReadGate struct {
	sync.Mutex
	next time.Time
}

func readRecordingDirectory(ctx context.Context, file *os.File) ([]os.DirEntry, error) {
	directoryReadGate.Lock()
	now := time.Now()
	slot := directoryReadGate.next
	if slot.Before(now) {
		slot = now
	}
	directoryReadGate.next = slot.Add(10 * time.Millisecond)
	directoryReadGate.Unlock()
	timer := time.NewTimer(time.Until(slot))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	return file.ReadDir(128)
}
