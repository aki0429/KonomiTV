package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// このファイルは独立レビュー用の probe。親の期待値は参照せず、
// server/app/routers/VideoStreamsRouter.py のパラメータ宣言をローカル FastAPI 0.136.3 /
// Pydantic 2.13.5 で再現して得た ground truth (python_probe_result.json) から期待値を導出した。
// 参考: 依存は Pydantic 検証通過後に解決され、例外は蓄積済み検証エラーを破棄する。

// zzDetail は検証エラー配列の期待本文を組み立てる。
func zzDetail(items ...string) string {
	return `{"detail":[` + strings.Join(items, ",") + `]}` + "\n"
}

const (
	zzMissingSession  = `{"type":"missing","loc":["query","session_id"],"msg":"Field required","input":null}`
	zzMissingSequence = `{"type":"missing","loc":["query","sequence"],"msg":"Field required","input":null}`
	zzMissingCacheKey = `{"type":"missing","loc":["query","cache_key"],"msg":"Field required","input":null}`
	// zzSessionAbsent はクエリ検証を通過したが存在しないセッションに到達したことを示す。
	zzSessionAbsent = "{\"detail\":\"Session does not exist\"}\n"
)

func zzPathVideoIDParsing(input string) string {
	return fmt.Sprintf(`{"type":"int_parsing","loc":["path","video_id"],`+
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":%q}`, input)
}

func zzQueryIntParsing(name, input string) string {
	return fmt.Sprintf(`{"type":"int_parsing","loc":["query",%q],`+
		`"msg":"Input should be a valid integer, unable to parse string as an integer","input":%q}`, name, input)
}

// TestZZReviewPathQueryOrderAndShape は配列の要素順序 (path → query, 宣言順) と
// missing / int_parsing の区別を固定する。
func TestZZReviewPathQueryOrderAndShape(t *testing.T) {
	server, _ := setupVideoStreamTest(t)

	// Python: abc/720p/segment (クエリ無し) → video_id が先、続いて session_id/sequence/cache_key
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/segment",
		http.StatusUnprocessableEntity,
		zzDetail(zzPathVideoIDParsing("abc"), zzMissingSession, zzMissingSequence, zzMissingCacheKey))

	// Python: abc/720p/buffer (クエリ無し) → [video_id, session_id]
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/buffer",
		http.StatusUnprocessableEntity,
		zzDetail(zzPathVideoIDParsing("abc"), zzMissingSession))

	// Python: abc/720p/buffer?session_id=x → [video_id] のみ (クエリが揃えば path エラーだけ残る)
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/buffer?session_id=x",
		http.StatusUnprocessableEntity,
		zzDetail(zzPathVideoIDParsing("abc")))

	// Python: 1043/720p/segment?session_id=x&sequence=abc → [sequence(int_parsing), cache_key(missing)]
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=x&sequence=abc",
		http.StatusUnprocessableEntity,
		zzDetail(zzQueryIntParsing("sequence", "abc"), zzMissingCacheKey))

	// 空文字の sequence は欠落ではなく int_parsing (input は空文字)
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=x&sequence=&cache_key=k",
		http.StatusUnprocessableEntity,
		zzDetail(zzQueryIntParsing("sequence", "")))

	// 空文字の session_id は str として有効値なので欠落にならない (存在しないセッションへ進む)
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=&sequence=0&cache_key=k",
		http.StatusUnprocessableEntity, zzSessionAbsent)

	// 空文字の cache_key も str|None として有効値
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/segment?session_id=x&sequence=0&cache_key=",
		http.StatusUnprocessableEntity, zzSessionAbsent)
}

