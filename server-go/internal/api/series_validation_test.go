package api

import (
	"net/http"
	"testing"
)

// order の Literal エラー期待本文。SeriesListAPI / SeriesSearchAPI で共通。
const seriesOrderLiteralDetail = `{"detail":[{"type":"literal_error","loc":["query","order"],` +
	`"msg":"Input should be 'desc' or 'asc'","input":"__invalid__",` +
	`"ctx":{"expected":"'desc' or 'asc'"}}]}` + "\n"

// TestSeriesListOrderValidation は GET /api/series の order が
// Python 版と同じ literal_error 配列になることを固定する。
//
// 移植元: SeriesRouter.SeriesListAPI の
// order: Annotated[Literal['desc','asc'], Query(...)] = 'desc'
// 実機 Python (127.0.0.77:7010) の実測値 (SeriesSearchAPI):
// GET /api/series/search?order=__invalid__ は 422 で literal_error 配列を返す。
// 修正前の Go 版は {"detail":"Invalid order"} という文字列 detail を返していた。
func TestSeriesListOrderValidation(t *testing.T) {
	server, _ := newTestServer(t, "")
	assertValidationResponse(t, server, http.MethodGet, "/api/series?order=__invalid__",
		http.StatusUnprocessableEntity, seriesOrderLiteralDetail)
}

// TestSeriesSearchOrderValidation は GET /api/series/search の order が
// literal_error 配列になることを固定する (実機実測値そのもの) 。
func TestSeriesSearchOrderValidation(t *testing.T) {
	server, _ := newTestServer(t, "")
	assertValidationResponse(t, server, http.MethodGet, "/api/series/search?order=__invalid__",
		http.StatusUnprocessableEntity, seriesOrderLiteralDetail)
}

// TestSeriesPageIntParsing は int 型の page が Pydantic の int_parsing になることを固定する。
//
// 移植元: SeriesRouter.SeriesListAPI / SeriesSearchAPI の
// page: Annotated[int, Query(...)] = 1
// 実機で確認した int_parsing の msg に合わせ、小数・指数表記も int_parsing として扱う。
func TestSeriesPageIntParsing(t *testing.T) {
	server, _ := newTestServer(t, "")

	want := `{"detail":[{"type":"int_parsing","loc":["query","page"],` +
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet, "/api/series?page=abc",
		http.StatusUnprocessableEntity, want)

	// Pydantic は小数表記も int_parsing として扱う (int_from_float ではない)
	wantDecimal := `{"detail":[{"type":"int_parsing","loc":["query","page"],` +
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":"1.5"}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet, "/api/series?page=1.5",
		http.StatusUnprocessableEntity, wantDecimal)

	// 検索 API でも同じ
	assertValidationResponse(t, server, http.MethodGet, "/api/series/search?page=abc",
		http.StatusUnprocessableEntity, want)
}

// TestSeriesSearchValidationOrder は複数の検証エラーが宣言順 (order → page) に
// 1 つの配列へまとまることを検証する。
func TestSeriesSearchValidationOrder(t *testing.T) {
	server, _ := newTestServer(t, "")
	want := `{"detail":[` +
		`{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc' or 'asc'","input":"__invalid__","ctx":{"expected":"'desc' or 'asc'"}},` +
		`{"type":"int_parsing","loc":["query","page"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet, "/api/series/search?order=__invalid__&page=abc",
		http.StatusUnprocessableEntity, want)
}

// TestSeriesIDPathIntParsing は必須のパスパラメータ series_id が
// Pydantic の int_parsing になることを固定する。
//
// 移植元: SeriesRouter.SeriesAPI の series_id: Annotated[int, Path(...)] (必須)
// パスパラメータはロケーションが ["path", "series_id"] になる。
func TestSeriesIDPathIntParsing(t *testing.T) {
	server, _ := newTestServer(t, "")
	want := `{"detail":[{"type":"int_parsing","loc":["path","series_id"],` +
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}` + "\n"
	assertValidationResponse(t, server, http.MethodGet, "/api/series/abc",
		http.StatusUnprocessableEntity, want)
}

// TestSeriesValidationDefaults は両ルーターに必須のクエリパラメータが存在せず、
// 省略時は既定値 (order は desc、page は 1、query は空文字) が使われて
// missing にならないことを検証する (負の対照) 。
//
// 移植元の宣言はすべて既定値つき (order=desc、page=1、query=空文字) で、
// 必須なのは GET /api/series/{series_id} の系列 ID (パス) だけである。
func TestSeriesValidationDefaults(t *testing.T) {
	server, _ := newTestServer(t, "")
	for _, path := range []string{
		"/api/series",
		"/api/series/search",
		"/api/series?order=asc&page=2",
		"/api/series/search?query=x&order=asc&page=2",
	} {
		recorder := doJSONRequest(t, server.Handler(), http.MethodGet, path, "", "", "")
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (body: %s)", path, recorder.Code, recorder.Body.String())
		}
	}
}
