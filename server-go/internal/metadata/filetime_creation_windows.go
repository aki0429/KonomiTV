//go:build windows

package metadata

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// windowsCreationTime は Windows API (GetFileAttributesEx) の FileCreationTime を返す。
func windowsCreationTime(fileInfo os.FileInfo) (time.Time, error) {
	stat, ok := fileInfo.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, fmt.Errorf("unexpected file info type")
	}
	created := time.Unix(0, stat.CreationTime.Nanoseconds())
	return created.In(constants.JST), nil
}