// TestZZReviewDependencyPriority は依存 (ValidateVideoID / ValidateQuality) が
// Pydantic 検証エラーより優先されることを固定する。
func TestZZReviewDependencyPriority(t *testing.T) {
	server, _ := setupVideoStreamTest(t)

	// video_id 非整数 + 不正 quality → 依存の品質例外が勝つ (path エラーは破棄)
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/abc/9999p/playlist?session_id=x",
		http.StatusUnprocessableEntity, "Specified quality was not found")
	// video_id 非整数 + original → 依存の original 例外が勝つ
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/abc/original/buffer?session_id=x",
		http.StatusUnprocessableEntity, "Original quality is not available for HLS playlist")
	// 存在しないが整数の video_id → video 依存が勝つ
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/9999/720p/playlist?session_id=x",
		http.StatusUnprocessableEntity, "Specified video_id was not found")
	// video_id 非整数 + quality 正常 → video 依存は呼ばれず検証配列が残る
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/playlist",
		http.StatusUnprocessableEntity, zzDetail(zzPathVideoIDParsing("abc"), zzMissingSession))
	// 不正 quality はクエリ欠落より優先される (依存が先)
	assertPG4Error(t, server, http.MethodPut, "/api/streams/video/1/9999p/keep-alive",
		http.StatusUnprocessableEntity, "Specified quality was not found")
	// +9999 は Python の int として有効 (存在しない整数なので not found)
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/+9999/720p/offline-stream",
		http.StatusUnprocessableEntity, "Specified video_id was not found")
}

// TestZZReviewLiteralCtx は literal_error の ctx.expected と入力値を固定する。
func TestZZReviewLiteralCtx(t *testing.T) {
	server, _ := setupVideoStreamTest(t)

	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=x&type=xxx",
		http.StatusUnprocessableEntity, zzDetail(
			`{"type":"literal_error","loc":["query","type"],`+
				`"msg":"Input should be 'master', 'primary-audio' or 'secondary-audio'","input":"xxx",`+
				`"ctx":{"expected":"'master', 'primary-audio' or 'secondary-audio'"}}`))

	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/1/720p/segment?session_id=x&cache_key=k&sequence=0&audio=xxx",
		http.StatusUnprocessableEntity, zzDetail(
			`{"type":"literal_error","loc":["query","audio"],`+
				`"msg":"Input should be 'primary' or 'secondary'","input":"xxx",`+
				`"ctx":{"expected":"'primary' or 'secondary'"}}`))

	// type= (空文字) は欠落ではなく literal_error (input は空文字)
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/1/720p/playlist?session_id=x&type=",
		http.StatusUnprocessableEntity, zzDetail(
			`{"type":"literal_error","loc":["query","type"],`+
				`"msg":"Input should be 'master', 'primary-audio' or 'secondary-audio'","input":"",`+
				`"ctx":{"expected":"'master', 'primary-audio' or 'secondary-audio'"}}`))
}

