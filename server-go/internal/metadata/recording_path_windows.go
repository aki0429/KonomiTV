//go:build windows

package metadata

import (
	"fmt"
	"golang.org/x/sys/windows"
	"strings"
)

// EvalSymlinks だけでは残る junction/mount point も、開いた handle の実 target へ解決する。
func resolveRecordingFilesystemPath(path string) (string, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(encoded, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length >= 32768 {
			return "", fmt.Errorf("recording path is too long")
		}
		if length >= uint32(len(buffer)) {
			buffer = make([]uint16, length+1)
			continue
		}
		resolved := windows.UTF16ToString(buffer[:length])
		if strings.HasPrefix(resolved, `\\?\UNC\`) {
			return `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`), nil
		}
		return strings.TrimPrefix(resolved, `\\?\`), nil
	}
}
