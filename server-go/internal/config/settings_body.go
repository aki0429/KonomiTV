package config

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// settingsBodyBytes は JSON 以外のリクエスト本文を、検証エラー内で Python の bytes と同じく扱う。
type settingsBodyBytes []byte

func (value settingsBodyBytes) MarshalJSON() ([]byte, error) {
	if !utf8.Valid(value) {
		return nil, fmt.Errorf("request body is not valid UTF-8")
	}
	return []byte(encodeJSONString(string(value))), nil
}

// ValidateClientSettingsBody は FastAPI と同じ Content-Type 判定・本文の文字コード検出後に検証する。
// 保存済みの JSON 文字列の検査とは分け、HTTP 本文としてだけ UTF BOM・UTF-16/32 を受理する。
func ValidateClientSettingsBody(raw []byte, contentType string) (map[string]any, []SettingsValidationError) {
	if len(raw) == 0 {
		return ValidateClientSettingsJSON(raw)
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	isJSON := media == "application/json" || (strings.Count(media, "/") == 1 && strings.HasPrefix(media, "application/") && strings.HasSuffix(media, "+json"))
	if !isJSON {
		return nil, []SettingsValidationError{{Type: "model_attributes_type", Loc: []any{"body"}, Msg: "Input should be a valid dictionary or object to extract fields from", Input: settingsBodyBytes(raw)}}
	}
	decoded, ok := decodeSettingsBody(raw)
	if !ok {
		return nil, []SettingsValidationError{{Type: "body_error", Msg: "There was an error parsing the body"}}
	}
	return ValidateClientSettingsJSON(decoded)
}

// decodeSettingsBody は Python json.detect_encoding() と同じ BOM と先頭のゼロバイトを調べる。
func decodeSettingsBody(raw []byte) ([]byte, bool) {
	width, little, start := 1, false, 0
	switch {
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe, 0, 0}):
		width, little, start = 4, true, 4
	case bytes.HasPrefix(raw, []byte{0, 0, 0xfe, 0xff}):
		width, start = 4, 4
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe}):
		width, little, start = 2, true, 2
	case bytes.HasPrefix(raw, []byte{0xfe, 0xff}):
		width, start = 2, 2
	case bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}):
		start = 3
	case len(raw) >= 4:
		if raw[0] == 0 {
			width = 2
			if raw[1] == 0 {
				width = 4
			}
		} else if raw[1] == 0 {
			width, little = 2, true
			if raw[2] == 0 && raw[3] == 0 {
				width = 4
			}
		}
	case len(raw) == 2:
		if raw[0] == 0 {
			width = 2
		} else if raw[1] == 0 {
			width, little = 2, true
		}
	}
	data := raw[start:]
	if width == 1 {
		return data, utf8.Valid(data)
	}
	if len(data)%width != 0 {
		return nil, false
	}
	var order binary.ByteOrder = binary.BigEndian
	if little {
		order = binary.LittleEndian
	}
	var text []rune
	if width == 2 {
		units := make([]uint16, len(data)/2)
		for i := range units {
			units[i] = order.Uint16(data[i*2:])
		}
		text = utf16.Decode(units)
	} else {
		text = make([]rune, len(data)/4)
		for i := range text {
			code := order.Uint32(data[i*4:])
			if code > utf8.MaxRune || code >= 0xd800 && code <= 0xdfff {
				return nil, false
			}
			text[i] = rune(code)
		}
	}
	return []byte(string(text)), true
}
