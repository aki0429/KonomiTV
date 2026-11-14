package metadata

import (
	"context"
	"hash/fnv"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// TaskStatus は本家 MaintenanceTaskStatus と同じメモリ内の進捗情報。
type TaskStatus struct {
	Running    bool `json:"is_running"`
	Processed  int  `json:"processed_count"`
	Total      int  `json:"total_count"`
	Registered bool `json:"has_registered_files"`
}

var recordedPathLocks [256]sync.Mutex

// recordedPathLock は別の Service の手動解析・監視・欠落処理も同じパスで直列化する。
func recordedPathLock(path string) *sync.Mutex {
	path = filepath.Clean(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(path))
	return &recordedPathLocks[hash.Sum32()%uint32(len(recordedPathLocks))]
}

func acquireRecordedPath(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := recordedPathLock(path)
	if lock.TryLock() {
		return lock.Unlock, nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if lock.TryLock() {
				return lock.Unlock, nil
			}
		}
	}
}

func BatchScanStatus() TaskStatus {
	batchScanGuardMu.Lock()
	defer batchScanGuardMu.Unlock()
	return batchScanProgress
}

func updateBatchProgress(processed bool, total int) {
	batchScanGuardMu.Lock()
	defer batchScanGuardMu.Unlock()
	if total >= 0 {
		batchScanProgress.Total = total
		batchScanProgress.Registered = total > 0
	}
	if processed {
		batchScanProgress.Processed++
		if batchScanProgress.Processed > batchScanProgress.Total {
			batchScanProgress.Total = batchScanProgress.Processed
		}
	}
}

var analysisProgressMu sync.Mutex
var analysisProgress TaskStatus

var analysisRequests = make(chan struct{}, 1)

// BackgroundAnalysisRequests は取り込み直後の解析を、一つの低優先度 worker へまとめる。
func BackgroundAnalysisRequests() <-chan struct{} { return analysisRequests }
func requestBackgroundAnalysis() {
	select {
	case analysisRequests <- struct{}{}:
	default:
	}
}

func BackgroundAnalysisStatus() TaskStatus {
	analysisProgressMu.Lock()
	defer analysisProgressMu.Unlock()
	return analysisProgress
}
