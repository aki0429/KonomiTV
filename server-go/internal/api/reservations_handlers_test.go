package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは予約系 3 ルーターの HTTP ハンドラ (合計 11 エンドポイント) を検証する。
//
// 実機の EDCB には接続しない: edcbClientFactory を差し替えて fakeEDCBClient を返す。
// 期待するレスポンスは Python 実装 (FastAPI) と同じ形式・同じエラー文言に合わせている。

// newReservationTestMux は予約系ルーターだけを登録した mux を生成する。
//
// server.go への登録は親が行うため、ここではテスト対象の登録関数だけを呼ぶ。
func newReservationTestMux(server *Server) *http.ServeMux {
	mux := http.NewServeMux()
	server.registerReservationRoutes(mux)
	server.registerReservationConditionRoutes(mux)
	server.registerRecordingPresetRoutes(mux)
	return mux
}

// setEDCBClientFactory は EDCB クライアントの生成をテスト用に差し替える。
//
// 返り値は元に戻すための関数。
func setEDCBClientFactory(client edcbClient) func() {
	original := edcbClientFactory
	edcbClientFactory = func(cfg *config.Config) (edcbClient, error) {
		return client, nil
	}
	return func() { edcbClientFactory = original }
}

// reservationRouteCases は予約系 3 ルーターの全 11 エンドポイント。
var reservationRouteCases = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/recording/reservations"},
	{http.MethodPost, "/api/recording/reservations"},
	{http.MethodGet, "/api/recording/reservations/1"},
	{http.MethodPut, "/api/recording/reservations/1"},
	{http.MethodDelete, "/api/recording/reservations/1"},
	{http.MethodGet, "/api/recording/conditions"},
	{http.MethodPost, "/api/recording/conditions"},
	{http.MethodGet, "/api/recording/conditions/1"},
	{http.MethodPut, "/api/recording/conditions/1"},
	{http.MethodDelete, "/api/recording/conditions/1"},
	{http.MethodGet, "/api/recording/presets"},
}

// TestReservationRoutesBackendNotEDCB はバックエンドが EDCB でない場合の挙動を検証する。
//
// Python 版 (ReservationsRouter.GetCtrlCmdUtil) と同じく 422 と
// 'This API is only available when the backend is EDCB' を返すこと。
func TestReservationRoutesBackendNotEDCB(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "Mirakurun" // テスト中に実機の EDCB へ接続しないバックエンド
	mux := newReservationTestMux(server)

	for _, testCase := range reservationRouteCases {
		path := testCase.path
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(testCase.method, path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s %s: status = %d, want 422", testCase.method, testCase.path, recorder.Code)
			continue
		}
		body := strings.TrimSpace(recorder.Body.String())
		expected := `{"detail":"This API is only available when the backend is EDCB"}`
		if body != expected {
			t.Errorf("%s %s: body = %s, want %s", testCase.method, testCase.path, body, expected)
		}
	}
}

// TestReservationRoutesEmptyResponses は EDCB から取得できなかった場合の一覧 API の挙動を検証する。
//
// Python 版と同じく空のリストを 200 で返すこと。
func TestReservationRoutesEmptyResponses(t *testing.T) {
	server, _ := newTestServer(t, "")

	// EnumReserve / EnumAutoAdd が失敗 (None) するケース
	client := &fakeEDCBClient{}
	restore := setEDCBClientFactory(client)
	defer restore()

	mux := newReservationTestMux(server)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/recording/reservations", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/recording/reservations: status = %d, want 200", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body != `{"total":0,"reservations":[]}` {
		t.Errorf("GET /api/recording/reservations: body = %s", body)
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/recording/conditions", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/recording/conditions: status = %d, want 200", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body != `{"total":0,"reservation_conditions":[]}` {
		t.Errorf("GET /api/recording/conditions: body = %s", body)
	}

	// EDCB から取得できなかった場合 (None) は Python 版と同じく 500 になる
	listFailureCases := []struct {
		path   string
		detail string
	}{
		{"/api/recording/reservations/999", "Failed to get the list of recording reservations"},
		{"/api/recording/conditions/999", "Failed to get the list of reserve conditions"},
	}
	for _, testCase := range listFailureCases {
		recorder = httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("GET %s: status = %d, want 500", testCase.path, recorder.Code)
			continue
		}
		expected := `{"detail":"` + testCase.detail + `"}`
		if body := strings.TrimSpace(recorder.Body.String()); body != expected {
			t.Errorf("GET %s: body = %s, want %s", testCase.path, body, expected)
		}
	}
}

