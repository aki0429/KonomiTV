//go:build !windows

package metadata

import "path/filepath"

func resolveRecordingFilesystemPath(path string) (string, error) { return filepath.EvalSymlinks(path) }
