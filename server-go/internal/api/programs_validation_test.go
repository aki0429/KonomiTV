package api

import (
	"net/http"
	"testing"
)

// assertValidationResponse はバリデーションエラー応答のステータスと本文を完全一致で検証する。
// FastAPI/Pydantic はパスパラメータ → クエリパラメータの宣言順に全エラーを 1 つの配列へまとめ、
// {"detail": [ ... ]} の形で 422 を返す。本文の型・件数・順序まで合わせるため完全一致で比較する。
func assertValidationResponse(t *testing.T, server *Server, method string, path string, wantStatus int, wantBody string) {
	t.Helper()
	recorder := doJSONRequest(t, server.Handler(), method, path, "", "", "")
	if recorder.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d (body: %s)", method, path, recorder.Code, wantStatus, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("%s %s:\n got: %s\nwant: %s", method, path, got, wantBody)
	}
}

// TestProgramTimeTableChannelTypeValidation は channel_type の Literal 検証が
// Python 版 FastAPI/Pydantic と同じ literal_error 配列になることを固定する。
//
// 移植元: ProgramsRouter.TimeTableAPI の
// channel_type: Annotated[Literal['GR','BS','CS','CATV','SKY','BS4K'] | None, Query(...)] = None
// 実機 Python (127.0.0.77:7010) の実測値:
// GET /api/programs/timetable?channel_type=__invalid__ は 422 で
// {"detail":[{"type":"literal_error","loc":["query","channel_type"],...}]} を返す。
// 修正前の Go 版は無効値を無視して 200 (channels: []) を返していた。
func TestProgramTimeTableChannelTypeValidation(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	want := `{"detail":[{"type":"literal_error","loc":["query","channel_type"],` +
		`"msg":"Input should be 'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'","input":"__invalid__",` +
		`"ctx":{"expected":"'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'"}}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?channel_type=__invalid__",
		http.StatusUnprocessableEntity, want)

	// 空文字も「未指定」ではなく許容外の入力として literal_error になる
	// (Python の str は空文字も有効値のため欠落と区別される) 。
	wantEmpty := `{"detail":[{"type":"literal_error","loc":["query","channel_type"],` +
		`"msg":"Input should be 'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'","input":"",` +
		`"ctx":{"expected":"'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'"}}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?channel_type=",
		http.StatusUnprocessableEntity, wantEmpty)
}

// TestProgramTimeTableChannelTypeValid は有効な channel_type の全 Literal 値が
// 検証を通過することを検証する (負の対照: 有効値は 422 にならない) 。
func TestProgramTimeTableChannelTypeValid(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	for _, channelType := range []string{"GR", "BS", "CS", "CATV", "SKY", "BS4K"} {
		recorder := doJSONRequest(t, server.Handler(), http.MethodGet,
			"/api/programs/timetable?channel_type="+channelType, "", "", "")
		if recorder.Code != http.StatusOK {
			t.Errorf("channel_type=%s: status = %d, want 200 (body: %s)", channelType, recorder.Code, recorder.Body.String())
		}
	}
}
