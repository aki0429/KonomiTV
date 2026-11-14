package api

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

func TestSearchProgramDetailContinuationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../reservations/testdata/program_detail_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Description string
			Text              string `json:"extended_text"`
			API               struct {
				Detail      [][2]string
				Description string
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			program := decodeSearchEvent(reservations.EventInfo{ShortInfo: &reservations.ShortEventInfo{EventName: "title", TextChar: tc.Description}, ExtInfo: &reservations.ExtendedEventInfo{TextChar: tc.Text}})
			// dict の順序も比較し、同名見出しのタブや概要の補完位置まで本家に揃える。
			expected := []byte{'{'}
			for i, entry := range tc.API.Detail {
				if i > 0 {
					expected = append(expected, ',')
				}
				key, _ := json.Marshal(entry[0])
				body, _ := json.Marshal(entry[1])
				expected = append(expected, key...)
				expected = append(expected, ':')
				expected = append(expected, body...)
			}
			expected = append(expected, '}')
			if string(program.Detail) != string(expected) || program.Description != tc.API.Description {
				t.Fatalf("Go=%s / %q; upstream=%s / %q", program.Detail, program.Description, expected, tc.API.Description)
			}
		})
	}
}
