package reservations

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// GetLogoID は本家 EDCBUtil.getLogoIDFromLogoDataIni に従う。
func GetLogoID(text string, networkID, serviceID int) int {
	target := fmt.Sprintf("%04X%04X", networkID, serviceID)
	for _, line := range splitLines(text) {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.ToUpper(strings.TrimSpace(key)) == target {
			if id, err := logoInteger(value); err == nil {
				return id
			}
			break
		}
	}
	return -1
}

// logoInteger はINIの十進整数を読む。区切りの下線は数字の間だけ許す。
func logoInteger(value string) (int, error) {
	var normalized strings.Builder
	for _, r := range pyStrip(value) {
		if unicode.Is(unicode.Nd, r) {
			for _, span := range unicode.Nd.R16 {
				if r >= rune(span.Lo) && r <= rune(span.Hi) && (uint32(r)-uint32(span.Lo))%uint32(span.Stride) == 0 {
					r = '0' + (r-rune(span.Lo))/rune(span.Stride)%10
					break
				}
			}
			if r > 127 {
				for _, span := range unicode.Nd.R32 {
					if uint32(r) >= span.Lo && uint32(r) <= span.Hi && (uint32(r)-span.Lo)%span.Stride == 0 {
						r = '0' + rune((uint32(r)-span.Lo)/span.Stride%10)
						break
					}
				}
			}
		}
		normalized.WriteRune(r)
	}
	text := normalized.String()
	for i, r := range text {
		if r == '_' && (i == 0 || i+1 == len(text) || text[i-1] < '0' || text[i-1] > '9' || text[i+1] < '0' || text[i+1] > '9') {
			return 0, fmt.Errorf("invalid logo integer")
		}
	}
	return strconv.Atoi(strings.ReplaceAll(text, "_", ""))
}

// GetLogoFileName はディレクトリ一覧を行順で検索する。ロゴの版数でソートしない。
func GetLogoFileName(index string, networkID, logoID, kind int) string {
	target := fmt.Sprintf("%04X_%03X_", networkID, logoID)
	targetType := fmt.Sprintf("_%02d.", kind)
	for _, line := range splitLines(index) {
		fields := strings.SplitN(line, " ", 4)
		if len(fields) != 4 {
			continue
		}
		name := fields[3]
		if len(name) >= 16 && strings.ToUpper(name[:9]) == target && name[12:16] == targetType && !strings.ContainsAny(name, `/\`) {
			return name
		}
	}
	return ""
}
