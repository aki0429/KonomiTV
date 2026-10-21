//go:build windows

package config

import "golang.org/x/sys/windows"

// replaceConfigFile は同一ボリューム上のファイルを置換し、完了をディスクへ反映する。
func replaceConfigFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