// TestZZReviewNumericGrammar は Pydantic の文字列→int 変換の受理/拒否を固定する。
// 受理される場合は存在しないセッションへ到達するため "Session does not exist" になる。
// 期待値はローカル Pydantic 2.13.5 の実測 (numeric_probe_result.json) から導出。
func TestZZReviewNumericGrammar(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	base := "/api/streams/video/1/720p/segment?session_id=x&cache_key=k&sequence="

	accepted := []struct{ name, url string }{
		{"ゼロ小数部 1.0", "1.0"},
		{"ゼロ小数部 1.00", "1.00"},
		{"符号付きゼロ小数 +1.0", "%2B1.0"},
		{"アンダースコア+ゼロ小数 1_0.0", "1_0.0"},
		{"先頭ゼロ 000.0", "000.0"},
		{"負のゼロ -0", "-0"},
		{"先頭ゼロ 007", "007"},
		{"アンダースコア 1_0", "1_0"},
		{"桁区切り 1_000", "1_000"},
		{"前後空白 1", "%201%20"},
		{"符合 + のみ前置 +1", "%2B1"},
		{"負値 -1", "-1"},
		{"ゼロ 0", "0"},
		{"int64 上限", "9223372036854775807"},
		{"int64 上限+1", "9223372036854775808"},
		{"int64 下限", "-9223372036854775808"},
		{"ゼロ小数を多数 1.000000000000000000000", "1.000000000000000000000"},
	}
	for _, c := range accepted {
		t.Run("accept_"+c.name, func(t *testing.T) {
			assertPG4Body(t, server, http.MethodGet, base+c.url,
				http.StatusUnprocessableEntity, zzSessionAbsent)
		})
	}

	// rejected は URL 文字列と、Pydantic が input に載せるデコード後の文字列の両方を持つ。
	rejected := []struct{ name, url, input string }{
		{"小数 1.5", "1.5", "1.5"},
		{"指数 1e2", "1e2", "1e2"},
		{"指数 1e", "1e", "1e"},
		{"文字 abc", "abc", "abc"},
		{"末尾ドット 1.", "1.", "1."},
		{"先頭ドット .5", ".5", ".5"},
		{"16進 0x10", "0x10", "0x10"},
		{"inf", "inf", "inf"},
		{"NaN", "NaN", "NaN"},
		{"二重マイナス --1", "--1", "--1"},
		{"プラスマイナス +-1", "%2B-1", "+-1"},
		{"二重アンダースコア 1__0", "1__0", "1__0"},
		{"先頭アンダースコア _1", "_1", "_1"},
		{"末尾アンダースコア 1_", "1_", "1_"},
		{"内部空白 1 0", "1%200", "1 0"},
		{"非ASCII数字", "%D9%A1%D9%A2%D9%A3", "١٢٣"},
		{"プラス単独", "%2B", "+"},
		{"マイナス単独", "-", "-"},
		{"非ゼロ小数部 1.0000000000000000001", "1.0000000000000000001", "1.0000000000000000001"},
	}
	for _, c := range rejected {
		t.Run("reject_"+c.name, func(t *testing.T) {
			assertPG4Body(t, server, http.MethodGet, base+c.url,
				http.StatusUnprocessableEntity, zzDetail(zzQueryIntParsing("sequence", c.input)))
		})
	}

	// 空白のみは int_parsing。input は URL デコード後の空白 1 文字になる。
	assertPG4Body(t, server, http.MethodGet, base+"%20",
		http.StatusUnprocessableEntity, zzDetail(zzQueryIntParsing("sequence", " ")))
}

// TestZZReviewDuplicateQueryLastWins は同名クエリで最後の値が採用されることを固定する。
func TestZZReviewDuplicateQueryLastWins(t *testing.T) {
	server, _ := setupVideoStreamTest(t)
	const base = "/api/streams/video/1/720p/segment?session_id=x&cache_key=k"

	// 最後が abc → int_parsing (input は abc)
	assertPG4Body(t, server, http.MethodGet, base+"&sequence=5&sequence=abc",
		http.StatusUnprocessableEntity, zzDetail(zzQueryIntParsing("sequence", "abc")))
	// 最後が 3 → 受理
	assertPG4Body(t, server, http.MethodGet, base+"&sequence=abc&sequence=3",
		http.StatusUnprocessableEntity, zzSessionAbsent)
	// 最後に欠落を作れないので、session_id の重複で最後が採用されることを確認
	assertPG4Body(t, server, http.MethodGet,
		"/api/streams/video/1/720p/buffer?session_id=&session_id=",
		http.StatusUnprocessableEntity, zzSessionAbsent)
}

// TestZZReviewOfflineShape は offline-stream の未知 ID が 422 で、パス型のみを検証する契約を固定する。
func TestZZReviewOfflineShape(t *testing.T) {
	server, _ := setupVideoStreamTest(t)

	// 未知 ID → 422 文字列
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/9999/720p/offline-stream",
		http.StatusUnprocessableEntity, "Specified video_id was not found")
	// 非整数 ID → 検証エラー配列
	assertPG4Body(t, server, http.MethodGet, "/api/streams/video/abc/720p/offline-stream",
		http.StatusUnprocessableEntity, zzDetail(zzPathVideoIDParsing("abc")))
	// 非整数 ID + 不正 quality → 依存の品質例外が勝つ
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/abc/9999p/offline-stream",
		http.StatusUnprocessableEntity, "Specified quality was not found")
	// クエリは検証しない (未知クエリを付けても挙動が変わらない)
	assertPG4Error(t, server, http.MethodGet, "/api/streams/video/9999/720p/offline-stream?session_id=x&zzz=1",
		http.StatusUnprocessableEntity, "Specified video_id was not found")
}
