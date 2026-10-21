//go:build windows

package metadata

import "golang.org/x/sys/windows"

func finalizeCacheBackup(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// ファイル本体を Sync/Close してから同一ディレクトリへ rename 済み。
func syncCacheBackupDirectory(string) error { return nil }
