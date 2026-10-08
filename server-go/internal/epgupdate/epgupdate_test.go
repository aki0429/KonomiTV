package epgupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// edcbFixture は tools/generate_epgupdate_fixture.py が Python 版を実行して生成したオラクル。
type edcbFixture struct {
	Now           string            `json:"now"`
	SeedSQL       []string          `json:"seed_sql"`
	ChSet5        string            `json:"chset5"`
	EnumService   []fixtureService  `json:"enum_service"`
	ServiceEvents []fixtureSvcEvent `json:"service_events"`
	Cases         []struct {
		Name            string                  `json:"name"`
		PreferredRegion *string                 `json:"preferred_terrestrial_region"`
		Schema          []string                `json:"schema"`
		Expected        map[string]fixtureTable `json:"expected"`
	} `json:"cases"`
}

type fixtureTable struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

type fixtureService struct {
	Onid               int    `json:"onid"`
	Tsid               int    `json:"tsid"`
	Sid                int    `json:"sid"`
	ServiceType        int    `json:"service_type"`
	PartialReception   int    `json:"partial_reception_flag"`
	ServiceProvider    string `json:"service_provider_name"`
	ServiceName        string `json:"service_name"`
	NetworkName        string `json:"network_name"`
	TsName             string `json:"ts_name"`
	RemoteControlKeyID int    `json:"remote_control_key_id"`
}

func (s fixtureService) toInfo() reservations.ServiceInfo {
	return reservations.ServiceInfo{Onid: s.Onid, Tsid: s.Tsid, Sid: s.Sid, ServiceType: s.ServiceType,
		PartialReceptionFlag: s.PartialReception, ServiceProviderName: s.ServiceProvider, ServiceName: s.ServiceName,
		NetworkName: s.NetworkName, TsName: s.TsName, RemoteControlKeyID: s.RemoteControlKeyID}
}

type fixtureSvcEvent struct {
	ServiceInfo fixtureService `json:"service_info"`
	EventList   []struct {
		Onid       int     `json:"onid"`
		Tsid       int     `json:"tsid"`
		Sid        int     `json:"sid"`
		Eid        int     `json:"eid"`
		FreeCaFlag int     `json:"free_ca_flag"`
		StartTime  *string `json:"start_time"`
		Duration   *int    `json:"duration_sec"`
		ShortInfo  *struct {
			EventName string `json:"event_name"`
			TextChar  string `json:"text_char"`
		} `json:"short_info"`
		ExtInfo *struct {
			TextChar string `json:"text_char"`
		} `json:"ext_info"`
		ContentInfo *struct {
			NibbleList []struct {
				ContentNibble int `json:"content_nibble"`
				UserNibble    int `json:"user_nibble"`
			} `json:"nibble_list"`
		} `json:"content_info"`
		ComponentInfo *struct {
			StreamContent int    `json:"stream_content"`
			ComponentType int    `json:"component_type"`
			ComponentTag  int    `json:"component_tag"`
			TextChar      string `json:"text_char"`
		} `json:"component_info"`
		AudioInfo *struct {
			ComponentList []struct {
				StreamContent      int    `json:"stream_content"`
				ComponentType      int    `json:"component_type"`
				ComponentTag       int    `json:"component_tag"`
				StreamType         int    `json:"stream_type"`
				SimulcastGroupTag  int    `json:"simulcast_group_tag"`
				EsMultiLingualFlag int    `json:"es_multi_lingual_flag"`
				MainComponentFlag  int    `json:"main_component_flag"`
				QualityIndicator   int    `json:"quality_indicator"`
				SamplingRate       int    `json:"sampling_rate"`
				TextChar           string `json:"text_char"`
			} `json:"component_list"`
		} `json:"audio_info"`
		EventGroupInfo *struct {
			GroupType     int `json:"group_type"`
			EventDataList []struct {
				Onid int `json:"onid"`
				Tsid int `json:"tsid"`
				Sid  int `json:"sid"`
				Eid  int `json:"eid"`
			} `json:"event_data_list"`
		} `json:"event_group_info"`
	} `json:"event_list"`
}

