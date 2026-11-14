package epgupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

func TestProgramDetailContinuationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../reservations/testdata/program_detail_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Description string
			Text              string `json:"extended_text"`
			Entries           [][2]string
			EDCB, Mirakurun   struct {
				Detail      [][2]string
				Description string
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile("testdata/mirakurun_update_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var setup mirakurunFixture
	if err := json.Unmarshal(base, &setup); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, setup.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range fixture.Cases {
		t.Run(tc.Name+"/EDCB", func(t *testing.T) {
			program, detail := decodeProgram(reservations.EventInfo{ShortInfo: &reservations.ShortEventInfo{EventName: "title", TextChar: tc.Description}, ExtInfo: &reservations.ExtendedEventInfo{TextChar: tc.Text}})
			actual := make([][2]string, 0, detail.Len())
			for _, entry := range detail.Entries() {
				actual = append(actual, [2]string{entry.Heading, entry.Body})
			}
			if !reflect.DeepEqual(actual, tc.EDCB.Detail) || program.Description != tc.EDCB.Description {
				t.Fatalf("Go=%v / %q; upstream=%v / %q", actual, program.Description, tc.EDCB.Detail, tc.EDCB.Description)
			}
		})
		t.Run(tc.Name+"/Mirakurun", func(t *testing.T) {
			db := openFixtureDB(t, setup.Cases[0].Schema, setup.SeedSQL)
			// 見出しは挿入順で送り、全角整形後の衝突と「最後の辞書キー」への結合も検査する。
			extended := orderedReferenceDetail(tc.Entries)
			program := map[string]any{"id": 327360102400100, "eventId": 100, "serviceId": 1024, "networkId": 32736,
				"startAt": 1791518400000, "duration": 3600000, "isFree": true, "name": "continuation fixture", "description": tc.Description, "extended": extended,
				"audios": []any{map[string]any{"componentType": 3, "samplingRate": 48000, "langs": []string{"jpn"}}}}
			input, err := json.Marshal([]any{program})
			if err != nil {
				t.Fatal(err)
			}
			source := fixtureMirakurun{services: setup.Services, programs: input}
			if err := UpdateChannelsFromMirakurun(context.Background(), db, source, nil, logger); err != nil {
				t.Fatal(err)
			}
			if err := UpdateProgramsFromMirakurun(context.Background(), db, source, now, logger); err != nil {
				t.Fatal(err)
			}
			var detail, description string
			if err := db.QueryRow(`SELECT detail,description FROM programs WHERE event_id=100 AND network_id=32736 AND service_id=1024`).Scan(&detail, &description); err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(detail)); err != nil {
				t.Fatal(err)
			}
			if compact.String() != string(orderedReferenceDetail(tc.Mirakurun.Detail)) || description != tc.Mirakurun.Description {
				t.Fatalf("Go=%s / %q; upstream=%s / %q", compact.String(), description, orderedReferenceDetail(tc.Mirakurun.Detail), tc.Mirakurun.Description)
			}
		})
	}
}

func orderedReferenceDetail(entries [][2]string) json.RawMessage {
	data := []byte{'{'}
	for i, entry := range entries {
		if i > 0 {
			data = append(data, ',')
		}
		key, _ := json.Marshal(entry[0])
		value, _ := json.Marshal(entry[1])
		data = append(data, key...)
		data = append(data, ':')
		data = append(data, value...)
	}
	return append(data, '}')
}
