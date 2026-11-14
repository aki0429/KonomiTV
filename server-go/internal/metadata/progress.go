package metadata

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// TaskStatus は本家 MaintenanceTaskStatus と同じメモリ内の進捗情報。
type TaskStatus struct {
	Running    bool `json:"is_running"`
	Processed  int  `json:"processed_count"`
	Total      int  `json:"total_count"`
	Registered bool `json:"has_registered_files"`
}

type recordedPathEntry struct {
	token chan struct{}
	users int
}

var recordedLocksMu sync.Mutex
var recordedLocks = map[string]*recordedPathEntry{}

// recordedPathKey は同じファイルに対する手動解析・監視・欠落処理のキーを揃える。
func recordedPathKey(path string) string {
	path = filepath.Clean(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func acquireRecordedPath(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := recordedPathKey(path)
	recordedLocksMu.Lock()
	entry := recordedLocks[key]
	if entry == nil {
		entry = &recordedPathEntry{token: make(chan struct{}, 1)}
		recordedLocks[key] = entry
	}
	entry.users++
	recordedLocksMu.Unlock()
	// 待機者も参照数に含め、キャンセルや終了後には使わないパスを辞書から除く。
	dropReference := func() {
		recordedLocksMu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(recordedLocks, key)
		}
		recordedLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		dropReference()
		return nil, ctx.Err()
	case entry.token <- struct{}{}:
		unlock := func() { <-entry.token; dropReference() }
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
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
