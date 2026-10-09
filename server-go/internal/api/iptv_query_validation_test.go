package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// iptvIntParsingPage は Pydantic の int_parsing エラー配列 (page) の期待値。
const iptvIntParsingPage = `{"detail":[{"type":"int_parsing","loc":["query","page"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}`

// iptvIntParsingPerPage は Pydantic の int_parsing エラー配列 (per_page) の期待値。
const iptvIntParsingPerPage = `{"detail":[{"type":"int_parsing","loc":["query","per_page"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}`

// iptvBoolParsingRefresh は Pydantic の bool_parsing エラー配列 (refresh) の期待値。
const iptvBoolParsingRefresh = `{"detail":[{"type":"bool_parsing","loc":["query","refresh"],"msg":"Input should be a valid boolean, unable to interpret input","input":"abc"}]}`

// iptvBoolParsingWithQuality は Pydantic の bool_parsing エラー配列 (with_quality) の期待値。
const iptvBoolParsingWithQuality = `{"detail":[{"type":"bool_parsing","loc":["query","with_quality"],"msg":"Input should be a valid boolean, unable to interpret input","input":"abc"}]}`

// iptvMissingURL は Pydantic の missing エラー配列 (必須クエリ url) の期待値。
const iptvMissingURL = `{"detail":[{"type":"missing","loc":["query","url"],"msg":"Field required","input":null}]}`

// assertIPTVValidationBody は 422 のレスポンス本文が期待する検証エラー配列と完全一致することを検証する。
func assertIPTVValidationBody(t *testing.T, recorder *httptest.ResponseRecorder, expected string) {
	t.Helper()
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", recorder.Code, recorder.Body.String())
	}
	// writeJSON は末尾に改行を付けるため、比較時に取り除く
	actual := strings.TrimRight(recorder.Body.String(), "\n")
	if actual != expected {
		t.Errorf("body = %s, want %s", actual, expected)
	}
}

