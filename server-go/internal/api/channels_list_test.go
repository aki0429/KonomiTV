package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
)

// TestChannelsList はチャンネル一覧 API の基本的な挙動を検証する。
func TestChannelsList(t *testing.T) {
	server, _ := newTestServer(t, "")
	now := time.Now()

	// 地デジ (GR) 2 局 + BS + CS
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32737-SID1025", DisplayChannelID: "gr021", NetworkID: 32737, ServiceID: 1025,
		RemoconID: 2, ChannelNumber: "021", Type: "GR", Name: "NHK Eテレ1・東京", IsWatchable: true,
	})
	insertTestChannel(t, server.db, testChannel{
		ID: "NID40071-SID101", DisplayChannelID: "bs101", NetworkID: 40071, ServiceID: 101,
		RemoconID: 0, ChannelNumber: "BS101", Type: "BS", Name: "NHK BS", IsWatchable: true,
	})
	insertTestChannel(t, server.db, testChannel{
		ID: "NID40072-SID201", DisplayChannelID: "cs201", NetworkID: 40072, ServiceID: 201,
		RemoconID: 0, ChannelNumber: "CS201", Type: "CS", Name: "CS放送", IsWatchable: true,
	})
	// 視聴不可のチャンネルは含まれない
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32738-SID1026", DisplayChannelID: "gr031", NetworkID: 32738, ServiceID: 1026,
		RemoconID: 3, ChannelNumber: "031", Type: "GR", Name: "視聴不可チャンネル", IsWatchable: false,
	})

	// 現在放送中の番組と次の番組
	insertTestProgram(t, server.db, "program-gr011-present", "NID32736-SID1024", "放送中の番組",
		now.Add(-30*time.Minute), now.Add(30*time.Minute), 3600)
	insertTestProgram(t, server.db, "program-gr011-following", "NID32736-SID1024", "次の番組",
		now.Add(30*time.Minute), now.Add(90*time.Minute), 3600)
	// 地デジ 2 局目は現在放送中の番組のみ
	insertTestProgram(t, server.db, "program-gr021-present", "NID32737-SID1025", "Eテレの放送中の番組",
		now.Add(-10*time.Minute), now.Add(50*time.Minute), 3600)
	// BS は 24 時間以内に放送開始予定の番組のみ (放送休止中)
	insertTestProgram(t, server.db, "program-bs101-following", "NID40071-SID101", "BSの次の番組",
		now.Add(2*time.Hour), now.Add(4*time.Hour), 7200)
	// CS は 24 時間より先の番組しかない (番組情報なし扱い)
	insertTestProgram(t, server.db, "program-cs201-later", "NID40072-SID201", "CSの遠い番組",
		now.Add(30*time.Hour), now.Add(32*time.Hour), 7200)

	handler := server.Handler()
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response liveChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}

	// チャンネルタイプごとに分類される (空のタイプもキー自体は存在する)
	if len(response.GR) != 2 || len(response.BS) != 1 || len(response.CS) != 1 {
		t.Fatalf("GR/BS/CS = %d/%d/%d", len(response.GR), len(response.BS), len(response.CS))
	}
	if response.CATV == nil || response.SKY == nil || response.BS4K == nil || response.IPTV == nil {
		t.Error("empty channel types should be empty arrays")
	}
	if !strings.Contains(recorder.Body.String(), `"CATV":[]`) || !strings.Contains(recorder.Body.String(), `"IPTV":[]`) {
		t.Errorf("body = %s", recorder.Body.String())
	}

	// ***** 地デジ 1 局目 (現在 + 次の番組) *****
	gr := response.GR[0]
	if gr.ID != "NID32736-SID1024" || gr.DisplayChannelID != "gr011" || gr.Type != "GR" {
		t.Errorf("GR[0] = %+v", gr)
	}
	if gr.ProgramPresent == nil || gr.ProgramPresent.Title != "放送中の番組" {
		t.Fatalf("program_present = %+v", gr.ProgramPresent)
	}
	if gr.ProgramFollowing == nil || gr.ProgramFollowing.Title != "次の番組" {
		t.Fatalf("program_following = %+v", gr.ProgramFollowing)
	}
	if len(gr.TerrestrialRegions) == 0 {
		t.Errorf("terrestrial_regions = %v", gr.TerrestrialRegions)
	}
	// CSV の JSON フィールドはデコードされて返る
	if string(gr.ProgramPresent.Detail) != `{"テスト":"値"}` {
		t.Errorf("detail = %s", gr.ProgramPresent.Detail)
	}
	if string(gr.ProgramPresent.Genres) != `[{"major":"ニュース／報道","middle":"国内"}]` {
		t.Errorf("genres = %s", gr.ProgramPresent.Genres)
	}
	if !gr.ProgramPresent.IsFree || gr.ProgramPresent.Duration != 3600.0 {
		t.Errorf("program = %+v", gr.ProgramPresent)
	}
	if gr.ViewerCount != 0 || !gr.IsWatchable || !gr.IsDisplay {
		t.Errorf("GR[0] = %+v", gr)
	}
	if gr.ProgramPresent.StartTime == "" || gr.ProgramPresent.StartTime[len(gr.ProgramPresent.StartTime)-6:] != "+09:00" {
		t.Errorf("start_time = %s", gr.ProgramPresent.StartTime)
	}

	// ***** 地デジ 2 局目 (現在放送中の番組のみ) *****
	gr2 := response.GR[1]
	if gr2.ID != "NID32737-SID1025" {
		t.Fatalf("GR[1] = %+v", gr2)
	}
	if gr2.ProgramPresent == nil || gr2.ProgramPresent.Title != "Eテレの放送中の番組" {
		t.Errorf("program_present = %+v", gr2.ProgramPresent)
	}
	if gr2.ProgramFollowing != nil {
		t.Errorf("program_following = %+v", gr2.ProgramFollowing)
	}

	// ***** BS (24 時間以内に放送開始予定の番組のみ) *****
	bs := response.BS[0]
	if bs.Type != "BS" || bs.ProgramPresent != nil {
		t.Errorf("BS = %+v", bs)
	}
	if bs.ProgramFollowing == nil || bs.ProgramFollowing.Title != "BSの次の番組" {
		t.Errorf("program_following = %+v", bs.ProgramFollowing)
	}
	// 地デジ以外のチャンネルの地域名は null
	if bs.TerrestrialRegions != nil {
		t.Errorf("terrestrial_regions = %v", bs.TerrestrialRegions)
	}

	// ***** CS (24 時間より先の番組しかない) *****
	cs := response.CS[0]
	if cs.ProgramPresent != nil || cs.ProgramFollowing != nil {
		t.Errorf("CS = %+v", cs)
	}
}

