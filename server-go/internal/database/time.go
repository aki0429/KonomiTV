package database

import (
	"fmt"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// dbTimeLayouts は Tortoise ORM が SQLite に保存する日時文字列のパース候補。
// SQLite には Python の datetime.isoformat(" ") 形式 (例: "2025-09-22 15:47:00.123456+09:00") で
// 保存されている (tortoise/backends/sqlite/executor.py を参照) 。
var dbTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05-07:00",
}

// ParseDBTime は SQLite に保存された日時文字列を time.Time に変換する。
// タイムゾーン情報がない場合は JST として扱う。
func ParseDBTime(value string) (time.Time, error) {
	for _, layout := range dbTimeLayouts {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		if !strings.Contains(layout, "-07:00") {
			// レイアウトにタイムゾーンが含まれていない場合は time.Parse が UTC として解釈するため、JST に置き換える
			return time.Date(
				parsed.Year(), parsed.Month(), parsed.Day(),
				parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(),
				constants.JST,
			), nil
		}
		return parsed.In(constants.JST), nil
	}
	return time.Time{}, fmt.Errorf("failed to parse datetime string: %q", value)
}

// ParseNullableDBTime は NULL 許容の日時文字列をパースする。
func ParseNullableDBTime(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := ParseDBTime(*value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// FormatJSONTime は time.Time を Pydantic v2 / FastAPI と同じ ISO8601 形式の文字列に変換する。
// 例: "2026-09-22T15:11:50.725472+09:00" (マイクロ秒が 0 の場合は省略される)
func FormatJSONTime(value time.Time) string {
	inJST := value.In(constants.JST)
	if inJST.Nanosecond() == 0 {
		// Pydantic v2 はマイクロ秒が 0 の場合は小数部自体を省略する
		return inJST.Format("2006-01-02T15:04:05-07:00")
	}
	// Go の .999999 レイアウトは末尾の 0 を省略してしまうため、必ず 6 桁で出力する
	return inJST.Format("2006-01-02T15:04:05.000000-07:00")
}
