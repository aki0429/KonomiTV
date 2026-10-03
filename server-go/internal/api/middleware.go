package api

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/aki0429/KonomiTV/server-go/internal/logging"
)

// statusRecorder はレスポンスのステータスコードを記録する ResponseWriter。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// Flush はストリーミング配信のために基になる ResponseWriter をフラッシュする。
// これを実装しないと、ストリーミング系の API が http.Flusher を検出できずに失敗する。
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// accessLogMiddleware は Uvicorn のアクセスログに相当するリクエストログを出力する。
// アクセスログは Python 版と同じ形式で server/logs/KonomiTV-Access.log にも出力する。
func accessLogMiddleware(next http.Handler, writers []io.Writer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		// リクエストラインは Python 版 (uvicorn) と同じくクエリ文字列を含む
		target := r.URL.RequestURI()
		protocol := r.Proto
		if protocol == "" {
			protocol = "HTTP/1.1"
		}
		logging.AccessLog(writers, r.RemoteAddr, r.Method, target, protocol, recorder.status)
	})
}

// recoverMiddleware はハンドラー内で発生した panic を捕捉し、FastAPI 互換の 500 レスポンスを返す。
func recoverMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("panic recovered", slog.Any("error", err), slog.String("path", r.URL.Path))
				writeError(w, http.StatusInternalServerError, "Internal Server Error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
