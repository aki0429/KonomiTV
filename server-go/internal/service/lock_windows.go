package service

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func acquireLock(file string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	overlapped := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another native service operation holds the lock: %w", err)
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, overlapped); _ = lock.Close() }, nil
}