// TestIPTVChannelsQueryValidation はチャンネル一覧 API のクエリ検証が FastAPI 互換であることを検証する。
func TestIPTVChannelsQueryValidation(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	for _, testCase := range []struct {
		name     string
		query    string
		expected string
	}{
		{"page int_parsing", "?page=abc", iptvIntParsingPage},
		{"per_page int_parsing", "?per_page=abc", iptvIntParsingPerPage},
		{"refresh bool_parsing", "?refresh=abc", iptvBoolParsingRefresh},
		{"with_quality bool_parsing", "?with_quality=abc", iptvBoolParsingWithQuality},
		// 宣言順 (page, per_page, refresh, with_quality) に全件を 1 つの配列へまとめる (最初の失敗で止めない)
		{
			"aggregated in declaration order",
			"?page=abc&per_page=0&refresh=abc&with_quality=abc",
			`{"detail":[` +
				`{"type":"int_parsing","loc":["query","page"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"},` +
				// ge/le の ctx (実測値) まで一致させる
				`{"type":"greater_than_equal","loc":["query","per_page"],"msg":"Input should be greater than or equal to 1","input":"0","ctx":{"ge":1}},` +
				`{"type":"bool_parsing","loc":["query","refresh"],"msg":"Input should be a valid boolean, unable to interpret input","input":"abc"},` +
				`{"type":"bool_parsing","loc":["query","with_quality"],"msg":"Input should be a valid boolean, unable to interpret input","input":"abc"}` +
				`]}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+testCase.query, "", "", "")
			assertIPTVValidationBody(t, recorder, testCase.expected)
		})
	}

	// 範囲制約の 422 を実機 Python の本文 (ctx つき) で固定する
	for _, testCase := range []struct {
		query    string
		expected string
	}{
		{"?page=0", `{"detail":[{"type":"greater_than_equal","loc":["query","page"],"msg":"Input should be greater than or equal to 1","input":"0","ctx":{"ge":1}}]}`},
		{"?per_page=501", `{"detail":[{"type":"less_than_equal","loc":["query","per_page"],"msg":"Input should be less than or equal to 500","input":"501","ctx":{"le":500}}]}`},
	} {
		recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+testCase.query, "", "", "")
		assertIPTVValidationBody(t, recorder, testCase.expected)
	}
}

// TestIPTVChannelsQueryCoercion は Pydantic v2 の int / bool 型強制の受理・拒否を固定する。
func TestIPTVChannelsQueryCoercion(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	// 受理される値は 200 を返す
	accepted := []string{
		"?page=1", "?page=%2B1", "?page=%201%20", "?page=1_0", "?page=1.0",
		"?per_page=%2B10", "?per_page=%2060%20",
		"?refresh=true", "?refresh=false", "?refresh=1", "?refresh=0",
		"?refresh=yes", "?refresh=no", "?refresh=on", "?refresh=off",
		"?refresh=t", "?refresh=f", "?refresh=y", "?refresh=n",
		"?refresh=TRUE", "?refresh=Yes", "?refresh=OFF",
		"?with_quality=true", "?with_quality=0",
	}
	for _, query := range accepted {
		if recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+query, "", "", ""); recorder.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200, body = %s", query, recorder.Code, recorder.Body.String())
		}
	}

	// 拒否される値は 422 を返す
	rejected := []string{
		"?page=1.5", "?page=1e2", "?page=", "?page=0x10", "?page=-3",
		"?refresh=", "?refresh=%20true%20", "?refresh=Truee", "?refresh=2",
		"?with_quality=abc",
	}
	for _, query := range rejected {
		if recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+query, "", "", ""); recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422, body = %s", query, recorder.Code, recorder.Body.String())
		}
	}
}

// TestIPTVCountriesAndGroupsQueryValidation は国・グループ一覧 API の refresh 検証を検証する。
func TestIPTVCountriesAndGroupsQueryValidation(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	for _, path := range []string{"/api/iptv/countries", "/api/iptv/groups"} {
		assertIPTVValidationBody(t,
			doJSONRequest(t, handler, http.MethodGet, path+"?refresh=abc", "", "", ""),
			iptvBoolParsingRefresh)
		// 受理される値は 200
		for _, query := range []string{"?refresh=true", "?refresh=0", "?refresh=off", "?refresh=YES"} {
			if recorder := doJSONRequest(t, handler, http.MethodGet, path+query, "", "", ""); recorder.Code != http.StatusOK {
				t.Errorf("%s%s: status = %d, want 200", path, query, recorder.Code)
			}
		}
	}
}

// TestIPTVDisabledStillValidates は IPTV 機能有効・無効に関わらずクエリ検証が先に行われることを検証する。
func TestIPTVDisabledStillValidates(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	server.config.IPTV.Enabled = false
	handler := server.Handler()

	assertIPTVValidationBody(t,
		doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?refresh=abc", "", "", ""),
		iptvBoolParsingRefresh)
	assertIPTVValidationBody(t,
		doJSONRequest(t, handler, http.MethodGet, "/api/iptv/countries?refresh=abc", "", "", ""),
		iptvBoolParsingRefresh)
	assertIPTVValidationBody(t,
		doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?page=abc", "", "", ""),
		iptvIntParsingPage)
}

// TestIPTVProxyAndLogoRequiredURL は proxy / logo の必須 url 欠落が 422 の missing 配列になることを検証する。
func TestIPTVProxyAndLogoRequiredURL(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	for _, path := range []string{"/api/iptv/proxy", "/api/iptv/logo"} {
		// url が無い場合は 422 の missing 配列
		assertIPTVValidationBody(t,
			doJSONRequest(t, handler, http.MethodGet, path, "", "", ""),
			iptvMissingURL)
		// 空文字の url は「欠落」ではなく str として有効。ただしスキーム検証で 400 になる。
		recorder := doJSONRequest(t, handler, http.MethodGet, path+"?url=", "", "", "")
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Proxy URL must be started with http:// or https://.") {
			t.Errorf("%s?url=: status = %d, body = %s", path, recorder.Code, recorder.Body.String())
		}
		// 不正なスキームは従来どおり 400 の文字列 detail
		recorder = doJSONRequest(t, handler, http.MethodGet, path+"?url=ftp%3A%2F%2Fexample.com%2Fx", "", "", "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s?url=ftp: status = %d, want 400", path, recorder.Code)
		}
	}
}
