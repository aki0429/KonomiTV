package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestVideosValidationParity は GET /api/videos・/api/videos/search の検証エラー応答を固定する。
//
// 期待値は録画機の本番 Python 版 (127.0.0.77:7010) から 2026-10-09 に実測した本文そのもの。
// クライアント (client/src/services/APIClient.ts) は detail が配列か文字列かで表示を分岐するため、
// エラーの種類・件数・宣言順・loc の要素 (list[int] は要素インデックスを含む) まで一致させる必要がある。
//
// 移植元: server/app/routers/VideosRouter.py
//
//	GET /api/videos         order: Literal['desc','asc','ids'] = 'desc' / page: int = 1 / ids: list[int] | None = None
//	GET /api/videos/search  query: str = '' / order: Literal['desc','asc'] = 'desc' / page: int = 1
func TestVideosValidationParity(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	intParsing := "Input should be a valid integer, unable to parse string as an integer"
	// 検証エラーが返るケース。本文は Python 版の実測と完全一致させる (フィールド順も含む)
	errorCases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "page が非整数",
			path: "/api/videos?page=abc",
			want: `{"detail":[{"type":"int_parsing","loc":["query","page"],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "ids の要素が非整数 (loc に要素インデックスが入る)",
			path: "/api/videos?ids=abc",
			want: `{"detail":[{"type":"int_parsing","loc":["query","ids",0],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "ids の 2 番目の要素が非整数",
			path: "/api/videos?ids=1&ids=abc",
			want: `{"detail":[{"type":"int_parsing","loc":["query","ids",1],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "ids が空文字 (空文字は有効値ではない)",
			path: "/api/videos?ids=",
			want: `{"detail":[{"type":"int_parsing","loc":["query","ids",0],"msg":"` + intParsing + `","input":""}]}`,
		},
		{
			name: "ids はカンマ区切りを受け付けない (FastAPI は繰り返しパラメータのみ)",
			path: "/api/videos?ids=1,2",
			want: `{"detail":[{"type":"int_parsing","loc":["query","ids",0],"msg":"` + intParsing + `","input":"1,2"}]}`,
		},
		{
			name: "order が Literal 外",
			path: "/api/videos?order=__invalid__",
			want: `{"detail":[{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc', 'asc' or 'ids'","input":"__invalid__","ctx":{"expected":"'desc', 'asc' or 'ids'"}}]}`,
		},
		{
			name: "order は大文字小文字を区別する",
			path: "/api/videos?order=DESC",
			want: `{"detail":[{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc', 'asc' or 'ids'","input":"DESC","ctx":{"expected":"'desc', 'asc' or 'ids'"}}]}`,
		},
		{
			name: "order → page の宣言順で 2 件を 1 配列にまとめる",
			path: "/api/videos?page=abc&order=__invalid__",
			want: `{"detail":[{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc', 'asc' or 'ids'","input":"__invalid__","ctx":{"expected":"'desc', 'asc' or 'ids'"}},` +
				`{"type":"int_parsing","loc":["query","page"],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "page → ids の宣言順で 2 件を 1 配列にまとめる",
			path: "/api/videos?ids=abc&page=abc",
			want: `{"detail":[{"type":"int_parsing","loc":["query","page"],"msg":"` + intParsing + `","input":"abc"},` +
				`{"type":"int_parsing","loc":["query","ids",0],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "search の order は desc/asc のみ",
			path: "/api/videos/search?order=__invalid__",
			want: `{"detail":[{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc' or 'asc'","input":"__invalid__","ctx":{"expected":"'desc' or 'asc'"}}]}`,
		},
		{
			name: "search も order → page の宣言順",
			path: "/api/videos/search?page=abc&order=__invalid__",
			want: `{"detail":[{"type":"literal_error","loc":["query","order"],"msg":"Input should be 'desc' or 'asc'","input":"__invalid__","ctx":{"expected":"'desc' or 'asc'"}},` +
				`{"type":"int_parsing","loc":["query","page"],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
		{
			name: "search の query は制約のない str (検証エラーにならない)",
			path: "/api/videos/search?query=abc&page=abc",
			want: `{"detail":[{"type":"int_parsing","loc":["query","page"],"msg":"` + intParsing + `","input":"abc"}]}`,
		},
	}
	for _, testCase := range errorCases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := doJSONRequest(t, handler, http.MethodGet, testCase.path, "", "", "")
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			// writeJSON は json.Encoder を使うため本文末尾に改行が 1 つ付く (Python 版は改行なし) 。
			// クライアントの解釈には影響しない差なので、比較時だけ取り除く。
			got := strings.TrimSuffix(recorder.Body.String(), "\n")
			if got != testCase.want {
				t.Errorf("body mismatch\n got: %s\nwant: %s", got, testCase.want)
			}
		})
	}

	// 検証を通過するケース (Python 版も 200 を返す)
	acceptCases := []struct {
		name string
		path string
	}{
		{name: "page=0 は先頭ページを返す (Python は 200)", path: "/api/videos?page=0"},
		{name: "page=-1 も先頭ページを返す (Python は 200)", path: "/api/videos?page=-1"},
		{name: "page は小数ゼロ表記を受理する", path: "/api/videos?page=1.0"},
		{name: "page が範囲外でも 200 で空になる", path: "/api/videos?page=99999"},
		{name: "ids の要素は小数ゼロ表記を受理する", path: "/api/videos?ids=1&ids=1.0"},
		{name: "search も page=0 は 200", path: "/api/videos/search?page=0"},
	}
	for _, testCase := range acceptCases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := doJSONRequest(t, handler, http.MethodGet, testCase.path, "", "", "")
			if recorder.Code != http.StatusOK {
				t.Errorf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
