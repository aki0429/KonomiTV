package database

import (
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// TestParseDBTime は Tortoise ORM 形式の日時文字列のパースを検証する。
func TestParseDBTime(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"microseconds", "2026-09-22 15:11:50.725472+09:00", "2026-09-22T15:11:50.725472+09:00"},
		{"trailing zero", "2026-09-22 15:11:58.874290+09:00", "2026-09-22T15:11:58.874290+09:00"},
		{"no fraction", "2026-09-22 15:11:50+09:00", "2026-09-22T15:11:50+09:00"},
		{"no timezone", "2026-09-22 15:11:50.725472", "2026-09-22T15:11:50.725472+09:00"},
		{"iso T separator", "2026-09-22T15:11:50.725472+09:00", "2026-09-22T15:11:50.725472+09:00"},
		// SQLite の CURRENT_TIMESTAMP 由来 (UTC) の値は JST に変換される
		{"rfc3339 Z", "2026-10-03T20:29:06Z", "2026-10-04T05:29:06+09:00"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, err := ParseDBTime(testCase.value)
			if err != nil {
				t.Fatalf("ParseDBTime(%q) returned an error: %v", testCase.value, err)
			}
			if got := FormatJSONTime(parsed); got != testCase.want {
				t.Errorf("FormatJSONTime(ParseDBTime(%q)) = %q, want %q", testCase.value, got, testCase.want)
			}
		})
	}

	if _, err := ParseDBTime("not-a-date"); err == nil {
		t.Error("ParseDBTime() should fail for an invalid value")
	}
}

// TestFormatJSONTimeMatchesPydantic は Pydantic v2 の出力形式と一致することを検証する。
func TestFormatJSONTimeMatchesPydantic(t *testing.T) {
	// マイクロ秒あり: 必ず 6 桁で出力される (末尾の 0 を省略しない)
	withMicroseconds := time.Date(2026, 9, 22, 15, 11, 58, 874290000, constants.JST)
	if got, want := FormatJSONTime(withMicroseconds), "2026-09-22T15:11:58.874290+09:00"; got != want {
		t.Errorf("FormatJSONTime() = %q, want %q", got, want)
	}
	// マイクロ秒なし: 小数部自体を省略する
	withoutMicroseconds := time.Date(2026, 9, 22, 15, 11, 58, 0, constants.JST)
	if got, want := FormatJSONTime(withoutMicroseconds), "2026-09-22T15:11:58+09:00"; got != want {
		t.Errorf("FormatJSONTime() = %q, want %q", got, want)
	}
	// UTC の時刻は JST に変換される
	utcTime := time.Date(2026, 9, 22, 6, 11, 58, 0, time.UTC)
	if got, want := FormatJSONTime(utcTime), "2026-09-22T15:11:58+09:00"; got != want {
		t.Errorf("FormatJSONTime() = %q, want %q", got, want)
	}
}
