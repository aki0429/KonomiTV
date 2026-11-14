package reservations

import (
	"encoding/json"
	"strings"
)

// ProgramDetailEntry は番組詳細の見出しと本文。
type ProgramDetailEntry struct {
	Heading string
	Body    string
}

// ProgramDetail は Python dict の挿入順・同名上書きを保持する番組詳細。
type ProgramDetail struct {
	entries   []ProgramDetailEntry
	positions map[string]int
}

// Entries は挿入順の見出しと本文を返す。
func (detail ProgramDetail) Entries() []ProgramDetailEntry {
	return detail.entries
}

// Len は見出しの数 (Python の len(dict)) を返す。
func (detail ProgramDetail) Len() int {
	return len(detail.entries)
}

func (detail *ProgramDetail) contains(heading string) bool {
	_, exists := detail.positions[heading]
	return exists
}

// set は同名キーの上書きでも元の挿入位置を維持する。
func (detail *ProgramDetail) set(heading, body string) {
	if detail.positions == nil {
		detail.positions = make(map[string]int)
	}
	if index, exists := detail.positions[heading]; exists {
		detail.entries[index].Body = body
		return
	}
	detail.positions[heading] = len(detail.entries)
	detail.entries = append(detail.entries, ProgramDetailEntry{heading, body})
}

// ParseProgramExtendedText は EDCBUtil.parseProgramExtendedText と同じく、
// 生の拡張テキストを見出しと本文に分ける (見出しの重複はタブを付けて保持する) 。
func ParseProgramExtendedText(text string) ProgramDetail {
	var detail ProgramDetail
	text = strings.ReplaceAll(text, "\r", "")
	head := ""
	i := 0
	for {
		j := -1
		if i == 0 && strings.HasPrefix(text, "- ") {
			j = 2
		} else {
			if offset := strings.Index(text[i:], "\n- "); offset >= 0 {
				j = i + offset
				for detail.contains(head) {
					head += "\t"
				}
				start := 0
				if i != 0 {
					start = i + 1
				}
				detail.set(head, text[start:j+1])
				j += 3
			} else {
				if len(text) != 0 {
					for detail.contains(head) {
						head += "\t"
					}
					start := 0
					if i != 0 {
						start = i + 1
					}
					detail.set(head, text[start:])
				}
				break
			}
		}
		offset := strings.IndexByte(text[j:], '\n')
		if offset < 0 {
			head = text[j:]
			for detail.contains(head) {
				head += "\t"
			}
			detail.set(head, "")
			break
		}
		i = j + offset
		head = text[j:i]
	}
	return detail
}

// Normalized は Program.updateFromEDCB / DecodeEDCBEventInfo の見出し・本文整形、
// 空見出しの継続、二段階目の重複処理を適用する。description が空なら最初の独立項目で補う。
func (detail ProgramDetail) Normalized(description *string) ProgramDetail {
	var result ProgramDetail
	for _, entry := range detail.entries {
		head := strings.Trim(strings.ReplaceAll(FormatString(entry.Heading), "◇", ""), " \r\n")
		body := FormatProgramText(entry.Body)
		// WOWOW 等の分割本文は直前の辞書項目へ結合する。通常の分割位置には改行を足さない。
		if pyStrip(head) == "" && len(result.entries) > 0 {
			last := &result.entries[len(result.entries)-1]
			if strings.HasPrefix(body, "【") {
				last.Body += "\n"
			}
			last.Body += body
			continue
		}
		// 最初の空見出しだけ固定名にする。その後、整形で衝突した見出しもタブで保持する。
		if pyStrip(head) == "" {
			head = "番組内容"
		}
		for result.contains(head) {
			head += "\t"
		}
		result.set(head, body)
		// 継続項目は概要の補完に使わないため、結合後の本文からの再計算は行わない。
		if description != nil && pyStrip(*description) == "" {
			*description = body
		}
	}
	return result
}

// FormatProgramText は本家の formatString().strip() に合わせ、制御文字の空白も取り除く。
// 見出しの重複を表すタブを残す場合はこの関数を使わず、明示された文字だけを除去する。
func FormatProgramText(text string) string {
	return pyStrip(FormatString(text))
}

// MarshalJSON はソートせず挿入順に JSON object を生成する。
func (detail ProgramDetail) MarshalJSON() ([]byte, error) {
	buffer := []byte{'{'}
	for index, entry := range detail.entries {
		if index != 0 {
			buffer = append(buffer, ',')
		}
		key, err := json.Marshal(entry.Heading)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(entry.Body)
		if err != nil {
			return nil, err
		}
		buffer = append(buffer, key...)
		buffer = append(buffer, ':')
		buffer = append(buffer, body...)
	}
	return append(buffer, '}'), nil
}