// fixtureEDCB は固定データを返す EDCBSource 。
type fixtureEDCB struct {
	fixture     *edcbFixture
	serviceList []int64
}

func (f *fixtureEDCB) FileCopy(name string) ([]byte, bool) {
	if name != "ChSet5.txt" {
		return nil, false
	}
	return []byte(f.fixture.ChSet5), true
}

func (f *fixtureEDCB) EnumService() ([]reservations.ServiceInfo, bool) {
	result := []reservations.ServiceInfo{}
	for _, service := range f.fixture.EnumService {
		result = append(result, service.toInfo())
	}
	return result, true
}

func (f *fixtureEDCB) EnumPgInfoEx(list []int64) ([]reservations.ServiceEventInfo, bool) {
	f.serviceList = list
	var result []reservations.ServiceEventInfo
	for _, service := range f.fixture.ServiceEvents {
		converted := reservations.ServiceEventInfo{ServiceInfo: service.ServiceInfo.toInfo()}
		for _, source := range service.EventList {
			event := reservations.EventInfo{Onid: source.Onid, Tsid: source.Tsid, Sid: source.Sid, Eid: source.Eid,
				FreeCaFlag: source.FreeCaFlag, DurationSec: source.Duration}
			if source.StartTime != nil {
				start, err := time.Parse(time.RFC3339, *source.StartTime)
				if err != nil {
					panic(err)
				}
				event.StartTime = &start
			}
			if source.ShortInfo != nil {
				event.ShortInfo = &reservations.ShortEventInfo{EventName: source.ShortInfo.EventName, TextChar: source.ShortInfo.TextChar}
			}
			if source.ExtInfo != nil {
				event.ExtInfo = &reservations.ExtendedEventInfo{TextChar: source.ExtInfo.TextChar}
			}
			if source.ContentInfo != nil {
				content := &reservations.ContentInfo{}
				for _, nibble := range source.ContentInfo.NibbleList {
					content.NibbleList = append(content.NibbleList, reservations.ContentData{ContentNibble: nibble.ContentNibble, UserNibble: nibble.UserNibble})
				}
				event.ContentInfo = content
			}
			if c := source.ComponentInfo; c != nil {
				event.ComponentInfo = &reservations.ComponentInfo{StreamContent: c.StreamContent, ComponentType: c.ComponentType, ComponentTag: c.ComponentTag, TextChar: c.TextChar}
			}
			if a := source.AudioInfo; a != nil {
				audio := &reservations.AudioComponentInfo{ComponentList: []reservations.AudioComponentInfoData{}}
				for _, item := range a.ComponentList {
					audio.ComponentList = append(audio.ComponentList, reservations.AudioComponentInfoData{StreamContent: item.StreamContent,
						ComponentType: item.ComponentType, ComponentTag: item.ComponentTag, StreamType: item.StreamType,
						SimulcastGroupTag: item.SimulcastGroupTag, EsMultiLingualFlag: item.EsMultiLingualFlag,
						MainComponentFlag: item.MainComponentFlag, QualityIndicator: item.QualityIndicator,
						SamplingRate: item.SamplingRate, TextChar: item.TextChar})
				}
				event.AudioInfo = audio
			}
			if g := source.EventGroupInfo; g != nil {
				group := &reservations.EventGroupInfo{GroupType: g.GroupType}
				for _, data := range g.EventDataList {
					group.EventDataList = append(group.EventDataList, reservations.EventData{Onid: data.Onid, Tsid: data.Tsid, Sid: data.Sid, Eid: data.Eid})
				}
				event.EventGroupInfo = group
			}
			converted.EventList = append(converted.EventList, event)
		}
		result = append(result, converted)
	}
	return result, true
}

func loadFixture(t *testing.T) *edcbFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "edcb_update_fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture edcbFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return &fixture
}

func openFixtureDB(t *testing.T, schema []string, seed []string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range append(append([]string{}, schema...), seed...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%v: %s", err, statement)
		}
	}
	return db
}

