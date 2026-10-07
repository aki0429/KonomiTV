package metadata

import (
	"strings"
	"unicode/utf8"
)

// formatString は文字列に含まれる英数や記号を半角に置換し、一律な表現に整える。
// 移植元: app.utils.TSInformation.formatString()
//
// Python 版は str.translate() で 1 文字ずつ置換した後、正規表現 (選択肢の定義順) で置換する。
// Go の strings.NewReplacer は最長一致ではないため、同じ挙動になるよう手動で走査する。
func formatString(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if replacement, exists := formatRuneTable[r]; exists {
			builder.WriteString(replacement)
		} else {
			builder.WriteRune(r)
		}
	}
	translated := builder.String()

	// 正規表現置換 (Python の re.sub 相当) 。左端から走査し、各位置で定義順に一致を試す。
	var result strings.Builder
	for index := 0; index < len(translated); {
		matched := false
		for _, pair := range formatRegexPairs {
			if strings.HasPrefix(translated[index:], pair[0]) {
				result.WriteString(pair[1])
				index += len(pair[0])
				matched = true
				break
			}
		}
		if !matched {
			r, size := utf8.DecodeRuneInString(translated[index:])
			result.WriteRune(r)
			index += size
		}
	}
	return result.String()
}
