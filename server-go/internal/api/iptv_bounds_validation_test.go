package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// このファイルは IPTV チャンネル一覧の範囲制約 (ge / le) の検証が、実機の Python 版
// (FastAPI/Pydantic) の応答本文と完全一致することを固定する。
//
// 期待値はすべて録画機の本番 Python サーバー (127.0.0.77:7010) から実測した生 JSON で、
// Pydantic の範囲制約エラーは ctx ({"ge": 1} / {"le": 500}) を持つ点まで含めて一致させる。

// iptvGreaterThanEqualPage は page=0 (および -1) の実測応答。input は解析前の生文字列。
const iptvGreaterThanEqualPage = `{"detail":[{"type":"greater_than_equal","loc":["query","page"],"msg":"Input should be greater than or equal to 1","input":"0","ctx":{"ge":1}}]}`

// iptvGreaterThanEqualPageNegative は page=-1 の実測応答。
const iptvGreaterThanEqualPageNegative = `{"detail":[{"type":"greater_than_equal","loc":["query","page"],"msg":"Input should be greater than or equal to 1","input":"-1","ctx":{"ge":1}}]}`

// iptvGreaterThanEqualPerPage は per_page=0 の実測応答。
const iptvGreaterThanEqualPerPage = `{"detail":[{"type":"greater_than_equal","loc":["query","per_page"],"msg":"Input should be greater than or equal to 1","input":"0","ctx":{"ge":1}}]}`

// iptvLessThanEqualPerPage は per_page=501 の実測応答 (上限は IPTVRouter.MAX_PER_PAGE = 500) 。
const iptvLessThanEqualPerPage = `{"detail":[{"type":"less_than_equal","loc":["query","per_page"],"msg":"Input should be less than or equal to 500","input":"501","ctx":{"le":500}}]}`

// iptvBoundsAndBoolAggregated は page / per_page の範囲違反と refresh / with_quality の
// bool 不正が同時に起きた場合の実測応答。Python は宣言順 (page, per_page, refresh, with_quality)
// で 1 つの配列にまとめるため、その順序も固定する。
const iptvBoundsAndBoolAggregated = `{"detail":[{"type":"greater_than_equal","loc":["query","page"],"msg":"Input should be greater than or equal to 1","input":"0","ctx":{"ge":1}},{"type":"less_than_equal","loc":["query","per_page"],"msg":"Input should be less than or equal to 500","input":"501","ctx":{"le":500}},{"type":"bool_parsing","loc":["query","refresh"],"msg":"Input should be a valid boolean, unable to interpret input","input":"abc"},{"type":"bool_parsing","loc":["query","with_quality"],"msg":"Input should be a valid boolean, unable to interpret input","input":"xyz"}]}`

// TestIPTVChannelsBoundsValidation は範囲制約 (ge / le) 付き int パラメータの応答を検証する。
func TestIPTVChannelsBoundsValidation(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	for _, testCase := range []struct {
		name     string
		query    string
		expected string
	}{
		{name: "page_below_ge", query: "page=0", expected: iptvGreaterThanEqualPage},
		{name: "page_negative", query: "page=-1", expected: iptvGreaterThanEqualPageNegative},
		{name: "per_page_below_ge", query: "per_page=0", expected: iptvGreaterThanEqualPerPage},
		{name: "per_page_above_le", query: "per_page=501", expected: iptvLessThanEqualPerPage},
		{name: "bounds_and_bool_aggregated", query: "page=0&per_page=501&refresh=abc&with_quality=xyz", expected: iptvBoundsAndBoolAggregated},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/iptv/channels?"+testCase.query, nil))
			assertIPTVValidationBody(t, recorder, testCase.expected)
		})
	}
}

// TestIPTVChannelsBoundsNegativeControl は境界値そのもの (ge / le のちょうど) が
// 拒否されず 200 になることを確認する負の対照。範囲検証が「常に 422」に壊れた場合に落ちる。
func TestIPTVChannelsBoundsNegativeControl(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	for _, testCase := range []struct {
		name  string
		query string
	}{
		{name: "page_at_ge", query: "page=1&per_page=1"},
		{name: "per_page_at_le", query: "per_page=500"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/iptv/channels?"+testCase.query, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
