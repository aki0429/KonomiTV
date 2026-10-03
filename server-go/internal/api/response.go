package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// errorResponse は FastAPI / Starlette 互換のエラーレスポンスを表す。
type errorResponse struct {
	Detail string `json:"detail"`
}

// pydanticFloat64 は Pydantic v2 と同じ形式で float を JSON 出力する型。
// Pydantic v2 は整数値の float でも "1800.0" のように小数部を付けて出力する。
type pydanticFloat64 float64

// MarshalJSON は Pydantic v2 互換の浮動小数点数文字列を出力する。
func (value pydanticFloat64) MarshalJSON() ([]byte, error) {
	text := strconv.FormatFloat(float64(value), 'g', -1, 64)
	// 整数値の場合は Pydantic v2 と同じく ".0" を付与する (例: 1800.0)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return []byte(text), nil
}

// writeJSON は任意の値を JSON として書き出す。
// FastAPI の JSONResponse と同じく非 ASCII 文字をそのまま出力し、HTML エスケープを行わない。
func writeJSON(w http.ResponseWriter, status int, body any) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		// エンコードに失敗した場合は何も書かずに 500 を返す
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("{\"detail\":\"Internal Server Error\"}\n"))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

// writeError は FastAPI 互換の {"detail": "..."} 形式でエラーを書き出す。
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, errorResponse{Detail: detail})
}
