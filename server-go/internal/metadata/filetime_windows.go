//go:build windows

package metadata

import (
	"os"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// fileCreationTime は Windows のファイル作成日時を返す。
// 移植元: MetadataAnalyzer.analyze() の stat().st_ctime (Windows では作成日時を表す) 。
func fileCreationTime(fileInfo os.FileInfo) time.Time {
	created, err := windowsCreationTime(fileInfo)
	if err != nil {
		return fileInfo.ModTime().In(constants.JST)
	}
	return created
}
