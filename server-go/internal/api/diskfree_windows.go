//go:build windows

package api

import (
	"errors"
	"syscall"
	"unsafe"
)

// freeDiskSpace は指定されたパスのドライブの空き容量 (バイト) を返す。
// 移植元: Python 版 CapturesRouter の shutil.disk_usage(upload_folder).free
func freeDiskSpace(path string) (uint64, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeAvailable, total, totalFree uint64
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")
	result, _, callError := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if result == 0 {
		return 0, callError
	}
	return freeAvailable, nil
}

// isDiskFullError はエラーがディスク容量不足によるものかを返す。
// 移植元: Python 版 CapturesRouter の errno.ENOSPC / winerror == 112 の判定
func isDiskFullError(err error) bool {
	if err == nil {
		return false
	}
	// ERROR_DISK_FULL (112) または ERROR_HANDLE_DISK_FULL (39)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.Errno(112) || errno == syscall.Errno(39)
	}
	return false
}
