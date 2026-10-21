//go:build !windows

package metadata

import "os"

func finalizeCacheBackup(from, to string) error { return os.Rename(from, to) }

func syncCacheBackupDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
