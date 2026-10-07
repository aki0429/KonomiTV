package reservations

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ConvertBytesToString は BOM と文字列内容に基づいてバイト列を文字列へ変換する。
//
// 移植元: server/app/utils/edcb/EDCBUtil.py の EDCBUtil.convertBytesToString()
// BOM がない場合は UTF-8 (strict) を優先し、失敗時のみ既定エンコーディング (cp932) でデコードする。
// Linux 版 EDCB が UTF-8 (BOM なし) の ini を返すケースを吸収するための挙動。
func ConvertBytesToString(buffer []byte, defaultEncoding string) string {
	if len(buffer) == 0 {
		return ""
	}
	if len(buffer) >= 2 && buffer[0] == 0xFF && buffer[1] == 0xFE {
		return decodeUTF16LEWithReplacement(buffer[2:])
	}
	if len(buffer) >= 3 && buffer[0] == 0xEF && buffer[1] == 0xBB && buffer[2] == 0xBF {
		return decodeUTF8WithReplacement(buffer[3:])
	}
	if utf8.Valid(buffer) {
		return string(buffer)
	}
	if defaultEncoding == "cp932" || defaultEncoding == "" {
		return DecodeCP932WithReplacement(buffer)
	}
	// cp932 以外のフォールバックは Python 版でも実質使われないため、cp932 と同じ扱いにする。
	return DecodeCP932WithReplacement(buffer)
}

// decodeUTF16LEWithReplacement は UTF-16LE のバイト列をエラー置換付きで文字列へ変換する。
//
// Python の bytes.decode('utf_16_le', 'replace') 相当。奇数長の末尾は 1 バイトを置換文字にする。
func decodeUTF16LEWithReplacement(buffer []byte) string {
	units := make([]uint16, 0, len(buffer)/2)
	index := 0
	for ; index+1 < len(buffer); index += 2 {
		units = append(units, uint16(buffer[index])|uint16(buffer[index+1])<<8)
	}
	text := string(utf16.Decode(units))
	if index < len(buffer) {
		// 端数バイトは Python と同じく置換文字 1 文字になる
		text += "\uFFFD"
	}
	return text
}

// decodeUTF8WithReplacement は UTF-8 のバイト列をエラー置換付きで文字列へ変換する。
//
// Python の bytes.decode('utf_8', 'replace') 相当。Python は不正な並び 1 つにつき
// 置換文字 1 文字を出力するため、Go の range によるバイト単位置換とは結果が異なる場合がある。
func decodeUTF8WithReplacement(buffer []byte) string {
	var builder strings.Builder
	for len(buffer) > 0 {
		r, size := utf8.DecodeRune(buffer)
		if r == utf8.RuneError && size <= 1 {
			builder.WriteRune('\uFFFD')
			buffer = buffer[1:]
			continue
		}
		builder.WriteRune(r)
		buffer = buffer[size:]
	}
	return builder.String()
}

// DecodeCP932WithReplacement は cp932 (Shift_JIS) のバイト列をエラー置換付きで文字列へ変換する。
//
// Python の bytes.decode('cp932', 'replace') 相当。Python は未定義の並びでも
// 先行バイト 1 バイトだけを消費して置換文字を出力するため、その挙動に合わせている。
func DecodeCP932WithReplacement(buffer []byte) string {
	var builder strings.Builder
	for index := 0; index < len(buffer); {
		lead := buffer[index]
		leadIndex := cp932LeadIndex[lead]
		if leadIndex >= 0 && index+1 < len(buffer) {
			if trailIndex := cp932TrailIndex[buffer[index+1]]; trailIndex >= 0 {
				if r := cp932DoubleTable[int(leadIndex)*cp932TrailCount+trailIndex]; r != cp932Invalid {
					builder.WriteRune(r)
					index += 2
					continue
				}
			}
		}
		if r := cp932SingleTable[lead]; r != cp932Invalid {
			builder.WriteRune(r)
		} else {
			// 未定義の先行バイト・不正な後続バイトは置換文字にする (先行バイトのみ消費する)
			builder.WriteRune('\uFFFD')
		}
		index++
	}
	return builder.String()
}

// cp932TrailCount は cp932 の後続バイトの数 (cp932_table.go の生成結果と一致させる) 。
const cp932TrailCount = 188

// pyStrip は Python の str.strip() と同じ文字集合を両端から取り除く。
//
// Go の strings.TrimSpace は Unicode の White_Space を使うため、
// Python が空白として扱う \x1c〜\x1f が含まれない。configparser の再実装で差異が出ないよう揃えている。
func pyStrip(value string) string {
	return strings.Trim(value, pyWhitespace)
}

// pyWhitespace は Python の str.strip() / str.isspace() が空白として扱う文字集合。
const pyWhitespace = "\t\n\v\f\r \x1c\x1d\x1e\x1f\x85\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005" +
	"\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

// pyInt は Python の int(str) 相当の変換を行う。
//
// 前後の空白を許容し、符号とアンダースコア区切り (1_000) も受け付ける。
// 変換できない場合は Python の ValueError 相当として ok=false を返す。
func pyInt(value string) (int, bool) {
	text := pyStrip(value)
	// Python の int() は全角数字なども受け付けるが、EDCB の ini では実質使われないため扱わない。
	sign := 1
	if strings.HasPrefix(text, "+") {
		text = text[1:]
	} else if strings.HasPrefix(text, "-") {
		sign = -1
		text = text[1:]
	}
	if text == "" {
		return 0, false
	}
	digits := strings.ReplaceAll(text, "_", "")
	if digits == "" {
		return 0, false
	}
	if digits[0] == '_' || strings.HasSuffix(text, "_") {
		return 0, false
	}
	result := 0
	for index := 0; index < len(digits); index++ {
		char := digits[index]
		if char < '0' || char > '9' {
			return 0, false
		}
		result = result*10 + int(char-'0')
	}
	return sign * result, true
}

// PyRepr は Python の repr(str) 相当の文字列表現を返す (エラーメッセージの互換用) 。
func PyRepr(value string) string {
	var builder strings.Builder
	builder.WriteByte('\'')
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			builder.WriteString(`\'`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7F {
				builder.WriteString(fmt.Sprintf(`\x%02x`, r))
				continue
			}
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('\'')
	return builder.String()
}
