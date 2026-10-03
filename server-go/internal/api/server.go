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
	"sync"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
)

// Server は Go 版 KonomiTV サーバーのインスタンス。
type Server struct {
	config        *config.Config
	paths         constants.Paths
	db            *sql.DB // 読み取り専用
	writeDB       *sql.DB // 書き込み用 (SQLite の書き込みを直列化するため別接続)
	auth          *auth.Manager
	logger        *slog.Logger
	iptv          *iptv.Manager
	liveStreams   *stream.Manager
	proxy         http.Handler // nil の場合はプロキシ無効
	latestVersion *latestVersionCache

	// jikkyoChannels は実況チャンネルの対応表 (初回アクセス時に読み込む) 。
	jikkyoChannels     *jikkyo.ChannelMap
	jikkyoChannelsOnce sync.Once
}

// Options は Server の生成に必要な依存関係。
type Options struct {
	Config  *config.Config
	Paths   constants.Paths
	DB      *sql.DB
	WriteDB *sql.DB
	Auth    *auth.Manager
	Logger  *slog.Logger
	// IPTV は IPTV チャンネル一覧のマネージャー。nil の場合は内部で生成する。
	IPTV *iptv.Manager
	// LiveStreams はライブストリームのマネージャー。nil の場合は内部で生成する。
	LiveStreams *stream.Manager
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
	writeDB := options.WriteDB
	if writeDB == nil {
		// 書き込み用接続が指定されていない場合は読み取り用接続を使う (テスト用)
		writeDB = options.DB
	}
	iptvManager := options.IPTV
	if iptvManager == nil {
		iptvManager = iptv.New(iptv.Options{
			Config:  options.Config,
			DataDir: options.Paths.DataDir,
			Logger:  options.Logger,
		})
	}
	liveStreams := options.LiveStreams
	if liveStreams == nil {
		liveStreams = stream.NewManager(options.Logger)
	}
	// ライブストリーミングのエンコードタスクを有効化する
	// (IPTV の疑似チャンネルは IPTV マネージャーから、それ以外のチャンネルは DB からチャンネル情報を取得する) 。
	liveStreams.EnableEncodingTasks(stream.TaskOptions{
		Config: options.Config,
		Paths:  options.Paths,
		Logger: options.Logger,
		IPTV:   iptvManager,
		DB:     options.DB,
	})
	return &Server{
		config:        options.Config,
		paths:         options.Paths,
		db:            options.DB,
		writeDB:       writeDB,
		auth:          options.Auth,
		logger:        options.Logger,
		iptv:          iptvManager,
		liveStreams:   liveStreams,
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
	mux.HandleFunc("POST /api/users", s.handleUserCreate)
	mux.HandleFunc("POST /api/users/token", s.handleUserAccessToken)
	mux.HandleFunc("GET /api/users", s.handleUsers)
	mux.HandleFunc("GET /api/users/me", s.handleUserMe)
	mux.HandleFunc("PUT /api/users/me", s.handleUserUpdate)
	mux.HandleFunc("DELETE /api/users/me", s.handleUserDelete)
	mux.HandleFunc("GET /api/users/me/icon", s.handleUserMeIcon)
	mux.HandleFunc("PUT /api/users/me/icon", s.handleUserUpdateIcon)
	mux.HandleFunc("POST /api/users/me/account-links", s.handleAccountLinkCreate)
	mux.HandleFunc("DELETE /api/users/me/account-links/{link_id}", s.handleAccountLinkDelete)
	mux.HandleFunc("GET /api/users/{username}", s.handleSpecifiedUser)
	mux.HandleFunc("PUT /api/users/{username}", s.handleSpecifiedUserUpdate)
	mux.HandleFunc("DELETE /api/users/{username}", s.handleSpecifiedUserDelete)
	mux.HandleFunc("GET /api/users/{username}/icon", s.handleSpecifiedUserIcon)

	// データ放送ブラウザ (web-bml) 向け API
	mux.HandleFunc("GET /api/data-broadcasting/internet-status", s.handleDataBroadcastingInternetStatus)

	// チャンネル
	mux.HandleFunc("GET /api/channels", s.handleChannels)
	mux.HandleFunc("GET /api/channels/{channel_id}", s.handleChannel)
	mux.HandleFunc("GET /api/channels/{channel_id}/logo", s.handleChannelLogo)
	mux.HandleFunc("GET /api/channels/{channel_id}/jikkyo", s.handleChannelJikkyo)

	// 番組表・番組検索
	// POST /api/programs/search は EDCB バックエンド専用のため、Go 側ではバックエンドのチェックのみ行う
	mux.HandleFunc("POST /api/programs/search", s.handleProgramSearch)
	mux.HandleFunc("GET /api/programs/timetable", s.handleProgramTimeTable)

	// シリーズ番組
	mux.HandleFunc("GET /api/series", s.handleSeriesList)
	mux.HandleFunc("GET /api/series/search", s.handleSeriesSearch)
	mux.HandleFunc("GET /api/series/{series_id}", s.handleSeries)

	// 設定
	mux.HandleFunc("GET /api/settings/client", s.handleClientSettings)
	mux.HandleFunc("PUT /api/settings/client", s.handleClientSettingsUpdate)
	mux.HandleFunc("GET /api/settings/server", s.handleServerSettings)
	mux.HandleFunc("PUT /api/settings/server", s.handleServerSettingsUpdate)

	// ライブストリーミング
	mux.HandleFunc("GET /api/streams/live", s.handleLiveStreams)
	mux.HandleFunc("GET /api/streams/live/{display_channel_id}/{quality}", s.handleLiveStream)
	mux.HandleFunc("GET /api/streams/live/{display_channel_id}/{quality}/events", s.handleLiveStreamEvents)
	mux.HandleFunc("GET /api/streams/live/{display_channel_id}/{quality}/psi-archived-data", s.handleLiveStreamPSIArchivedData)
	mux.HandleFunc("GET /api/streams/live/{display_channel_id}/{quality}/mpegts", s.handleLiveStreamMPEGTS)

	// IPTV (M3U プレイリストの取り込みとストリームのプロキシ)
	mux.HandleFunc("GET /api/iptv/channels", s.handleIPTVChannels)
	mux.HandleFunc("GET /api/iptv/countries", s.handleIPTVCountries)
	mux.HandleFunc("GET /api/iptv/groups", s.handleIPTVGroups)
	mux.HandleFunc("GET /api/iptv/playlist.m3u", s.handleIPTVPlaylist)
	mux.HandleFunc("GET /api/iptv/sources", s.handleIPTVSources)
	mux.HandleFunc("POST /api/iptv/sources", s.handleIPTVSourceAdd)
	mux.HandleFunc("DELETE /api/iptv/sources", s.handleIPTVSourceDelete)
	mux.HandleFunc("GET /api/iptv/tvui", s.handleIPTVTVUIChannels)
	mux.HandleFunc("POST /api/iptv/tvui", s.handleIPTVTVUIRegister)
	mux.HandleFunc("DELETE /api/iptv/tvui", s.handleIPTVTVUIUnregister)
	mux.HandleFunc("GET /api/iptv/proxy", s.handleIPTVProxy)
	mux.HandleFunc("GET /api/iptv/logo", s.handleIPTVLogo)

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
