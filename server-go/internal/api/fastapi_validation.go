package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// fastapiValidation は FastAPI (Pydantic) のリクエスト検証を宣言順に再現する。
//
// 移植元の FastAPI はパスパラメータとクエリパラメータの検証を 1 パスで行い、
// 最初の失敗で打ち切らず、全エラーを 1 つの配列 ({"detail": [...]}) にまとめて 422 で返す。
// さらに検証を通過した後で依存関係 (ValidateVideoID / ValidateQuality などの
// HTTPException) を実行する。Go 版が 1 件ずつ文字列 detail を返すと、
// detail の型で表示を分岐するクライアント (client/src/services/APIClient.ts) の
// 挙動が変わるため、エラーの種類・件数・順序まで合わせる。
//
// 参照: server/app/routers/VideoStreamsRouter.py の各エンドポイント定義
type fastapiValidation struct {
	// query は検証対象のクエリパラメータ。
	query url.Values
	// details は蓄積した検証エラー。パスパラメータ → クエリパラメータの宣言順に積む。
	details []validationDetail
}

// newFastAPIValidation はリクエストのクエリパラメータを保持する検証器を作る。
func newFastAPIValidation(r *http.Request) *fastapiValidation {
	return &fastapiValidation{query: r.URL.Query()}
}

// appendIntParsing は Pydantic の int_parsing エラーを積む。
// Pydantic は小数表記 (1.5) や指数表記 (1e2) も int_parsing として扱う。
func (v *fastapiValidation) appendIntParsing(loc []string, input string) {
	v.details = append(v.details, validationDetail{
		Type:  "int_parsing",
		Loc:   loc,
		Msg:   "Input should be a valid integer, unable to parse string as an integer",
		Input: input,
	})
}

// pathInt はパスパラメータの整数を返す。解析できない場合は int_parsing を積んで false を返す。
func (v *fastapiValidation) pathInt(r *http.Request, name string) (int64, bool) {
	value := r.PathValue(name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		v.appendIntParsing([]string{"path", name}, value)
		return 0, false
	}
	return parsed, true
}

// queryString は必須のクエリ文字列を返す。欠落している場合は missing を積んで空文字を返す。
// Python 側の型が str のときは空文字も有効値なので、空文字と欠落を区別する。
func (v *fastapiValidation) queryString(name string) string {
	values, ok := v.query[name]
	if !ok {
		v.details = append(v.details, missingDetail([]string{"query", name}, nil))
		return ""
	}
	// FastAPI (Starlette) は同名パラメータが複数ある場合に最後の値を採用する
	return lastQueryValue(values)
}

// queryInt は必須のクエリ整数を返す。欠落は missing、解析不能は int_parsing を積む。
//
// Pydantic の文字列 → int 変換は前後の空白・符号・アンダースコア区切り・
// ゼロ小数部 (1.0) を許容する。同じ文法を番組検索の検証 (searchJSONInteger) が
// 実装済みなので、そこへ文字列を 1 つ渡して再利用する。
func (v *fastapiValidation) queryInt(name string) int64 {
	values, ok := v.query[name]
	if !ok {
		v.details = append(v.details, missingDetail([]string{"query", name}, nil))
		return 0
	}
	raw := lastQueryValue(values)
	parsed, err := searchJSONInteger(json.RawMessage(strconv.Quote(strings.TrimSpace(raw))))
	if err != nil {
		v.appendIntParsing([]string{"query", name}, raw)
		return 0
	}
	if !parsed.IsInt64() {
		// Python の int は上限が無く、int64 を超える値でも受け付けて後段の
		// 「該当する位置が無い」判定に落ちる。同じ結果になる最大値へ寄せる。
		return math.MaxInt64
	}
	return parsed.Int64()
}

// queryLiteral は既定値つきの Literal クエリパラメータを返す。
// 値が許容外の場合は literal_error (ctx.expected つき) を積んで空文字を返す。
func (v *fastapiValidation) queryLiteral(name string, allowed []string, defaultValue string, expected string) string {
	values, ok := v.query[name]
	if !ok {
		return defaultValue
	}
	raw := lastQueryValue(values)
	for _, candidate := range allowed {
		if raw == candidate {
			return raw
		}
	}
	v.details = append(v.details, literalDetail([]string{"query", name}, raw, expected))
	return ""
}

// writeIfInvalid は検証エラーがあれば FastAPI 互換の 422 を書き出して true を返す。
// エラーが無かった場合は何も書かず false を返し、呼び出し側が依存関係の検証へ進む。
func (v *fastapiValidation) writeIfInvalid(w http.ResponseWriter) bool {
	if len(v.details) == 0 {
		return false
	}
	writeValidationDetails(w, v.details)
	return true
}
