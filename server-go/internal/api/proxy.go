package api

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newProxy は未移行の API リクエストを Python 版サーバーへ転送するリバースプロキシを生成する。
// backendURL が空の場合は nil を返し、プロキシは無効となる。
func newProxy(backendURL string, logger *slog.Logger) (http.Handler, error) {
	if backendURL == "" {
		return nil, nil
	}
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// ストリーミング (TS 配信や SSE) のレスポンスをバッファリングせず即座に転送する
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error(
			"proxy error",
			slog.String("path", r.URL.Path),
			slog.String("backend", backendURL),
			slog.Any("error", err),
		)
		writeError(w, http.StatusBadGateway, "Bad Gateway")
	}
	// プロキシ経由のリクエストであることをバックエンドに伝える
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Header.Set("X-Forwarded-Proto", "http")
	}
	// タイムアウトは設定しない (ストリーミング配信があるため) 。
	// バックエンドへ接続できない場合は ErrorHandler が 502 を返す。
	return proxy, nil
}
