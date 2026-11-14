package config

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// settingsJSONParser は Python json.loads と同じ整数精度・非有限数・辞書の順序を保持する。
// 構文エラーの位置も FastAPI が使う文字数で返すため、UTF-8 のバイト位置だけに依存しない。
type settingsJSONParser struct {
	data  string
	pos   int
	order map[string][]string
	err   *SettingsValidationError
}

func settingsPathKey(path []any) string {
	value, _ := json.Marshal(path)
	return string(value)
}

func settingsChild(path []any, child any) []any {
	return append(append([]any{}, path...), child)
}

func (p *settingsJSONParser) fail(message string, position int) {
	if p.err == nil {
		p.err = &SettingsValidationError{Type: "json_invalid", Loc: []any{"body", utf8.RuneCountInString(p.data[:position])}, Msg: "JSON decode error", Input: map[string]any{}, Ctx: map[string]any{"error": message}}
	}
}

func (p *settingsJSONParser) space() {
	for p.pos < len(p.data) && strings.ContainsRune(" \t\n\r", rune(p.data[p.pos])) {
		p.pos++
	}
}

func (p *settingsJSONParser) text() string {
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		character := p.data[p.pos]
		if character < 0x20 {
			p.fail("Invalid control character at", p.pos)
			return ""
		}
		if character == '"' {
			p.pos++
			var text string
			_ = json.Unmarshal([]byte(p.data[start:p.pos]), &text)
			return text
		}
		if character == '\\' {
			if p.pos+1 >= len(p.data) {
				break
			}
			p.pos++
			if p.data[p.pos] == 'u' {
				if p.pos+4 >= len(p.data) {
					p.fail("Invalid \\uXXXX escape", p.pos)
					return ""
				}
				for i := 1; i <= 4; i++ {
					if !strings.ContainsRune("0123456789abcdefABCDEF", rune(p.data[p.pos+i])) {
						p.fail("Invalid \\uXXXX escape", p.pos)
						return ""
					}
				}
				p.pos += 4
			} else if !strings.ContainsRune(`"\/bfnrt`, rune(p.data[p.pos])) {
				p.fail("Invalid \\escape", p.pos-1)
				return ""
			}
		}
		p.pos++
	}
	p.fail("Unterminated string starting at", start)
	return ""
}

func (p *settingsJSONParser) value(path []any) any {
	// 通常の設定を超える深い入力で処理系のスタックやメモリを使い切らない。
	// Python 側も JSON デコード自体の失敗は型検査より前の 400 として返す。
	if len(path) > 4096 {
		p.err = &SettingsValidationError{Type: "body_error", Msg: "There was an error parsing the body"}
		return nil
	}
	p.space()
	if p.pos >= len(p.data) {
		p.fail("Expecting value", p.pos)
		return nil
	}
	switch p.data[p.pos] {
	case '"':
		return p.text()
	case '{':
		p.pos++
		p.space()
		object := map[string]any{}
		keys := []string{}
		if p.pos < len(p.data) && p.data[p.pos] == '}' {
			p.pos++
			p.order[settingsPathKey(path)] = keys
			return object
		}
		for p.err == nil {
			p.space()
			if p.pos >= len(p.data) || p.data[p.pos] != '"' {
				p.fail("Expecting property name enclosed in double quotes", p.pos)
				break
			}
			key := p.text()
			p.space()
			if p.pos >= len(p.data) || p.data[p.pos] != ':' {
				p.fail("Expecting ':' delimiter", p.pos)
				break
			}
			p.pos++
			if _, exists := object[key]; !exists {
				keys = append(keys, key)
			}
			object[key] = p.value(append(path, key))
			p.space()
			if p.err != nil {
				break
			}
			if p.pos < len(p.data) && p.data[p.pos] == '}' {
				p.pos++
				break
			}
			if p.pos >= len(p.data) || p.data[p.pos] != ',' {
				p.fail("Expecting ',' delimiter", p.pos)
				break
			}
			comma := p.pos
			p.pos++
			p.space()
			if p.pos < len(p.data) && p.data[p.pos] == '}' {
				p.fail("Illegal trailing comma before end of object", comma)
				break
			}
		}
		p.order[settingsPathKey(path)] = keys
		return object
	case '[':
		p.pos++
		p.space()
		array := []any{}
		if p.pos < len(p.data) && p.data[p.pos] == ']' {
			p.pos++
			return array
		}
		for p.err == nil {
			array = append(array, p.value(append(path, len(array))))
			p.space()
			if p.err != nil {
				break
			}
			if p.pos < len(p.data) && p.data[p.pos] == ']' {
				p.pos++
				break
			}
			if p.pos >= len(p.data) || p.data[p.pos] != ',' {
				p.fail("Expecting ',' delimiter", p.pos)
				break
			}
			comma := p.pos
			p.pos++
			p.space()
			if p.pos < len(p.data) && p.data[p.pos] == ']' {
				p.fail("Illegal trailing comma before end of array", comma)
				break
			}
		}
		return array
	}
	for _, literal := range []struct {
		text  string
		value any
	}{{"null", nil}, {"true", true}, {"false", false}, {"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)}} {
		if strings.HasPrefix(p.data[p.pos:], literal.text) {
			p.pos += len(literal.text)
			return literal.value
		}
	}
	// Python と同じ JSON 数値文法で読み、整数の桁を float64 へ丸めない。
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
		p.fail("Expecting value", start)
		return nil
	}
	if p.data[p.pos] == '0' {
		p.pos++
	} else {
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos+1 < len(p.data) && p.data[p.pos] == '.' && p.data[p.pos+1] >= '0' && p.data[p.pos+1] <= '9' {
		p.pos += 2
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		exponent := p.pos
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			p.pos = exponent
		} else {
			for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
				p.pos++
			}
		}
	}
	text := p.data[start:p.pos]
	if strings.ContainsAny(text, ".eE") {
		value, _ := strconv.ParseFloat(text, 64)
		return value
	}
	if len(strings.TrimPrefix(text, "-")) > 4300 {
		p.err = &SettingsValidationError{Type: "body_error", Msg: "There was an error parsing the body"}
		return nil
	}
	return json.Number(text)
}