// TestReservationNotFound は指定された ID が見つからない場合の挙動を検証する。
//
// Python 版と同じく 422 と 'Specified ... was not found' を返すこと。
func TestReservationNotFound(t *testing.T) {
	server, _ := newTestServer(t, "")

	// 一覧の取得自体は成功するが、該当 ID が存在しないケース
	client := &fakeEDCBClient{
		enumReserveOK:   true,
		enumReserveData: []reservations.ReserveData{},
		enumAutoAddOK:   true,
		enumAutoAddData: []reservations.AutoAddData{},
	}
	restore := setEDCBClientFactory(client)
	defer restore()

	mux := newReservationTestMux(server)

	notFoundCases := []struct {
		path   string
		detail string
	}{
		{"/api/recording/reservations/999", "Specified reservation_id was not found"},
		{"/api/recording/conditions/999", "Specified reservation_condition_id was not found"},
		{"/api/recording/reservations/999", "Specified reservation_id was not found"},
	}
	for index, testCase := range notFoundCases {
		method := http.MethodGet
		if index == 2 {
			method = http.MethodDelete
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, testCase.path, nil))
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s %s: status = %d, want 422", method, testCase.path, recorder.Code)
			continue
		}
		expected := `{"detail":"` + testCase.detail + `"}`
		if body := strings.TrimSpace(recorder.Body.String()); body != expected {
			t.Errorf("%s %s: body = %s, want %s", method, testCase.path, body, expected)
		}
	}
}

// TestRecordingPresetsEndpoint は録画設定プリセット一覧 API のレスポンスを検証する。
//
// EDCB から取得した EpgTimerSrv.ini を解析した結果が、Python 版の解析結果
// (parse_global_defaults / parse_preset_*) と一致すること。
// SET セクションの PresetID=1,2 のため、ID=0 のデフォルト + ID=1 のプリセットが返り、
// 解析に失敗する ID=2 はスキップされる (Python 版と同じ挙動) 。
func TestRecordingPresetsEndpoint(t *testing.T) {
	server, _ := newTestServer(t, "")

	client := &fakeEDCBClient{
		fileCopy2OK: true,
		fileCopy2Files: map[string][]byte{
			"EpgTimerSrv.ini": []byte(epgTimerSrvIniFixture),
		},
	}
	restore := setEDCBClientFactory(client)
	defer restore()

	mux := newReservationTestMux(server)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/recording/presets", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/recording/presets: status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}

	expected := `{"global_defaults":` + pythonReservationFixtures["parse_global_defaults"] +
		`,"presets":[` + pythonReservationFixtures["parse_preset_0"] + `,` +
		pythonReservationFixtures["parse_preset_1"] + `]}`
	if body := strings.TrimSpace(recorder.Body.String()); body != expected {
		t.Errorf("GET /api/recording/presets が Python 実装と一致しない\n期待: %s\n実際: %s", expected, body)
	}
}

// TestRecordingPresetsEndpointWithoutIni は EpgTimerSrv.ini が取得できなかった場合の挙動を検証する。
//
// Python 版と同じく 500 と 'Failed to get EpgTimerSrv.ini from EDCB' を返すこと。
func TestRecordingPresetsEndpointWithoutIni(t *testing.T) {
	server, _ := newTestServer(t, "")

	client := &fakeEDCBClient{} // FileCopy2 が失敗する
	restore := setEDCBClientFactory(client)
	defer restore()

	mux := newReservationTestMux(server)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/recording/presets", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	expected := `{"detail":"Failed to get EpgTimerSrv.ini from EDCB"}`
	if body := strings.TrimSpace(recorder.Body.String()); body != expected {
		t.Errorf("body = %s, want %s", body, expected)
	}
}

// TestReservationRoutesConcurrentSafety はルート登録とハンドラが並行呼び出しで壊れないことを確認する。
//
// (edcbClientFactory の差し替えはテスト内で直列に行うため、ここでは一覧 API のみを並行で叩く)
func TestReservationRoutesConcurrentSafety(t *testing.T) {
	server, _ := newTestServer(t, "")
	client := &fakeEDCBClient{}
	restore := setEDCBClientFactory(client)
	defer restore()

	mux := newReservationTestMux(server)
	var waitGroup sync.WaitGroup
	for index := 0; index < 8; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/recording/reservations", nil))
			if recorder.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", recorder.Code)
			}
		}()
	}
	waitGroup.Wait()
}
