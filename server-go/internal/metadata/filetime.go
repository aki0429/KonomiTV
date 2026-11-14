package metadata

import (
	"os"
	"time"
)

// FileCreationTime は本家 stat().st_ctime と同じ意味の時刻を OS ごとに取得する。
func FileCreationTime(info os.FileInfo) time.Time { return fileCreationTime(info) }
