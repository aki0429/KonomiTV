package metadata

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseGenres は DB に保存された genres の JSON 文字列を解析する。
func ParseGenres(raw string) ([]Genre, error) {
	var items []struct {
		Major  string `json:"major"`
		Middle string `json:"middle"`
	}
	if strings.TrimSpace(raw) == "" {
		return []Genre{}, nil
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	genres := make([]Genre, 0, len(items))
	for _, item := range items {
		genres = append(genres, Genre{Major: item.Major, Middle: item.Middle})
	}
	return genres, nil
}

// ParseCMSections は DB に保存された cm_sections の JSON 文字列を解析する。
// JSON の null は「未解析」を表すため、空のスライスを返す。
func ParseCMSections(raw string) ([]CMSection, error) {
	var items []struct {
		StartTime float64 `json:"start_time"`
		EndTime   float64 `json:"end_time"`
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return []CMSection{}, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		return nil, err
	}
	sections := make([]CMSection, 0, len(items))
	for _, item := range items {
		sections = append(sections, CMSection{StartTime: item.StartTime, EndTime: item.EndTime})
	}
	sort.SliceStable(sections, func(left int, right int) bool {
		return sections[left].StartTime < sections[right].StartTime
	})
	return sections, nil
}
func pyFloat(value float64) string {
	text := strconv.FormatFloat(value, 'g', -1, 64)
	if !strings.ContainsAny(text, "eE") {
		// Go の 'g' (-1) は指数表記の閾値が Python と異なるため、Python に合わせて変換する
		if abs := absFloat(value); abs != 0 && (abs >= 1e16 || abs < 1e-4) {
			return fixPyExponent(strconv.FormatFloat(value, 'e', -1, 64))
		}
		// 整数値の場合は Pydantic/Tortoise と同じく ".0" を付与する (例: 1200.0)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return text
	}
	return fixPyExponent(text)
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// fixPyExponent は Go の指数表記 (1e+16) を Python の表記 (1e+16 / 1e-05) に揃える。
func fixPyExponent(text string) string {
	index := strings.IndexAny(text, "eE")
	if index < 0 {
		return text
	}
	mantissa, exponent := text[:index], text[index+1:]
	sign := "+"
	if strings.HasPrefix(exponent, "-") || strings.HasPrefix(exponent, "+") {
		sign = exponent[:1]
		exponent = exponent[1:]
	}
	if len(exponent) < 2 {
		exponent = "0" + exponent
	}
	return mantissa + "e" + sign + exponent
}

// pyString は Python の json.dumps(str, ensure_ascii=False) 相当のエスケープを行う。
func pyString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for index := 0; index < len(value); {
		r, size := utf8.DecodeRuneInString(value[index:])
		index += size
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		default:
			if r < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// CMSectionsJSON は cm_sections カラムに保存する JSON 文字列を返す。
func CMSectionsJSON(sections []CMSection) string {
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		parts = append(parts, `{"start_time": `+pyFloat(section.StartTime)+`, "end_time": `+pyFloat(section.EndTime)+`}`)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// genresJSON は genres カラムに保存する JSON 文字列を返す。
func genresJSON(genres []Genre) string {
	parts := make([]string, 0, len(genres))
	for _, genre := range genres {
		parts = append(parts, `{"major": `+pyString(genre.Major)+`, "middle": `+pyString(genre.Middle)+`}`)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// detailJSON は detail カラムに保存する JSON 文字列を返す。
func detailJSON(detail []DetailEntry) string {
	parts := make([]string, 0, len(detail))
	for _, entry := range detail {
		parts = append(parts, pyString(entry.Key)+": "+pyString(entry.Value))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// DetailJSON は detail カラム (Python の json.dumps(dict, ensure_ascii=False)) の JSON 文字列を返す。
func DetailJSON(detail []DetailEntry) string { return detailJSON(detail) }

// GenresJSON は genres カラムの JSON 文字列を返す。
func GenresJSON(genres []Genre) string { return genresJSON(genres) }
