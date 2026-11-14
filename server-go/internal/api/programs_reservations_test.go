package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// timetableEDCB は録画中の予約だけファイルパスを返す読み取り専用の代役。
type timetableEDCB struct {
	fakeEDCBClient
	checked []int
}

func (f *timetableEDCB) GetRecFilePath(id int) *string {
	f.checked = append(f.checked, id)
	if id == 1 {
		path := "recording.ts"
		return &path
	}
	return nil
}

// TestProgramTimeTableReservations は EID 直接照合と時間照合を実際の HTTP 応答で確認する。
func TestProgramTimeTableReservations(t *testing.T) {
	s, _ := newTestServer(t, "")
	now := timeTableTestSetup(t, s)
	s.config.General.Backend = "EDCB"
	fake := &timetableEDCB{fakeEDCBClient: fakeEDCBClient{enumReserveOK: true}}
	fake.enumReserveData = []reservations.ReserveData{
		{ReserveID: 1, Onid: 32736, Tsid: 32736, Sid: 1024, Eid: 1, StartTime: now.Add(10 * time.Minute), DurationSecond: 1800, OverlapMode: 1},
		{ReserveID: 2, Onid: 32736, Tsid: 32736, Sid: 1025, Eid: 99, StartTime: now.Add(50 * time.Minute), DurationSecond: 1800, OverlapMode: 2, RecSetting: reservations.RecSettingData{RecMode: 5}},
		{ReserveID: 3, Onid: 4, Tsid: 1, Sid: 101, Eid: 1, StartTime: now.Add(24 * time.Hour), DurationSecond: 1800},
	}
	restore := setEDCBClientFactory(fake)
	defer restore()
	response := doJSONRequest(t, s.Handler(), http.MethodGet, "/api/programs/timetable", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var table timeTableResponse
	if err := json.Unmarshal(response.Body.Bytes(), &table); err != nil {
		t.Fatal(err)
	}
	main := table.Channels[0].Programs[0].Reservation
	if main == nil || main.ID != 1 || main.Status != "Recording" || main.RecordingAvailability != "Partial" {
		t.Fatalf("main reservation=%+v", main)
	}
	sub := table.Channels[0].Subchannels[0].Programs[0].Reservation
	if sub == nil || sub.ID != 2 || sub.Status != "Disabled" || sub.RecordingAvailability != "Unavailable" {
		t.Fatalf("fallback reservation=%+v", sub)
	}
	if len(fake.checked) != 1 || fake.checked[0] != 1 {
		t.Fatalf("disabled/distant reservations must not query recording state: %v", fake.checked)
	}
}

// TestTimeTableReservationFallback は TSID の取り違えと暫定予約の重複割当を防ぐ。
func TestTimeTableReservationFallback(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "EDCB"
	now := time.Now()
	tsid := 100
	channels := []database.Channel{{ID: "channel", NetworkID: 1, ServiceID: 2, TransportStreamID: &tsid}}
	fake := &fakeEDCBClient{enumReserveOK: true, enumReserveData: []reservations.ReserveData{
		{ReserveID: 1, Onid: 1, Tsid: 200, Sid: 2, StartTime: now, DurationSecond: 100},
		{ReserveID: 2, Onid: 1, Tsid: 100, Sid: 2, StartTime: now, DurationSecond: 60},
		{ReserveID: 3, Onid: 1, Tsid: 100, Sid: 2, StartTime: now, DurationSecond: 100},
	}}
	restore := setEDCBClientFactory(fake)
	defer restore()
	_, byTime := s.timeTableReservations(channels, now)
	for _, want := range []int{3, 2, 0} {
		got := matchTimeTableReservation(byTime, "channel", database.FormatJSONTime(now), database.FormatJSONTime(now.Add(90*time.Second)))
		if want == 0 {
			if got != nil {
				t.Fatalf("reservation reused: %+v", got)
			}
		} else if got == nil || got.ID != want {
			t.Fatalf("reservation=%+v want=%d", got, want)
		}
	}
}

// TestProgramTimeTableEDCBUnavailable は予約取得失敗時も番組表を表示できることを確認する。
func TestProgramTimeTableEDCBUnavailable(t *testing.T) {
	s, _ := newTestServer(t, "")
	timeTableTestSetup(t, s)
	s.config.General.Backend = "EDCB"
	restore := setEDCBClientFactory(&fakeEDCBClient{})
	defer restore()
	response := doJSONRequest(t, s.Handler(), http.MethodGet, "/api/programs/timetable", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