// TestChannelsListSubchannel はサブチャンネルの is_display の判定を検証する。
func TestChannelsListSubchannel(t *testing.T) {
	server, _ := newTestServer(t, "")
	now := time.Now()

	// 現在放送中の番組があるサブチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011-1", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011-1", Type: "GR", Name: "サブチャンネル (放送中)",
		IsSubchannel: true, IsWatchable: true,
	})
	// 現在放送中の番組がないサブチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1025", DisplayChannelID: "gr011-2", NetworkID: 32736, ServiceID: 1025,
		RemoconID: 1, ChannelNumber: "011-2", Type: "GR", Name: "サブチャンネル (休止中)",
		IsSubchannel: true, IsWatchable: true,
	})
	// サイマル放送などで使われるラジオチャンネル
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1026", DisplayChannelID: "gr011-3", NetworkID: 32736, ServiceID: 1026,
		RemoconID: 1, ChannelNumber: "011-3", Type: "GR", Name: "ラジオチャンネル",
		IsRadiochannel: true, IsWatchable: true,
	})

	insertTestProgram(t, server.db, "program-sub-present", "NID32736-SID1024", "サブチャンネルの番組",
		now.Add(-5*time.Minute), now.Add(55*time.Minute), 3600)

	recorder := doJSONRequest(t, server.Handler(), http.MethodGet, "/api/channels", "", "", "")
	var response liveChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.GR) != 3 {
		t.Fatalf("GR = %d", len(response.GR))
	}
	// 現在の番組情報が存在しないサブチャンネルは表示されない
	if !response.GR[0].IsDisplay || response.GR[0].IsSubchannel == false {
		t.Errorf("GR[0] = %+v", response.GR[0])
	}
	if response.GR[1].IsDisplay {
		t.Errorf("GR[1] should not be displayed: %+v", response.GR[1])
	}
	// サブチャンネルでないチャンネルは番組情報がなくても表示される
	if !response.GR[2].IsDisplay || !response.GR[2].IsRadiochannel {
		t.Errorf("GR[2] = %+v", response.GR[2])
	}
}

