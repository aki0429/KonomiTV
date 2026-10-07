package reservations

import (
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// isWindows は実行環境が Windows かどうかを返す。
func isWindows() bool { return runtime.GOOS == "windows" }

// このファイルは server/app/utils/edcb/EDCBUtil.py の解析系ユーティリティを移植したもの。

// ConvertBytesToString のラッパー (既定エンコーディング cp932) 。
func ConvertEDCBBytesToString(buffer []byte) string {
	return ConvertBytesToString(buffer, "cp932")
}

// IniConfig は EDCB から取得した ini テキストを解析した結果。
//
// Python 版は configparser.ConfigParser を使っているが、Go には同等の標準ライブラリがないため、
// KonomiTV が使う範囲 (interpolation なし / strict=False / 大文字小文字の保持を選択可能 /
// コメントは行頭の # と ; のみ / 空行は複数行値に含める) を再実装している。
type IniConfig struct {
	sections map[string]map[string]string
	preserve bool
}

// HasSection は指定されたセクションが存在するかどうかを返す (Python 版の `name in config` 相当) 。
func (c *IniConfig) HasSection(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.sections[name]
	return ok
}

// Sections はセクション名の一覧を返す (順序は不定) 。
func (c *IniConfig) Sections() []string {
	names := make([]string, 0, len(c.sections))
	for name := range c.sections {
		names = append(names, name)
	}
	return names
}

// Items は指定されたセクションのキーと値の一覧を返す (存在しない場合は空) 。
func (c *IniConfig) Items(section string) map[string]string {
	if c == nil {
		return map[string]string{}
	}
	values, ok := c.sections[section]
	if !ok {
		return map[string]string{}
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

// Get は指定されたセクションのオプションを取得する (Python 版の section.get(key, fallback) 相当) 。
//
// 見つからない場合は fallback を返す。
func (c *IniConfig) Get(section string, option string, fallback string) string {
	if c == nil {
		return fallback
	}
	values, ok := c.sections[section]
	if !ok {
		return fallback
	}
	value, ok := values[c.transform(option)]
	if !ok {
		return fallback
	}
	return value
}

// transform は Python 版 configparser の optionxform 相当 (既定は小文字化) 。
func (c *IniConfig) transform(option string) string {
	if c.preserve {
		return option
	}
	return strings.ToLower(option)
}

// ParseEDCBIni は EDCB から取得した ini テキストを解析する。
//
// 移植元: EDCBUtil.parseEDCBIni()
//   - interpolation は無効 (BatFilePath の %SystemDrive% 等でエラーにしないため)
//   - strict=False (同一セクション内の重複キーは最後の値を採用する)
//   - preserveCase が true のときだけキー名の大文字小文字を保持する
func ParseEDCBIni(iniText string, preserveCase bool) (*IniConfig, error) {
	config := &IniConfig{sections: map[string]map[string]string{}, preserve: preserveCase}

	var (
		currentSection map[string]string
		currentOption  string
		hasOption      bool
		indentLevel    int
	)

	for _, rawLine := range splitKeepLines(iniText) {
		// Python 版 _Line.clean 相当 (行頭コメントなら空文字列、それ以外は strip した行)
		trimmed := pyStrip(rawLine)
		clean := trimmed
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			clean = ""
		}

		if clean == "" {
			// 空行・コメント行: 複数行値の途中なら空行を 1 行として追加する
			if !cleanIsComment(trimmed) && currentSection != nil && hasOption && currentOption != "" {
				if _, ok := currentSection[currentOption]; ok {
					currentSection[currentOption] += "\n"
				}
			}
			continue
		}

		curIndentLevel := utf8.RuneCountInString(rawLine) - utf8.RuneCountInString(strings.TrimLeftFunc(rawLine, pyIsSpace))

		// 継続行かどうか
		if currentSection != nil && hasOption && currentOption != "" && curIndentLevel > indentLevel {
			currentSection[currentOption] += "\n" + clean
			continue
		}

		indentLevel = curIndentLevel

		if header, ok := matchSectionHeader(clean); ok {
			if _, exists := config.sections[header]; exists {
				currentSection = config.sections[header]
			} else {
				currentSection = map[string]string{}
				config.sections[header] = currentSection
			}
			currentOption = ""
			hasOption = false
			continue
		}

		if currentSection == nil {
			// セクションヘッダーより前に行がある
			return config, &IniParseError{Message: "file contains no section headers"}
		}

		// オプション行
		delimiter := strings.IndexAny(clean, "=:")
		if delimiter < 0 {
			return config, &IniParseError{Message: "source contains parsing errors"}
		}
		option := strings.TrimRightFunc(clean[:delimiter], pyIsSpace)
		value := pyStrip(clean[delimiter+1:])
		if option == "" {
			return config, &IniParseError{Message: "source contains parsing errors"}
		}
		currentOption = config.transform(option)
		hasOption = true
		currentSection[currentOption] = value
	}

	// 複数行値の結合 (Python 版 _join_multiline_values) 。値は末尾の空白を取り除く
	for _, options := range config.sections {
		for name, value := range options {
			options[name] = strings.TrimRightFunc(value, pyIsSpace)
		}
	}
	return config, nil
}

// IniParseError は ini の解析に失敗したことを表す (Python 版 configparser.Error 相当) 。
type IniParseError struct {
	Message string
}

func (e *IniParseError) Error() string { return "failed to parse the ini text: " + e.Message }

// cleanIsComment は行全体がコメントかどうかを返す。
func cleanIsComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

// matchSectionHeader は Python 版 SECTCRE (`\[(.+)\]`, re.match) 相当の判定を行う。
func matchSectionHeader(clean string) (string, bool) {
	if !strings.HasPrefix(clean, "[") {
		return "", false
	}
	last := strings.LastIndex(clean, "]")
	if last < 2 {
		return "", false
	}
	return clean[1:last], true
}

// pyIsSpace は Python の str.isspace() / re の \s と同じ文字を空白として扱う。
func pyIsSpace(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '\x1c', '\x1d', '\x1e', '\x1f':
		return true
	}
	return false
}

// splitKeepLines は Python のファイル行イテレーション相当で行に分割する (\n 区切り) 。
func splitKeepLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

// splitLines は Python の str.splitlines() 相当で行に分割する。
func splitLines(text string) []string {
	lines := []string{}
	var builder strings.Builder
	runes := []rune(text)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if isPyLineBoundary(r) {
			lines = append(lines, builder.String())
			builder.Reset()
			if r == '\r' && index+1 < len(runes) && runes[index+1] == '\n' {
				index++
			}
			continue
		}
		builder.WriteRune(r)
	}
	if builder.Len() > 0 {
		lines = append(lines, builder.String())
	}
	return lines
}

