package api

import (
	"net/http"
	"strings"
)

// 設定可能な CORS のパラメーター (server/app/app.py の CORSMiddleware 設定と一致させること) 。
const (
	corsCredentials = "true"
	corsMaxAge      = "600"
	// ALL_METHODS (Starlette の cors.py と一致) 。
	corsAllowMethods = "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT, QUERY"
	// エラー時にもクライアントが読める必要があるヘッダー。
	corsExposeHeaders = "Accept-Ranges, Content-Length, Content-Range, ETag, Last-Modified"
)

// corsMiddleware は Starlette の CORSMiddleware と同等の CORS 処理を行う。
//
// KonomiTV の設定では以下の組み合わせになる:
//   - debug 時: allow_origins=["*"], allow_methods=["*"], allow_headers=["*"], allow_credentials=true
//   - 本番時: allow_origins=["https://app.konomi.tv"], それ以外は同上
//
// allow_credentials=true かつ allow_origins=["*"] の場合、Starlette はリクエストの Origin を
// そのままエコーバックする (ブラウザはワイルドカード + credentials を拒否するため) 。
func corsMiddleware(next http.Handler, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowedOrigin := ""
		originAllowed := false
		if origin != "" {
			if debug || origin == "https://app.konomi.tv" {
				allowedOrigin = origin
				originAllowed = true
			}
		}

		// プリフライトリクエスト
		if origin != "" && r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			header := w.Header()
			header.Set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers, Access-Control-Request-Private-Network")
			header.Set("Access-Control-Allow-Methods", corsAllowMethods)
			header.Set("Access-Control-Max-Age", corsMaxAge)
			header.Set("Access-Control-Allow-Credentials", corsCredentials)
			if originAllowed {
				header.Set("Access-Control-Allow-Origin", allowedOrigin)
				// allow_headers=["*"] のため、リクエストされたヘッダーをそのままエコーする
				if requestedHeaders := r.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
					header.Set("Access-Control-Allow-Headers", requestedHeaders)
				}
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}

		// 通常のリクエスト
		if origin != "" {
			header := w.Header()
			header.Set("Access-Control-Allow-Credentials", corsCredentials)
			header.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			if originAllowed {
				header.Set("Access-Control-Allow-Origin", allowedOrigin)
			}
			header.Set("Vary", strings.Join(append(header.Values("Vary"), "Origin"), ", "))
		}
		next.ServeHTTP(w, r)
	})
}
