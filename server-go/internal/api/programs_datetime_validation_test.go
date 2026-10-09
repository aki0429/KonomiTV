package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// urlQueryEscape はクエリ値の生文字列を URL エンコードする (空白は %20) 。
func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}

// jsonQuote はテスト本文に埋め込む文字列を JSON 文字列リテラルへ変換する。
func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// datetimeParsingRejectBody は datetime_from_date_parsing の 422 応答本文を組み立てる。
// 実機 Python (pydantic 2.13.4 / speedate) は日時パースに失敗すると date パースの
// エラー文言を ctx.error に載せ、"Input should be a valid datetime or date, <error>" を返す。
func datetimeParsingRejectBody(loc string, raw string, errMsg string) string {
	return `{"detail":[{"type":"datetime_from_date_parsing","loc":["query",` + jsonQuote(loc) + `],` +
		`"msg":"Input should be a valid datetime or date, ` + errMsg + `","input":` + jsonQuote(raw) + `,` +
		`"ctx":{"error":` + jsonQuote(errMsg) + `}}]}` + "\n"
}

// datetimeTypeRejectBody は datetime_parsing の 422 応答本文を組み立てる。
func datetimeTypeRejectBody(loc string, raw string, errMsg string) string {
	return `{"detail":[{"type":"datetime_parsing","loc":["query",` + jsonQuote(loc) + `],` +
		`"msg":"Input should be a valid datetime, ` + errMsg + `","input":` + jsonQuote(raw) + `,` +
		`"ctx":{"error":` + jsonQuote(errMsg) + `}}]}` + "\n"
}

const (
	errTooShort     = "input is too short"
	errExtraChars   = "unexpected extra characters at the end of the input"
	errDateSep      = "invalid date separator, expected `-`"
	errYear         = "invalid character in year"
	errMonthOut     = "month value is outside expected range of 1-12"
	errDayOut       = "day value is outside expected range"
	errAfter9999    = "dates after 9999 are not supported as unix timestamps"
	errYearZeroHard = "year 0 is out of range"
)

// TestProgramTimeTableStartTimeDatetimeErrors は start_time の datetime 検証エラーが
// Python 版 FastAPI/Pydantic と同じ type/msg/ctx.error/loc になることを完全一致で固定する。
//
// 移植元: ProgramsRouter.TimeTableAPI の
// start_time: Annotated[datetime | None, Query(...)] = None
// 実機 Python (127.0.0.77:7010) の実測値をそのまま期待値にしている。
func TestProgramTimeTableStartTimeDatetimeErrors(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	cases := []struct {
		raw    string
		errMsg string
	}{
		{"abc", errTooShort},
		{"2026-1", errTooShort},
		{"2026-01", errTooShort},
		{"2026-10-9", errTooShort},
		{"2026-13-01", errMonthOut},
		{"2026-00-01", errMonthOut},
		{"2026-02-30", errDayOut},
		{"2026-02-29", errDayOut}, // 2026 年は平年
		{"2026-10-00", errDayOut},
		{"2026-10-09T10", errExtraChars},
		{"2026-10-09T25:00:00", errExtraChars},
		{"2026-10-09T24:00:00", errExtraChars},
		{"2026-10-09T10:00:61", errExtraChars},
		{"2026-10-09T23:59:60", errExtraChars},
		{"2026-10-09T10:00:00+09", errExtraChars},
		{"2026-10-09T10:00:00+25:00", errExtraChars},
		{"2026-10-09T10:00:00 ", errExtraChars},
		{"2026-10-09T", errExtraChars},
		{"20261009T100000", errDateSep},
		{"2026/10/09", errDateSep},
		{"09-10-2026", errYear},
		{" 2026-10-09", errYear},
		{"300000000000000", errAfter9999},
		{"2026-10-09T10:00:00.123456,", errExtraChars},
	}
	for _, tc := range cases {
		assertValidationResponse(t, server, http.MethodGet,
			"/api/programs/timetable?start_time="+urlQueryEscape(tc.raw),
			http.StatusUnprocessableEntity,
			datetimeParsingRejectBody("start_time", tc.raw, tc.errMsg))
	}
}

