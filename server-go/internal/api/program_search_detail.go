package api

import (
	"encoding/json"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// orderedProgramDetail は Python dict の挿入順・同名上書き・JSON object 出力を保持する。
// programResponse は既存の RawMessage のままなので DB / 番組表 consumer を変更しない。
type orderedProgramDetail struct {
	entries   []programDetailEntry
	positions map[string]int
}

// programDetailEntry は順序つき詳細の見出しと本文。
type programDetailEntry struct{ heading, body string }

func (detail *orderedProgramDetail) contains(heading string) bool {
	_, exists := detail.positions[heading]
	return exists
}

// set は同名キーの上書きでも元の挿入位置を維持する。
func (detail *orderedProgramDetail) set(heading, body string) {
	if detail.positions == nil {
		detail.positions = make(map[string]int)
	}
	if index, exists := detail.positions[heading]; exists {
		detail.entries[index].body = body
		return
	}
	detail.positions[heading] = len(detail.entries)
	detail.entries = append(detail.entries, programDetailEntry{heading, body})
}

// parse は EDCBUtil.parseProgramExtendedText の raw 見出し重複も正確に保存する。
func (detail *orderedProgramDetail) parse(text string) {
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
}

// normalized は ProgramsRouter の整形と二段階目の重複処理を適用する。
func (detail orderedProgramDetail) normalized() orderedProgramDetail {
	var result orderedProgramDetail
	for _, entry := range detail.entries {
		head := strings.Trim(strings.ReplaceAll(reservations.FormatString(entry.heading), "◇", ""), " \r\n")
		for result.contains(head) {
			head += "\t"
		}
		// Python は fallback の後では重複判定せず、既存「番組内容」を上書きする場合がある。
		if head == "" {
			head = "番組内容"
		}
		result.set(head, strings.TrimSpace(reservations.FormatString(entry.body)))
	}
	return result
}

// MarshalJSON はソートせず挿入順に JSON object を生成する。
func (detail orderedProgramDetail) MarshalJSON() ([]byte, error) {
	buffer := []byte{'{'}
	for index, entry := range detail.entries {
		if index != 0 {
			buffer = append(buffer, ',')
		}
		key, err := json.Marshal(entry.heading)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(entry.body)
		if err != nil {
			return nil, err
		}
		buffer = append(buffer, key...)
		buffer = append(buffer, ':')
		buffer = append(buffer, body...)
	}
	return append(buffer, '}'), nil
}