// normalize は SQLite の値を JSON 由来の値と比較できる形にそろえる。
func normalize(value any) any {
	switch v := value.(type) {
	case int64:
		return float64(v)
	case []byte:
		return string(v)
	}
	return value
}

func dumpTable(t *testing.T, db *sql.DB, table string, columns []string) [][]any {
	t.Helper()
	query := "SELECT "
	for index, column := range columns {
		if index != 0 {
			query += ", "
		}
		// TIMESTAMP 列はドライバーが time.Time へ変換するため、保存された文字列そのものを比較する
		if strings.HasSuffix(column, "_time") {
			query += "CAST(" + column + " AS TEXT)"
		} else {
			query += column
		}
	}
	rows, err := db.Query(query + " FROM " + table + " ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for index := range values {
			values[index] = normalize(values[index])
		}
		result = append(result, values)
	}
	return result
}

// TestEDCBUpdateMatchesPython は Python 版の Channel.updateFromEDCB() → Program.updateFromEDCB() と
// 同じ入力から、channels / programs テーブルが全列一致することを検証する。
func TestEDCBUpdateMatchesPython(t *testing.T) {
	fixture := loadFixture(t)
	now, err := time.Parse(time.RFC3339, fixture.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := openFixtureDB(t, c.Schema, fixture.SeedSQL)
			source := &fixtureEDCB{fixture: fixture}
			if err := UpdateChannelsFromEDCB(context.Background(), db, source, c.PreferredRegion, logger); err != nil {
				t.Fatalf("channels: %v", err)
			}
			if err := UpdateProgramsFromEDCB(context.Background(), db, source, now, logger); err != nil {
				t.Fatalf("programs: %v", err)
			}
			if !reflect.DeepEqual(source.serviceList, allProgramsServiceTimeList) {
				t.Errorf("EnumPgInfoEx args = %v", source.serviceList)
			}
			for _, table := range []string{"channels", "programs"} {
				expected := c.Expected[table]
				actual := dumpTable(t, db, table, expected.Columns)
				if len(actual) != len(expected.Rows) {
					t.Errorf("%s: %d rows, want %d", table, len(actual), len(expected.Rows))
				}
				for index := 0; index < len(actual) && index < len(expected.Rows); index++ {
					if !reflect.DeepEqual(actual[index], expected.Rows[index]) {
						t.Errorf("%s row %d:\n got  %s\n want %s", table, index, fmt.Sprint(actual[index]), fmt.Sprint(expected.Rows[index]))
					}
				}
			}
		})
	}
}

// TestEDCBUpdateChSet5FailureRollsBack は ChSet5.txt を取得できない場合に何も変更しないことを検証する。
func TestEDCBUpdateChSet5FailureRollsBack(t *testing.T) {
	fixture := loadFixture(t)
	db := openFixtureDB(t, fixture.Cases[0].Schema, fixture.SeedSQL)
	before := dumpTable(t, db, "channels", fixture.Cases[0].Expected["channels"].Columns)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := UpdateChannelsFromEDCB(context.Background(), db, failingEDCB{}, nil, logger)
	if err != ErrChSet5Unavailable {
		t.Fatalf("err = %v", err)
	}
	if after := dumpTable(t, db, "channels", fixture.Cases[0].Expected["channels"].Columns); !reflect.DeepEqual(before, after) {
		t.Fatal("channels changed after failure")
	}
	if err := UpdateProgramsFromEDCB(context.Background(), db, failingEDCB{}, time.Now(), logger); err != ErrProgramsUnavailable {
		t.Fatalf("programs err = %v", err)
	}
}

type failingEDCB struct{}

func (failingEDCB) FileCopy(string) ([]byte, bool)                  { return nil, false }
func (failingEDCB) EnumService() ([]reservations.ServiceInfo, bool) { return nil, false }
func (failingEDCB) EnumPgInfoEx([]int64) ([]reservations.ServiceEventInfo, bool) {
	return nil, false
}
