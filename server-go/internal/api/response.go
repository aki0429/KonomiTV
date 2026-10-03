package api

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// errorResponse は FastAPI / Starlette 互換のエラーレスポンスを表す。
type errorResponse struct {
	Detail string `json:"detail"`
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
