package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// updateDatabaseFakeEDCB は ChSet5.txt と番組情報を固定で返す EDCB 。
type updateDatabaseFakeEDCB struct{ programsCalled bool }

func (f *updateDatabaseFakeEDCB) FileCopy(name string) ([]byte, bool) {
	return []byte("ＮＨＫ総合１・東京\t東京\t32736\t32736\t1024\t1\t0\t1\t1\t1\r\n"), name == "ChSet5.txt"
}
func (f *updateDatabaseFakeEDCB) EnumService() ([]reservations.ServiceInfo, bool) {
	return []reservations.ServiceInfo{{Onid: 32736, Tsid: 32736, Sid: 1024, RemoteControlKeyID: 1}}, true
}
func (f *updateDatabaseFakeEDCB) EnumPgInfoEx([]int64) ([]reservations.ServiceEventInfo, bool) {
	f.programsCalled = true
	return []reservations.ServiceEventInfo{}, true
}

// TestUpdateDatabaseNativeEDCB は proxy 無効の EDCB バックエンドで、Python へ転送せず
// Go 版でチャンネル情報・番組情報を更新して 204 を返すことを検証する。
func TestUpdateDatabaseNativeEDCB(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "EDCB"
	fake := &updateDatabaseFakeEDCB{}
	s.edcbUpdateSource = fake
	s.jikkyoStatusFetch = func(context.Context) ([]byte, error) { return nil, errors.New("offline") }
	response := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/maintenance/update-database", "", "", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var displayID, name string
	if err := s.db.QueryRow(`SELECT display_channel_id, name FROM channels WHERE id = 'NID32736-SID1024'`).Scan(&displayID, &name); err != nil {
		t.Fatalf("channel was not created: %v", err)
	}
	if displayID != "gr011" || name != "NHK総合1・東京" {
		t.Errorf("channel = %s %s", displayID, name)
	}
	if !fake.programsCalled {
		t.Error("programs were not updated")
	}
}

// TestUpdateDatabaseEDCBFailureStill204 は EDCB に接続できない場合も Python 版と同じくログのみで 204 を返すことを検証する。
func TestUpdateDatabaseEDCBFailureStill204(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "EDCB"
	s.config.General.EDCBURL = "tcp://127.0.0.1:1/"
	s.jikkyoStatusFetch = func(context.Context) ([]byte, error) { return nil, errors.New("offline") }
	response := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/maintenance/update-database", "", "", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

type updateDatabaseFakeMirakurun struct{ programsCalled bool }

func (f *updateDatabaseFakeMirakurun) Services(context.Context) ([]byte, error) {
	return []byte(`[{"serviceId":1024,"networkId":32736,"name":"ＮＨＫ総合１・東京","type":1,"remoteControlKeyId":1}]`), nil
}
func (f *updateDatabaseFakeMirakurun) Programs(context.Context) ([]byte, error) {
	f.programsCalled = true
	return []byte(`[]`), nil
}

// TestUpdateDatabaseNativeMirakurun は proxy 無効の Mirakurun バックエンドで Go 版が更新して 204 を返すことを検証する。
func TestUpdateDatabaseNativeMirakurun(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "Mirakurun"
	fake := &updateDatabaseFakeMirakurun{}
	s.mirakurunSource = fake
	s.jikkyoStatusFetch = func(context.Context) ([]byte, error) { return nil, errors.New("offline") }
	response := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/maintenance/update-database", "", "", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var displayID string
	var tsid int
	if err := s.db.QueryRow(`SELECT display_channel_id, transport_stream_id FROM channels WHERE id = 'NID32736-SID1024'`).Scan(&displayID, &tsid); err != nil {
		t.Fatalf("channel was not created: %v", err)
	}
	if displayID != "gr011" || tsid != 32736 || !fake.programsCalled {
		t.Errorf("channel = %s %d programs=%v", displayID, tsid, fake.programsCalled)
	}
}
