package epgupdate

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
)

// TestUpdateJikkyoStatus は Channel.updateJikkyoStatus() と同じく、現在時刻を含むスレッドの
// 実況勢いだけを視聴可能なチャンネルへ反映し、取得失敗時は前回の値を使うことを検証する。
func TestUpdateJikkyoStatus(t *testing.T) {
	fixture := loadFixture(t)
	db := openFixtureDB(t, fixture.Cases[0].Schema, []string{
		"INSERT INTO channels VALUES ('NID32736-SID1024','gr011',32736,1024,32736,1,'011','GR','NHK総合',NULL,0,0,1)",
		"INSERT INTO channels VALUES ('NID32737-SID1032','gr021',32737,1032,32737,2,'021','GR','Eテレ',7,0,0,1)",
		"INSERT INTO channels VALUES ('NID4-SID101','bs101',4,101,16625,1,'101','BS','NHK BS',NULL,0,0,0)",
		"INSERT INTO channels VALUES ('NID6-SID161','cs161',6,161,24608,161,'161','CS','QVC',NULL,0,0,1)",
	})
	// server/static/jikkyo_channels.json と同じ形式の最小対応表
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "jikkyo_channels.json"), []byte(`[
		{"jikkyo_id": 1, "network_id": 15, "service_id": "0x0400", "area": "関東広域", "channel_name": "NHK総合・東京"},
		{"jikkyo_id": 2, "network_id": 15, "service_id": "0x0408", "area": "関東広域", "channel_name": "NHKEテレ東京"},
		{"jikkyo_id": 101, "network_id": 4, "service_id": "101", "area": "全国", "channel_name": "NHK BS"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	channelMap, err := jikkyo.LoadChannelMap(staticDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, constants.JST)
	body := `[
		{"id": "jk1", "threads": [
			{"start_at": "2026-10-09T04:00:00+09:00", "end_at": "2026-10-09T11:59:59+09:00", "jikkyo_force": 1},
			{"start_at": "2026-10-09T12:00:00+09:00", "end_at": "2026-10-10T04:00:00+09:00", "jikkyo_force": 42}]},
		{"id": "jk2", "threads": [{"start_at": "2026-10-09T04:00:00", "end_at": "2026-10-10T04:00:00", "jikkyo_force": -1}]},
		{"id": "jk101", "threads": [{"start_at": "2026-10-09T04:00:00+09:00", "end_at": "2026-10-10T04:00:00+09:00", "jikkyo_force": 9}]},
		{"id": "jk999999", "threads": []}
	]`
	statuses := &JikkyoStatuses{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fetch := func(context.Context) ([]byte, error) { return []byte(body), nil }
	if err := UpdateJikkyoStatus(context.Background(), db, channelMap, statuses, fetch, now, logger); err != nil {
		t.Fatal(err)
	}
	want := map[string]*int64{"NID32736-SID1024": ptr(42), "NID32737-SID1032": ptr(7), "NID4-SID101": nil, "NID6-SID161": nil}
	assertForces(t, db, want)

	// 取得に失敗した回は前回の値で更新する (Python のクラス変数キャッシュと同じ)
	if _, err := db.Exec("UPDATE channels SET jikkyo_force = NULL WHERE id = 'NID32736-SID1024'"); err != nil {
		t.Fatal(err)
	}
	failing := func(context.Context) ([]byte, error) { return nil, errors.New("offline") }
	if err := UpdateJikkyoStatus(context.Background(), db, channelMap, statuses, failing, now, logger); err != nil {
		t.Fatal(err)
	}
	assertForces(t, db, want)
}

func ptr(value int64) *int64 { return &value }

func assertForces(t *testing.T, db *sql.DB, want map[string]*int64) {
	t.Helper()
	for id, expected := range want {
		var value sql.NullInt64
		if err := db.QueryRow("SELECT jikkyo_force FROM channels WHERE id = ?", id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if (expected == nil) != !value.Valid || (expected != nil && *expected != value.Int64) {
			t.Errorf("%s jikkyo_force = %v, want %v", id, value, expected)
		}
	}
}
