package twitter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// OrderedMap は挿入順を保持するマップ。
//
// Python 版は dict の挿入順をそのまま JSON のキー順として Twitter Web App へ送信するため、
// Go でも順序を厳密に再現できる必要がある (map[string]any は Go の encoding/json が
// キーをソートしてしまうため使えない) 。
type OrderedMap struct {
	keys   []string
	values map[string]any
}

// NewOrderedMap は空の OrderedMap を生成する。
func NewOrderedMap() *OrderedMap {
	return &OrderedMap{values: map[string]any{}}
}

// Set はキーを設定する (既存キーの場合は順序を保持したまま値を上書きする) 。
func (ordered *OrderedMap) Set(key string, value any) *OrderedMap {
	if ordered.values == nil {
		ordered.values = map[string]any{}
	}
	if _, exists := ordered.values[key]; !exists {
		ordered.keys = append(ordered.keys, key)
	}
	ordered.values[key] = value
	return ordered
}

// Get はキーの値を取得する。
func (ordered *OrderedMap) Get(key string) (any, bool) {
	value, exists := ordered.values[key]
	return value, exists
}

// Len は要素数を返す。
func (ordered *OrderedMap) Len() int {
	return len(ordered.keys)
}

// Keys は挿入順のキー一覧を返す。
func (ordered *OrderedMap) Keys() []string {
	return ordered.keys
}

// Map はキー順を保持しない map へ変換する (テストの比較用) 。
func (ordered *OrderedMap) Map() map[string]any {
	result := make(map[string]any, len(ordered.keys))
	for key, value := range ordered.values {
		result[key] = value
	}
	return result
}

// PythonJSON は Python の json.dumps(value, ensure_ascii=False) と同じバイト列を生成する。
//
// Python 版 TwitterGraphQLAPI は variables / additional_flags を json.dumps で
// JavaScript へ埋め込むため、Go 側も同じ出力にならなければリクエストが一致しない。
// Go の encoding/json とは以下の点が異なるため、独自にエンコードする。
//   - セパレータは Python 既定の ', ' と ': ' (Go は ',' と ':') 。
//   - '<' '>' '&' や U+2028 / U+2029 をエスケープしない (Go はエスケープする) 。
//   - 制御文字は Python と同じ短縮表記 (\n \t \r \b \f) と \uXXXX を使う。
func PythonJSON(value any) ([]byte, error) {
	buffer := &bytes.Buffer{}
	if err := writePythonJSON(buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writePythonJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		writePythonJSONString(buffer, typed)
	case int:
		buffer.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		buffer.WriteString(strconv.FormatInt(typed, 10))
	case float64:
		writePythonJSONFloat(buffer, typed)
	case json.Number:
		buffer.WriteString(typed.String())
	case *OrderedMap:
		buffer.WriteByte('{')
		for index, key := range typed.keys {
			if index > 0 {
				buffer.WriteString(", ")
			}
			writePythonJSONString(buffer, key)
			buffer.WriteString(": ")
			if err := writePythonJSON(buffer, typed.values[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []string:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteString(", ")
			}
			writePythonJSONString(buffer, item)
		}
		buffer.WriteByte(']')
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteString(", ")
			}
			if err := writePythonJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		// Python の dict の順序は再現できないため、キーをソートして決定的にする。
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteString(", ")
			}
			writePythonJSONString(buffer, key)
			buffer.WriteString(": ")
			if err := writePythonJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return errors.New("unsupported type for PythonJSON: " + fmt.Sprintf("%T", value))
	}
	return nil
}

func writePythonJSONFloat(buffer *bytes.Buffer, value float64) {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		// Python の json.dumps は NaN / Infinity をそのまま出力する
		buffer.WriteString("NaN")
		return
	}
	buffer.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
}

const pythonHexDigits = "0123456789abcdef"

// writePythonJSONString は Python の json モジュールと同じルールで文字列をエスケープする。
func writePythonJSONString(buffer *bytes.Buffer, value string) {
	buffer.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			buffer.WriteString("\\\"")
		case '\\':
			buffer.WriteString("\\\\")
		case '\b':
			buffer.WriteString("\\b")
		case '\f':
			buffer.WriteString("\\f")
		case '\n':
			buffer.WriteString("\\n")
		case '\r':
			buffer.WriteString("\\r")
		case '\t':
			buffer.WriteString("\\t")
		default:
			if character < 0x20 {
				buffer.WriteString("\\u00")
				buffer.WriteByte(pythonHexDigits[(character>>4)&0xf])
				buffer.WriteByte(pythonHexDigits[character&0xf])
				continue
			}
			if character == utf8.RuneError {
				// Python の json.dumps(surrogates) 相当。ここでは置換文字をそのまま出力する。
				buffer.WriteRune(character)
				continue
			}
			buffer.WriteRune(character)
		}
	}
	buffer.WriteByte('"')
}
