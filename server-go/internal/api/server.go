// Package api は KonomiTV の HTTP API サーバーを提供する。
//
// Python 版サーバーからの段階的な移行のため、Go 側で実装済みのルートはネイティブに処理し、
// 未移行のルートは Python 版サーバーへリバースプロキシする (ストラングラーパターン) 。
package api

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// Server は Go 版 KonomiTV サーバーのインスタンス。
type Server struct {
	config        *config.Config
	paths         constants.Paths
	db            *sql.DB
	auth          *auth.Manager
	logger        *slog.Logger
	proxy         http.Handler // nil の場合はプロキシ無効
	latestVersion *latestVersionCache
}

// Options は Server の生成に必要な依存関係。
type Options struct {
	Config *config.Config
	Paths  constants.Paths
	DB     *sql.DB
	Auth   *auth.Manager
	Logger *slog.Logger
	// PythonBackendURL は未移行 API の転送先 (例: http://127.0.0.77:7010/) 。
	// 空文字の場合はプロキシを無効化し、未実装の API は 404 を返す。
	PythonBackendURL string
}

// New は Server を生成する。
func New(options Options) (*Server, error) {
	proxy, err := newProxy(options.PythonBackendURL, options.Logger)
	if err != nil {
		return nil, err
	}
	return &Server{
		config:        options.Config,
		paths:         options.Paths,
		db:            options.DB,
		auth:          options.Auth,
		logger:        options.Logger,
		proxy:         proxy,
		latestVersion: newLatestVersionCache(options.Logger),
	}, nil
}

// Handler は HTTP ハンドラーを構成して返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ***** Go 実装済みのルート *****
	mux.HandleFunc("GET /api/version", s.handleVersion)

	// 認証 (読み取り系のみ。書き込み系は Python 版へプロキシする)
	mux.HandleFunc("POST /api/users/token", s.handleUserAccessToken)
	mux.HandleFunc("GET /api/users", s.handleUsers)
	mux.HandleFunc("GET /api/users/me", s.handleUserMe)
	mux.HandleFunc("GET /api/users/me/icon", s.handleUserMeIcon)
	mux.HandleFunc("GET /api/users/{username}", s.handleSpecifiedUser)
	mux.HandleFunc("GET /api/users/{username}/icon", s.handleSpecifiedUserIcon)

	// データ放送ブラウザ (web-bml) 向け API
	mux.HandleFunc("GET /api/data-broadcasting/internet-status", s.handleDataBroadcastingInternetStatus)

	// ***** 静的ファイル *****
	// Python 版の app.mount('/assets', StaticFiles(...)) 相当
	mux.HandleFunc("/assets/", s.handleAssets)
	// それ以外のすべて (未移行 API のプロキシと SPA 配信)
	mux.HandleFunc("/", s.handleRoot)

	// データ放送のリクエストプロキシは転送先 URL をパスに含む ("//" を含む) ため、
	// "//" を除去する ServeMux のパスクリーニングを通すことができない。
	// そのため ServeMux の手前で直接ディスパッチする。
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, dataBroadcastingProxyPrefix) {
			s.handleDataBroadcastingProxy(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// ミドルウェア (外側から アクセスログ → panic 回復 → CORS → ルーティング)
	var handler http.Handler = dispatcher
	handler = corsMiddleware(handler, s.config.General.Debug)
	handler = recoverMiddleware(handler, s.logger)
	handler = accessLogMiddleware(handler, s.logger)
	return handler
}
