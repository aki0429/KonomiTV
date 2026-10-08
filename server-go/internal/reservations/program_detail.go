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

// Normalized は Program.updateFromEDCB / DecodeEDCBEventInfo の見出し・本文整形と、
// 二段階目の重複処理を適用する。
func (detail ProgramDetail) Normalized() ProgramDetail {
	var result ProgramDetail
	for _, entry := range detail.entries {
		head := strings.Trim(strings.ReplaceAll(FormatString(entry.Heading), "◇", ""), " \r\n")
		for result.contains(head) {
			head += "\t"
		}
		// Python は fallback の後では重複判定せず、既存「番組内容」を上書きする場合がある。
		if head == "" {
			head = "番組内容"
		}
		result.set(head, strings.TrimSpace(FormatString(entry.Body)))
	}
	return result
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