// TestChannelsListIPTV は IPTV の疑似チャンネルの挙動を検証する。
func TestChannelsListIPTV(t *testing.T) {
	server, upstream := setupIPTVTestServer(t)
	handler := server.Handler()

	// IPTV チャンネルの display_channel_id を取得する
	var channels iptvChannelsResponse
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels", "", "", "").Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	alpha := channels.Channels[0]

	// 登録前は IPTV の疑似チャンネルは含まれない
	var response liveChannelsResponse
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels", "", "", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.IPTV) != 0 {
		t.Errorf("IPTV = %+v", response.IPTV)
	}

	// テレビ視聴 UI に登録すると含まれるようになる
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/iptv/tvui",
		`{"display_channel_id": "`+alpha.DisplayChannelID+`"}`, "", "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("register status = %d", recorder.Code)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("cookie should be set")
	}
	cookieHeader := cookies[0].Name + "=" + cookies[0].Value

	request := httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	request.Header.Set("Cookie", cookieHeader)
	recorder2 := httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if err := json.Unmarshal(recorder2.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.IPTV) != 1 {
		t.Fatalf("IPTV = %+v", response.IPTV)
	}
	iptvChannel := response.IPTV[0]
	if iptvChannel.ID != iptv.ChannelIDPrefix+alpha.DisplayChannelID {
		t.Errorf("id = %s", iptvChannel.ID)
	}
	if iptvChannel.DisplayChannelID != alpha.DisplayChannelID || iptvChannel.Type != "IPTV" {
		t.Errorf("channel = %+v", iptvChannel)
	}
	if iptvChannel.ChannelNumber != "JP" || iptvChannel.Name != alpha.Name+" (Japan)" {
		t.Errorf("channel = %+v", iptvChannel)
	}
	if iptvChannel.ViewerCount != 0 || !iptvChannel.IsWatchable || !iptvChannel.IsDisplay {
		t.Errorf("channel = %+v", iptvChannel)
	}
	if iptvChannel.TransportStreamID != nil || iptvChannel.TerrestrialRegions != nil || iptvChannel.JikkyoForce != nil {
		t.Errorf("channel = %+v", iptvChannel)
	}
	if iptvChannel.ProgramPresent != nil || iptvChannel.ProgramFollowing != nil {
		t.Errorf("channel = %+v", iptvChannel)
	}

	// 別の Cookie では登録されていない
	request = httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	recorder2 = httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if err := json.Unmarshal(recorder2.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.IPTV) != 0 {
		t.Errorf("other user IPTV = %+v", response.IPTV)
	}

	// IPTV の疑似チャンネルは DB のチャンネルとは別枠で扱われる
	if len(response.GR) != 0 {
		t.Errorf("GR = %+v", response.GR)
	}

	// プロキシが無効でも IPTV チャンネルは Go 側で解決できる (upstream を止めても一覧は返る)
	upstream.Close()
	request = httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	request.Header.Set("Cookie", cookieHeader)
	recorder2 = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder2, request)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder2.Code)
	}
}

// TestPickPresentAndFollowingPrograms は現在/次の番組の選択ロジックを検証する。
func TestPickPresentAndFollowingPrograms(t *testing.T) {
	present := &testProgramRow{ID: "present"}
	following := &testProgramRow{ID: "following"}

	// 番組情報なし
	if pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(nil); pickedPresent != nil || pickedFollowing != nil {
		t.Error("empty programs should return nil")
	}
	// 現在放送中の番組のみ
	programs := []*databasePresentFollowingProgram{{Program: present.program(), IsPresent: true, ProgramOrder: 1}}
	if pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(programs); pickedPresent == nil || pickedFollowing != nil {
		t.Errorf("present only = %v/%v", pickedPresent, pickedFollowing)
	}
	// 次以降の番組のみ
	programs = []*databasePresentFollowingProgram{{Program: following.program(), IsPresent: false, ProgramOrder: 1}}
	if pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(programs); pickedPresent != nil || pickedFollowing == nil {
		t.Errorf("following only = %v/%v", pickedPresent, pickedFollowing)
	}
	// どちらも現在放送中 (DB に重複した番組がある場合)
	programs = []*databasePresentFollowingProgram{
		{Program: present.program(), IsPresent: true, ProgramOrder: 1},
		{Program: following.program(), IsPresent: true, ProgramOrder: 2},
	}
	if pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(programs); pickedPresent == nil || pickedFollowing != nil {
		t.Errorf("both present = %v/%v", pickedPresent, pickedFollowing)
	}
	// どちらも次以降 (放送休止中やサブチャンネル)
	programs = []*databasePresentFollowingProgram{
		{Program: following.program(), IsPresent: false, ProgramOrder: 2},
		{Program: present.program(), IsPresent: false, ProgramOrder: 1},
	}
	pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(programs)
	if pickedPresent != nil || pickedFollowing == nil {
		t.Errorf("both following = %v/%v", pickedPresent, pickedFollowing)
	} else if pickedFollowing.ID != "present" {
		t.Errorf("program_order = 1 should be selected: %s", pickedFollowing.ID)
	}
	// 現在放送中 + 次以降
	programs = []*databasePresentFollowingProgram{
		{Program: following.program(), IsPresent: false, ProgramOrder: 2},
		{Program: present.program(), IsPresent: true, ProgramOrder: 1},
	}
	if pickedPresent, pickedFollowing := pickPresentAndFollowingPrograms(programs); pickedPresent == nil || pickedFollowing == nil {
		t.Errorf("present and following = %v/%v", pickedPresent, pickedFollowing)
	}
}

// testProgramRow はテスト用の番組行。
type testProgramRow struct {
	ID string
}

// program はテスト用の番組を database.Program として返す。
func (row *testProgramRow) program() *database.Program {
	return &database.Program{ID: row.ID}
}

// databasePresentFollowingProgram は database.PresentFollowingProgram の別名 (テストの可読性のため) 。
type databasePresentFollowingProgram = database.PresentFollowingProgram
