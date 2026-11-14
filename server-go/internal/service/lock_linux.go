package service

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireLock(file string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another native service operation holds the lock: %w", err)
	}
	return func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }, nil
}
