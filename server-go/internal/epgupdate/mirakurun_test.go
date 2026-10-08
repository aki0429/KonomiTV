package epgupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// mirakurunFixture は tools/generate_epgupdate_mirakurun_fixture.py が Python 版を実行して生成したオラクル。
type mirakurunFixture struct {
	Now      string          `json:"now"`
	SeedSQL  []string        `json:"seed_sql"`
	Services json.RawMessage `json:"services"`
	Programs json.RawMessage `json:"programs"`
	Cases    []struct {
		Name            string                  `json:"name"`
		PreferredRegion *string                 `json:"preferred_terrestrial_region"`
		Schema          []string                `json:"schema"`
		Expected        map[string]fixtureTable `json:"expected"`
	} `json:"cases"`
}

type fixtureMirakurun struct{ services, programs []byte }

func (f fixtureMirakurun) Services(context.Context) ([]byte, error) { return f.services, nil }
func (f fixtureMirakurun) Programs(context.Context) ([]byte, error) { return f.programs, nil }

// TestMirakurunUpdateMatchesPython は Python 版の Channel.updateFromMirakurun() → Program.updateFromMirakurun() と
// 同じ入力から、channels / programs テーブルが全列一致することを検証する。
func TestMirakurunUpdateMatchesPython(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "mirakurun_update_fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture mirakurunFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, fixture.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	source := fixtureMirakurun{services: fixture.Services, programs: fixture.Programs}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := openFixtureDB(t, c.Schema, fixture.SeedSQL)
			if err := UpdateChannelsFromMirakurun(context.Background(), db, source, c.PreferredRegion, logger); err != nil {
				t.Fatalf("channels: %v", err)
			}
			if err := UpdateProgramsFromMirakurun(context.Background(), db, source, now, logger); err != nil {
				t.Fatalf("programs: %v", err)
			}
			for _, table := range []string{"channels", "programs"} {
				expected := c.Expected[table]
				actual := dumpTable(t, db, table, expected.Columns)
				if len(actual) != len(expected.Rows) {
					t.Errorf("%s: %d rows, want %d", table, len(actual), len(expected.Rows))
				}
				for index := 0; index < len(actual) && index < len(expected.Rows); index++ {
					if !reflect.DeepEqual(actual[index], expected.Rows[index]) {
						t.Errorf("%s row %d:\n got  %v\n want %v", table, index, actual[index], expected.Rows[index])
					}
				}
			}
		})
	}
}

// TestMirakurunFailureRollsBack は API の取得失敗・Python で例外になる応答で DB を変更しないことを検証する。
func TestMirakurunFailureRollsBack(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "mirakurun_update_fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture mirakurunFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openFixtureDB(t, fixture.Cases[0].Schema, fixture.SeedSQL)
	columns := fixture.Cases[0].Expected["programs"].Columns
	before := dumpTable(t, db, "programs", columns)
	for name, programs := range map[string]string{
		"unknown streamContent": `[{"eventId":1,"serviceId":1024,"networkId":32736,"startAt":1791560000000,"duration":600000,"isFree":true,"name":"a","video":{"type":"x","resolution":"y","streamContent":99,"componentType":1},"audios":[{"componentType":3,"samplingRate":48000,"langs":["jpn"]}]}]`,
		"no audio":              `[{"eventId":1,"serviceId":1024,"networkId":32736,"startAt":1791560000000,"duration":600000,"isFree":true,"name":"a"}]`,
		"empty audios":          `[{"eventId":1,"serviceId":1024,"networkId":32736,"startAt":1791560000000,"duration":600000,"isFree":true,"name":"a","audios":[]}]`,
	} {
		if err := UpdateProgramsFromMirakurun(context.Background(), db, fixtureMirakurun{programs: []byte(programs)}, time.Now(), logger); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if after := dumpTable(t, db, "programs", columns); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: programs changed after failure", name)
		}
	}
	failing := failingMirakurun{}
	if err := UpdateChannelsFromMirakurun(context.Background(), db, failing, nil, logger); err == nil {
		t.Error("channels: fetch failure accepted")
	}
}

type failingMirakurun struct{}

func (failingMirakurun) Services(context.Context) ([]byte, error) { return nil, errors.New("offline") }
func (failingMirakurun) Programs(context.Context) ([]byte, error) { return nil, errors.New("offline") }

// TestHTTPMirakurunEndpoints は mirakurun_url の末尾スラッシュを除いた URL へ接続し、200 以外を失敗とすることを検証する。
func TestHTTPMirakurunEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/services":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	source := HTTPMirakurun{BaseURL: server.URL + "/", Client: server.Client()}
	if body, err := source.Services(context.Background()); err != nil || string(body) != "[]" {
		t.Fatalf("services = %q %v", body, err)
	}
	if _, err := source.Programs(context.Background()); err == nil {
		t.Fatal("HTTP 503 accepted")
	}
}
