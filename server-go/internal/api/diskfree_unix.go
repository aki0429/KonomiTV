//go:build !windows

package api

import (
	"errors"
	"syscall"
)

// freeDiskSpace は指定されたパスのドライブの空き容量 (バイト) を返す。
// 移植元: Python 版 CapturesRouter の shutil.disk_usage(upload_folder).free
func freeDiskSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Bavail (非特権ユーザーが利用可能なブロック数) を空き容量として扱う
	return stat.Bavail * uint64(stat.Bsize), nil
}

// isDiskFullError はエラーがディスク容量不足によるものかを返す。
// 移植元: Python 版 CapturesRouter の errno.ENOSPC の判定
func isDiskFullError(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