// TestProgramTimeTableEndTimeDatetimeErrors は end_time の検証エラーの loc が
// ["query","end_time"] になり、start_time と同じ文言が使われることを固定する。
func TestProgramTimeTableEndTimeDatetimeErrors(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	for _, raw := range []string{"xyz", "abc", "2026-13-01", "2026-10-09T10"} {
		assertValidationResponse(t, server, http.MethodGet,
			"/api/programs/timetable?end_time="+urlQueryEscape(raw),
			http.StatusUnprocessableEntity,
			datetimeParsingRejectBody("end_time", raw, datetimeCaseError(raw)))
	}
}

// datetimeCaseError は start_time / end_time の不正入力に対応する ctx.error 文言を返す。
func datetimeCaseError(raw string) string {
	switch raw {
	case "abc", "xyz":
		return errTooShort
	case "2026-13-01":
		return errMonthOut
	case "2026-10-09T10":
		return errExtraChars
	}
	return ""
}

// TestProgramTimeTableDatetimeErrorOrder は Pydantic が宣言順
// (start_time → end_time → channel_type) で全エラーを 1 配列にまとめることを固定する。
//
// 実機 Python の実測:
//
//	?start_time=abc&end_time=xyz -> 2 件 (start_time, end_time)
//	?start_time=abc&end_time=xyz&channel_type=__invalid__ -> 3 件
func TestProgramTimeTableDatetimeErrorOrder(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	literal := `{"type":"literal_error","loc":["query","channel_type"],` +
		`"msg":"Input should be 'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'","input":"__invalid__",` +
		`"ctx":{"expected":"'GR', 'BS', 'CS', 'CATV', 'SKY' or 'BS4K'"}}`

	// start_time と end_time の 2 件
	two := `{"detail":[` +
		`{"type":"datetime_from_date_parsing","loc":["query","start_time"],"msg":"Input should be a valid datetime or date, input is too short","input":"abc","ctx":{"error":"input is too short"}},` +
		`{"type":"datetime_from_date_parsing","loc":["query","end_time"],"msg":"Input should be a valid datetime or date, input is too short","input":"xyz","ctx":{"error":"input is too short"}}` +
		`]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?start_time=abc&end_time=xyz",
		http.StatusUnprocessableEntity, two)

	// start_time, end_time, channel_type の 3 件
	three := `{"detail":[` +
		`{"type":"datetime_from_date_parsing","loc":["query","start_time"],"msg":"Input should be a valid datetime or date, input is too short","input":"abc","ctx":{"error":"input is too short"}},` +
		`{"type":"datetime_from_date_parsing","loc":["query","end_time"],"msg":"Input should be a valid datetime or date, input is too short","input":"xyz","ctx":{"error":"input is too short"}},` +
		literal + `]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?start_time=abc&end_time=xyz&channel_type=__invalid__",
		http.StatusUnprocessableEntity, three)

	// start_time, channel_type の 2 件
	twoCT := `{"detail":[` +
		`{"type":"datetime_from_date_parsing","loc":["query","start_time"],"msg":"Input should be a valid datetime or date, input is too short","input":"abc","ctx":{"error":"input is too short"}},` +
		literal + `]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?start_time=abc&channel_type=__invalid__",
		http.StatusUnprocessableEntity, twoCT)
}

// TestProgramTimeTableDatetimeLastValueWins は同名クエリパラメータを繰り返したとき
// Starlette が最後の値だけを採用することを固定する。
//
// 実機 Python の実測: ?start_time=abc&start_time=2026-10-09 は 200。
func TestProgramTimeTableDatetimeLastValueWins(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	recorder := doJSONRequest(t, server.Handler(), http.MethodGet,
		"/api/programs/timetable?start_time=abc&start_time=2026-10-09", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
}

// TestProgramTimeTableDatetimeEmptyValue は空文字が「未指定」ではなく
// 検証対象の入力として扱われることを固定する (Python の str は空文字も有効値) 。
//
// 実機 Python の実測: ?start_time= は 422 (input は "") 。
func TestProgramTimeTableDatetimeEmptyValue(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?start_time=",
		http.StatusUnprocessableEntity,
		datetimeParsingRejectBody("start_time", "", errTooShort))
	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?end_time=",
		http.StatusUnprocessableEntity,
		datetimeParsingRejectBody("end_time", "", errTooShort))
}

// TestProgramTimeTableDatetimeYearZero は 0000 年が Python の datetime の範囲外になり、
// datetime_parsing / "year 0 is out of range" を返すことを固定する。
//
// 実機 Python の実測: ?start_time=0000-01-01 は
// {"detail":[{"type":"datetime_parsing","loc":["query","start_time"],
// "msg":"Input should be a valid datetime, year 0 is out of range",...}]}
func TestProgramTimeTableDatetimeYearZero(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	assertValidationResponse(t, server, http.MethodGet,
		"/api/programs/timetable?start_time=0000-01-01",
		http.StatusUnprocessableEntity,
		datetimeTypeRejectBody("start_time", "0000-01-01", errYearZeroHard))
}

// TestProgramTimeTableDatetimeAcceptedValues は Python が 200 で受理する入力集合を固定する
// (負の対照: 受理される値は 422 にならない) 。
//
// 実機 Python の実測で 200 になる値のみを並べている。
func TestProgramTimeTableDatetimeAcceptedValues(t *testing.T) {
	server, _ := newTestServer(t, "")
	timeTableTestSetup(t, server)

	accepted := []string{
		"2026",
		"2026-10-09",
		"2026-10-09T10:00",
		"2026-10-09T10:00:00",
		"20261009100000",
		"2026-10-09T10:00:00Z",
		"2026-10-09T10:00:00+09:00",
		"2026-10-09T10:00:00.123456",
		"2026-10-09 10:00",
		"2026-10-09t10:00",
		"2026-10-09_10:00:00",
		"2026-10-09T10:00:00z",
		"2026-10-09T10:00:00+0900",
		"2026-10-09T10:00:00.1234567",
		"20261009",
		"2026-10-09T10:00:00.5",
		"0001-01-01",
		"1600000000",
	}
	for _, raw := range accepted {
		recorder := doJSONRequest(t, server.Handler(), http.MethodGet,
			"/api/programs/timetable?start_time="+urlQueryEscape(raw), "", "", "")
		if recorder.Code != http.StatusOK {
			t.Errorf("start_time=%s: status = %d, want 200 (body: %s)", raw, recorder.Code, recorder.Body.String())
		}
		recorder = doJSONRequest(t, server.Handler(), http.MethodGet,
			"/api/programs/timetable?end_time="+urlQueryEscape(raw), "", "", "")
		if recorder.Code != http.StatusOK {
			t.Errorf("end_time=%s: status = %d, want 200 (body: %s)", raw, recorder.Code, recorder.Body.String())
		}
	}
}

// TestProgramTimeTableDatetimePreservesExistingRange は、変更前の Go 実装
// (parseQueryDatetime / database.ParseDBTime) が受理していた入力について、
// 新しいパーサが同じ時刻を返すことを固定する。
// timetable の取得範囲 (応答内容) を変えていないことの担保になる。
func TestProgramTimeTableDatetimePreservesExistingRange(t *testing.T) {
	inputs := []string{
		"2026-10-09T10:00:00+09:00",
		"2026-10-09T10:00:00Z",
		"2026-10-09 10:00:00+09:00",
		"2026-10-09 10:00:00",
		"2026-10-09 10:00:00.123456+09:00",
		"2026-10-09T10:00:00.500000+09:00",
	}
	for _, raw := range inputs {
		legacy, err := database.ParseDBTime(raw)
		if err != nil {
			t.Fatalf("ParseDBTime(%q) failed: %v", raw, err)
		}
		legacy = legacy.In(constants.JST)
		parsed, parseErr := parsePydanticDatetime(raw)
		if parseErr != nil {
			t.Errorf("parsePydanticDatetime(%q) unexpectedly failed: %+v", raw, parseErr)
			continue
		}
		if !parsed.Equal(legacy) {
			t.Errorf("parsePydanticDatetime(%q) = %s, legacy = %s", raw, parsed, legacy)
		}
	}
}