// isPyLineBoundary は Python の str.splitlines() が行区切りとして扱う文字かどうかを返す。
func isPyLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\x85', '\u2028', '\u2029':
		return true
	}
	return false
}

// ParseChSet5 は ChSet5.txt を解析する (移植元: EDCBUtil.parseChSet5()) 。
//
// 9 フィールド未満の行や整数に変換できない行は読み飛ばす (Python 版と同じ) 。
func ParseChSet5(chset5Text string) []ChSet5Item {
	result := []ChSet5Item{}
	for _, line := range splitLines(chset5Text) {
		field := strings.Split(line, "\t")
		if len(field) < 9 {
			continue
		}
		onid, ok1 := pyInt(field[2])
		tsid, ok2 := pyInt(field[3])
		sid, ok3 := pyInt(field[4])
		serviceType, ok4 := pyInt(field[5])
		partialFlag, ok5 := pyInt(field[6])
		epgCapFlag, ok6 := pyInt(field[7])
		searchFlag, ok7 := pyInt(field[8])
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
			continue
		}
		remoconID := 0
		if len(field) >= 10 {
			parsed, ok := pyInt(field[9])
			if !ok {
				continue
			}
			remoconID = parsed
		}
		result = append(result, ChSet5Item{
			ServiceName: field[0],
			NetworkName: field[1],
			Onid:        onid,
			Tsid:        tsid,
			Sid:         sid,
			ServiceType: serviceType,
			PartialFlag: partialFlag != 0,
			EpgCapFlag:  epgCapFlag != 0,
			SearchFlag:  searchFlag != 0,
			RemoconID:   remoconID,
		})
	}
	return result
}

// fileTimeEpochOffset は 1601-01-01 から 1970-01-01 までの 100 ナノ秒単位の差。
const fileTimeEpochOffset = 116444736000000000

// DateTimeToFileTime は日時を FILETIME 時間 (1601 年からの 100 ナノ秒時刻) に変換する。
//
// 移植元: EDCBUtil.datetimeToFileTime()
// Python 版と同じく float 演算で計算してから整数へ切り捨てる (演算順序も合わせている) 。
func DateTimeToFileTime(value time.Time, offsetSeconds int) int64 {
	// Python の dt.timestamp() 相当 (UTC エポックからの秒数、マイクロ秒は float の小数部)
	utc := value.UTC()
	days := utc.Unix() / 86400
	seconds := utc.Unix() % 86400
	timestamp := float64(days*86400+seconds) + float64(utc.Nanosecond()/1000)/1e6
	return int64((timestamp+float64(offsetSeconds))*10000000) + fileTimeEpochOffset
}
