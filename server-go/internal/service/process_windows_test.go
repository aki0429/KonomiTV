package service

import "golang.org/x/sys/windows"

func processExited(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == windows.ERROR_INVALID_PARAMETER {
		return true
	}
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && state == windows.WAIT_OBJECT_0
}
