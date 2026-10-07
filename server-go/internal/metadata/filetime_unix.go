//go:build !windows

package metadata

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// fileCreationTime は Unix 系 OS のファイル状態変更日時 (ctime) を返す。
// 移植元: MetadataAnalyzer.analyze() の stat().st_ctime
func fileCreationTime(fileInfo os.FileInfo) time.Time {
	created, err := unixChangeTime(fileInfo)
	if err != nil {
		return fileInfo.ModTime().In(constants.JST)
	}
	return created
}

// unixChangeTime は POSIX の st_ctim を返す。
func unixChangeTime(fileInfo os.FileInfo) (time.Time, error) {
	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, fmt.Errorf("unexpected file info type")
	}
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).In(constants.JST), nil
}
