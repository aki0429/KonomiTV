package reservations

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestProgramDetailContinuationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/program_detail_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Description string
			Text              string `json:"extended_text"`
			EDCB              struct {
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
			description := tc.Description
			detail := ParseProgramExtendedText(tc.Text).Normalized(&description)
			actual := make([][2]string, 0, detail.Len())
			for _, entry := range detail.Entries() {
				actual = append(actual, [2]string{entry.Heading, entry.Body})
			}
			if !reflect.DeepEqual(actual, tc.EDCB.Detail) || description != tc.EDCB.Description {
				t.Fatalf("Go=%v / %q; upstream=%v / %q", actual, description, tc.EDCB.Detail, tc.EDCB.Description)
			}
		})
	}
}
