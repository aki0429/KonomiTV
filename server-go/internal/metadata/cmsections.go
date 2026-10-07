package metadata

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CM 区間検出 (CMSectionsDetector) はチャプターファイル (.chapter.txt) があればそこから CM 区間を取得する。
// join_logo_scp による自前解析 (__detectWithJLS) は Python 版でも未実装 (常に None) のため移植しない。

// DetectCMSectionsFromChapterFile は録画ファイルに対応するチャプターファイルを解析して CM 区間を返す。
// チャプターファイルが存在しない場合と、パースに失敗した場合は (nil, false) を返す。
// 移植元: CMSectionsDetector.__detectFromChapterFile()
func DetectCMSectionsFromChapterFile(filePath string, durationSec float64) ([]CMSection, bool) {
	chapterPath := chapterFilePath(filePath)
	if _, err := os.Stat(chapterPath); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(chapterPath)
	if err != nil {
		return nil, false
	}
	// Python は universal newlines で splitlines する
	lines := splitChapterLines(string(data))

	type chapter struct {
		name string
		time float64
	}
	chapters := []chapter{}
	for index := 0; index+1 < len(lines); index += 2 {
		timeLine := strings.TrimSpace(lines[index])
		nameLine := strings.TrimSpace(lines[index+1])
		if !strings.HasPrefix(timeLine, "CHAPTER") || !strings.HasPrefix(nameLine, "CHAPTER") || !strings.Contains(nameLine, "NAME") {
			return nil, false
		}
		// チャプター番号を取得 (Python は int(time_line[7:9]) で失敗すると None を返す)
		if len(timeLine) < 9 {
			return nil, false
		}
		if _, err := strconv.Atoi(timeLine[7:9]); err != nil {
			return nil, false
		}
		timeParts := strings.Split(timeLine, "=")
		if len(timeParts) < 2 {
			return nil, false
		}
		chapterTime, ok := timeToSeconds(timeParts[1])
		if !ok {
			return nil, false
		}
		nameParts := strings.Split(nameLine, "=")
		if len(nameParts) < 2 {
			return nil, false
		}
		chapterName := nameParts[1]
		if chapterTime <= durationSec {
			chapters = append(chapters, chapter{name: chapterName, time: chapterTime})
		}
	}

	sections := []CMSection{}
	var currentCMStart *float64
	for _, item := range chapters {
		if strings.HasPrefix(item.name, "CM") && currentCMStart == nil {
			value := item.time
			currentCMStart = &value
		} else if !strings.HasPrefix(item.name, "CM") && currentCMStart != nil {
			sections = append(sections, CMSection{StartTime: *currentCMStart, EndTime: item.time})
			currentCMStart = nil
		}
	}
	// 最後のチャプターが CM で終わっている場合、動画長を終了時刻とする
	if currentCMStart != nil {
		sections = append(sections, CMSection{StartTime: *currentCMStart, EndTime: durationSec})
	}
	return sections, true
}

// chapterFilePath は録画ファイルに対応するチャプターファイルのパスを返す (hoge.ts → hoge.chapter.txt) 。
func chapterFilePath(filePath string) string {
	stem := filePath
	if index := strings.LastIndexAny(filePath, `/\`); index >= 0 {
		stem = filePath[index+1:]
	}
	if index := strings.LastIndex(stem, "."); index > 0 {
		stem = stem[:index]
	}
	directory := ""
	if index := strings.LastIndexAny(filePath, `/\`); index >= 0 {
		directory = filePath[:index+1]
	}
	return directory + stem + ".chapter.txt"
}

// splitChapterLines は Python の str.splitlines() 相当 (CRLF / CR / LF いずれも行区切りとして扱う) 。
func splitChapterLines(value string) []string {
	var lines []string
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\r':
			lines = append(lines, builder.String())
			builder.Reset()
			if index+1 < len(value) && value[index+1] == '\n' {
				index++
			}
		case '\n':
			lines = append(lines, builder.String())
			builder.Reset()
		default:
			builder.WriteByte(value[index])
		}
	}
	if builder.Len() > 0 {
		lines = append(lines, builder.String())
	}
	return lines
}

// timeToSeconds は "HH:MM:SS.mmm" 形式の時刻文字列を秒に変換する。
// 移植元: CMSectionsDetector.__timeToSeconds()
func timeToSeconds(value string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err != nil {
		return 0, false
	}
	return hours*3600 + minutes*60 + seconds, true
}

// detectCMSections は Python 版 CMSectionsDetector.detectAndSave() の CM 区間決定部分を再現する。
// チャプターファイルから取得できない場合は自前解析 (未実装) にフォールバックするため、結果は常に
// 「検出できた区間」または「検出できなかったことを表す空スライス」になる。
func detectCMSections(filePath string, durationSec float64) []CMSection {
	sections, ok := DetectCMSectionsFromChapterFile(filePath, durationSec)
	if ok {
		return sections
	}
	// __detectWithJLS() は Python 版でも未実装のため None が返る
	return []CMSection{}
}

var _ = fmt.Sprintf
