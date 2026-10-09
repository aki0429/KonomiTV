package api

import (
	"encoding/json"
	"fmt"
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
func (v *fastapiValidation) appendIntParsing(loc []any, input string) {
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
		v.appendIntParsing([]any{"path", name}, value)
		return 0, false
	}
	return parsed, true
}

// queryString は必須のクエリ文字列を返す。欠落している場合は missing を積んで空文字を返す。
// Python 側の型が str のときは空文字も有効値なので、空文字と欠落を区別する。
func (v *fastapiValidation) queryString(name string) string {
	values, ok := v.query[name]
	if !ok {
		v.details = append(v.details, missingDetail([]any{"query", name}, nil))
		return ""
	}
	// FastAPI (Starlette) は同名パラメータが複数ある場合に最後の値を採用する
	return lastQueryValue(values)
}

// queryInt は必須のクエリ整数を返す。欠落は missing、解析不能は int_parsing を積む。
func (v *fastapiValidation) queryInt(name string) int64 {
	value, present := v.queryIntValue(name)
	if !present {
		v.details = append(v.details, missingDetail([]any{"query", name}, nil))
		return 0
	}
	return value
}

// queryIntDefault は既定値つきのクエリ整数を返す。
// Python 側が既定値を持つパラメータ (page など) は欠落しても検証エラーにならないため、
// missing を積まずに既定値を返す。
func (v *fastapiValidation) queryIntDefault(name string, defaultValue int64) int64 {
	value, present := v.queryIntValue(name)
	if !present {
		return defaultValue
	}
	return value
}

// queryIntValue はクエリ整数を解析する。戻り値の 2 番目は指定の有無。
//
// Pydantic の文字列 → int 変換は前後の空白・符号・アンダースコア区切り・
// ゼロ小数部 (1.0) を許容する。同じ文法を番組検索の検証 (searchJSONInteger) が
// 実装済みなので、そこへ文字列を 1 つ渡して再利用する。
func (v *fastapiValidation) queryIntValue(name string) (int64, bool) {
	values, ok := v.query[name]
	if !ok {
		return 0, false
	}
	raw := lastQueryValue(values)
	parsed, err := searchJSONInteger(json.RawMessage(strconv.Quote(strings.TrimSpace(raw))))
	if err != nil {
		v.appendIntParsing([]any{"query", name}, raw)
		return 0, true
	}
	if !parsed.IsInt64() {
		// Python の int は上限が無く、int64 を超える値でも受け付けて後段の
		// 「該当する位置が無い」判定に落ちる。同じ結果になる最大値へ寄せる。
		return math.MaxInt64, true
	}
	return parsed.Int64(), true
}

// greaterThanEqualDetail は Pydantic の greater_than_equal エラーを生成する。
//
// Python 側の Query(ge=...) 違反は ctx に下限値を持つため、ctx.ge まで一致させる。
// input は解析前の生文字列 (Pydantic は変換前の値をそのまま載せる) 。
func greaterThanEqualDetail(loc []any, input string, ge int) validationDetail {
	lowerBound := ge
	return validationDetail{
		Type:  "greater_than_equal",
		Loc:   loc,
		Msg:   fmt.Sprintf("Input should be greater than or equal to %d", ge),
		Input: input,
		Ctx:   &validationContext{Ge: &lowerBound},
	}
}

// lessThanEqualDetail は Pydantic の less_than_equal エラーを生成する。
func lessThanEqualDetail(loc []any, input string, le int) validationDetail {
	upperBound := le
	return validationDetail{
		Type:  "less_than_equal",
		Loc:   loc,
		Msg:   fmt.Sprintf("Input should be less than or equal to %d", le),
		Input: input,
		Ctx:   &validationContext{Le: &upperBound},
	}
}

// queryIntRange は既定値つき int クエリを範囲制約つきで検証する。
//
// Python 側の `Query(ge=..., le=...) = 既定値` に対応する。欠落はエラーにせず既定値を返し、
// 解析不能は int_parsing、範囲外は greater_than_equal / less_than_equal を積む。
// ge / le に nil を渡した側の制約は無効になる。違反時は既定値を返し、
// 呼び出し側の後続処理 (ページング等) が破綻しないようにする。
func (v *fastapiValidation) queryIntRange(name string, defaultValue int64, ge *int, le *int) int64 {
	values, present := v.query[name]
	if !present {
		return defaultValue
	}
	// 範囲判定に使う input は解析前の生文字列 (Pydantic は変換前の値をそのまま載せる)
	raw := lastQueryValue(values)
	before := len(v.details)
	value := v.queryIntDefault(name, defaultValue)
	if len(v.details) > before {
		// int_parsing を積んだ。範囲判定は行わず既定値で継続する。
		return defaultValue
	}
	if ge != nil && value < int64(*ge) {
		v.details = append(v.details, greaterThanEqualDetail([]any{"query", name}, raw, *ge))
		return defaultValue
	}
	if le != nil && value > int64(*le) {
		v.details = append(v.details, lessThanEqualDetail([]any{"query", name}, raw, *le))
		return defaultValue
	}
	return value
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
	v.details = append(v.details, literalDetail([]any{"query", name}, raw, expected))
	return ""
}

// queryIntList は list[int] | None のクエリパラメータを解析する。
//
// FastAPI は list[int] を同名パラメータの繰り返し (ids=1&ids=2) として受け取り、
// カンマ区切りは受け付けない ("1,2" は int_parsing になる) 。
// 解析できない要素は loc に要素インデックスを足した int_parsing を積む
// (例: {"loc": ["query", "ids", 1]}) 。全要素を検査するため、複数の不正要素はすべて報告される。
//
// 戻り値の 2 番目は全要素が解析できたかどうか。
func (v *fastapiValidation) queryIntList(name string) ([]int64, bool) {
	values, ok := v.query[name]
	if !ok {
		// 省略時の既定値は None (Go 側では nil) 。検証エラーにはしない。
		return nil, true
	}
	parsed := make([]int64, 0, len(values))
	valid := true
	for index, raw := range values {
		number, err := searchJSONInteger(json.RawMessage(strconv.Quote(strings.TrimSpace(raw))))
		if err != nil || !number.IsInt64() {
			// Python の int は上限が無いため、int64 を超える値は最大値に寄せる
			// (後段の ID 一致判定では該当なしになるため HTTP 上の結果は変わらない) 。
			if err == nil {
				parsed = append(parsed, math.MaxInt64)
				continue
			}
			v.appendIntParsing([]any{"query", name, index}, raw)
			valid = false
			continue
		}
		parsed = append(parsed, number.Int64())
	}
	return parsed, valid
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
