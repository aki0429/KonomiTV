package api

import (
	"net/http"
	"testing"
)

// TestVideoStreamUnknownVideoParity は録画ストリーム系 API の未知 video_id 応答を経路横断で固定する。
//
// 移植元: VideoStreamsRouter.ValidateVideoID() は未知 ID に対して 422
// (detail: "Specified video_id was not found") を返す。Go 版の offline-stream だけが
// 独自に 404 を返していたため、実機の差分プローブ (Go 404 / Python 422) を根拠に
// unknownVideoStatus の分岐を撤去した。同じ回帰を二度と入れないよう全経路を列挙する。
// 依存 (ValidateVideoID) は Pydantic の検証通過後に走るので、必須クエリを満たした URL を使う。
func TestVideoStreamUnknownVideoParity(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/streams/video/99999/1080p/playlist?session_id=x"},
		{http.MethodGet, "/api/streams/video/99999/1080p/offline-stream"},
		{http.MethodGet, "/api/streams/video/99999/1080p/segment?session_id=x&sequence=0&cache_key=k"},
		{http.MethodGet, "/api/streams/video/99999/1080p/buffer?session_id=x"},
		{http.MethodPut, "/api/streams/video/99999/1080p/keep-alive?session_id=x"},
	}
	for _, testCase := range cases {
		assertPG4Error(t, server, testCase.method, testCase.path, http.StatusUnprocessableEntity,
			"Specified video_id was not found")
	}
}

// TestVideoStreamValidationOrderParity は FastAPI/Pydantic の検証順を実測値で固定する。
//
// 2026-10-09 に実機の Python 版 (127.0.0.77:7010) が返した応答本文をそのまま期待値にしている。
// Pydantic はパスパラメータ → クエリパラメータの宣言順に全エラーを 1 つの配列へまとめ、
// 依存 (ValidateVideoID / ValidateQuality) はその検証を通過した後でだけ実行される。
func TestVideoStreamValidationOrderParity(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	// パス型エラーとクエリ欠落が同時に起きても 1 つの配列にまとまる (パスが先) 。
	want := `{"detail":[` +
		`{"type":"int_parsing","loc":["path","video_id"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"},` +
		`{"type":"missing","loc":["query","session_id"],"msg":"Field required","input":null},` +
		`{"type":"missing","loc":["query","sequence"],"msg":"Field required","input":null},` +
		`{"type":"missing","loc":["query","cache_key"],"msg":"Field required","input":null}]}` + "\n"
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/segment", http.StatusUnprocessableEntity, want)

	// Pydantic は小数・指数表記も int_parsing として扱う (int_from_float ではない)
	wantParsing := `{"detail":[{"type":"int_parsing","loc":["query","sequence"],` +
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":"1.5"}]}` + "\n"
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=x&cache_key=k&sequence=1.5",
		http.StatusUnprocessableEntity, wantParsing)

	// 空文字は欠落ではない (str は空文字も有効値) ため literal_error になる
	wantAudio := `{"detail":[{"type":"literal_error","loc":["query","audio"],` +
		`"msg":"Input should be 'primary' or 'secondary'","input":"xxx",` +
		`"ctx":{"expected":"'primary' or 'secondary'"}}]}` + "\n"
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=x&cache_key=k&sequence=1&audio=xxx",
		http.StatusUnprocessableEntity, wantAudio)

	wantType := `{"detail":[{"type":"literal_error","loc":["query","type"],` +
		`"msg":"Input should be 'master', 'primary-audio' or 'secondary-audio'","input":"xxx",` +
		`"ctx":{"expected":"'master', 'primary-audio' or 'secondary-audio'"}}]}` + "\n"
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/playlist?session_id=x&type=xxx",
		http.StatusUnprocessableEntity, wantType)
}
