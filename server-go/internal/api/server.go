// Package api は KonomiTV の HTTP API サーバーを提供する。
//
// Python 版サーバーからの段階的な移行のため、Go 側で実装済みのルートはネイティブに処理し、
// 未移行のルートは Python 版サーバーへリバースプロキシする (ストラングラーパターン) 。
package api

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/epgupdate"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
	"github.com/aki0429/KonomiTV/server-go/internal/torne"
	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// Server は Go 版 KonomiTV サーバーのインスタンス。
type Server struct {
	config            *config.Config
	paths             constants.Paths
	db                *sql.DB         // 読み取り専用
	writeDB           *sql.DB         // 書き込み用 (SQLite の書き込みを直列化するため別接続)
	captureCatalog    *captureCatalog // キャプチャ画像の登録・保存場所の同期
	vacuumState       maintenanceVacuumState
	recordingMoves    metadata.RecordingMoves           // 手動スキャンと常駐監視で共有する移動保留
	thumbnailRenderer *metadata.FFMpegThumbnailRenderer // 自動生成とキャプチャ差し替えの画像出力
	auth              *auth.Manager
	logger            *slog.Logger
	iptv              *iptv.Manager
	liveStreams       *stream.Manager
	videoStreams      *videostream.Manager              // 録画視聴セッション
	offlineFactory    videostream.OfflineEncoderFactory // 保存専用エンコーダー工場
	offlineSlots      chan struct{}                     // 通常視聴の余裕を残す三枠の保存上限
	proxy             http.Handler                      // nil の場合はプロキシ無効
	latestVersion     *latestVersionCache
	// lifecycleContext は手動スキャンなどを HTTP 接続ではなくサーバーの終了に結び付ける。
	lifecycleContext context.Context
	// accessLogWriters はアクセスログの出力先 (標準出力とログファイル) 。
	accessLogWriters []io.Writer
	// shutdown はサーバーを終了するための関数 (main.go から設定する) 。
	shutdown func()
	// restart はサーバーを再起動するための関数 (main.go から設定する) 。
	restart func()

	// jikkyoChannels は実況チャンネルの対応表 (初回アクセス時に読み込む) 。
	jikkyoChannels     *jikkyo.ChannelMap
	jikkyoChannelsOnce sync.Once
	// updateDatabaseMu は update-database の EDCB 更新を直列化する (SQLite の書き込み競合を避ける) 。
	updateDatabaseMu sync.Mutex
	// settingsSaveMu は並行 PUT が同じ設定ファイルの一時保存と競合するのを防ぐ。
	settingsSaveMu sync.Mutex
	// serverSettingsValidation は設定 API の実機検査をテスト時だけ差し替える。
	serverSettingsValidation *config.ServerSettingsValidationOptions
	// jikkyoStatuses は実況チャンネルごとの最新の実況勢い (Python: JikkyoClient のクラス変数) 。
	jikkyoStatuses epgupdate.JikkyoStatuses
	// toruCounts は全地域の取得に成功したトル数を保持する。
	toruCounts torne.Cache
	// edcbUpdateSource はテスト用に差し替える EDCB クライアント (nil なら config の edcb_url に接続する) 。
	edcbUpdateSource epgupdate.EDCBSource
	// mirakurunSource はテスト用に差し替える Mirakurun API (nil なら config の mirakurun_url に接続する) 。
	mirakurunSource epgupdate.MirakurunSource
	// jikkyoStatusFetch はテスト用に差し替える NX-Jikkyo のチャンネル情報取得 (nil なら HTTP で取得する) 。
	jikkyoStatusFetch epgupdate.FetchJikkyoChannels
}

// Options は Server の生成に必要な依存関係。
type Options struct {
	// LifecycleContext はサーバーの終了時にバックグラウンド処理を止める。nil はテスト向けに Background を使う。
	LifecycleContext context.Context
	Config           *config.Config
	Paths            constants.Paths
	DB               *sql.DB
	WriteDB          *sql.DB
	Auth             *auth.Manager
	Logger           *slog.Logger
	// IPTV は IPTV チャンネル一覧のマネージャー。nil の場合は内部で生成する。
	IPTV *iptv.Manager
	// LiveStreams はライブストリームのマネージャー。nil の場合は内部で生成する。
	LiveStreams *stream.Manager
	// PythonBackendURL は未移行 API の転送先 (例: http://127.0.0.77:7010/) 。
	// 空文字の場合は比較用プロキシを無効化し、未知の API は 404 を返す。
	PythonBackendURL string
	// AccessLogWriters はアクセスログの出力先 (省略時は標準出力のみ) 。
	AccessLogWriters []io.Writer
	// Shutdown はサーバーを終了するための関数 (メンテナンス API から呼び出す) 。
	Shutdown func()
	// Restart はサーバーを再起動するための関数 (メンテナンス API から呼び出す) 。
	Restart func()
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
	lifecycleContext := options.LifecycleContext
	if lifecycleContext == nil {
		lifecycleContext = context.Background()
	}
	return &Server{
		lifecycleContext: lifecycleContext,
		config:           options.Config,
		paths:            options.Paths,
		db:               options.DB,
		writeDB:          writeDB,
		captureCatalog:   &captureCatalog{db: options.DB, writeDB: writeDB, logger: options.Logger, folders: options.Config.Capture.UploadFolders},
		thumbnailRenderer: &metadata.FFMpegThumbnailRenderer{
			FrameWorkerPath:  options.Paths.ThumbnailWorkerPath(),
			FrameLibraryPath: filepath.Join(options.Paths.ThumbnailLibraryDir(), "av.libs"),
			FFmpegPath:       options.Paths.LibraryPath("FFmpeg"),
			TSReadExPath:     options.Paths.LibraryPath("tsreadex"),
			LibraryPath:      filepath.Dir(options.Paths.LibraryPath("FFmpeg")),
			Logger:           options.Logger,
		},
		auth:             options.Auth,
		logger:           options.Logger,
		iptv:             iptvManager,
		liveStreams:      liveStreams,
		videoStreams:     videostream.NewManager(options.Logger),
		offlineSlots:     make(chan struct{}, 3),
		proxy:            proxy,
		latestVersion:    newLatestVersionCache(options.Logger),
		accessLogWriters: options.AccessLogWriters,
		shutdown:         options.Shutdown,
		restart:          options.Restart,
	}, nil
}

// Handler は HTTP ハンドラーを構成して返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ***** Go 実装済みのルート *****
	mux.HandleFunc("GET /api/version", s.handleVersion)

	// 認証・アカウント作成と更新
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
	// POST /api/programs/search は EDCB の SearchPg へ接続する。
	mux.HandleFunc("POST /api/programs/search", s.handleProgramSearch)
	mux.HandleFunc("GET /api/programs/timetable", s.handleProgramTimeTable)
	mux.HandleFunc("GET /api/programs/toru-counts", s.handleToruCounts)
	mux.HandleFunc("GET /api/programs/toru-ranking", s.handleToruRanking)

	// シリーズ番組
	mux.HandleFunc("GET /api/series", s.handleSeriesList)
	mux.HandleFunc("GET /api/series/search", s.handleSeriesSearch)
	mux.HandleFunc("GET /api/series/{series_id}", s.handleSeries)

	// 録画番組
	// メタデータ再解析とサムネイル画像再生成は registerMetadataRoutes で登録する。
	mux.HandleFunc("GET /api/videos", s.handleVideos)
	mux.HandleFunc("GET /api/videos/search", s.handleVideosSearch)
	mux.HandleFunc("GET /api/videos/{video_id}", s.handleVideo)
	mux.HandleFunc("DELETE /api/videos/{video_id}", s.handleVideoDelete)
	mux.HandleFunc("GET /api/videos/{video_id}/download", s.handleVideoDownload)
	mux.HandleFunc("GET /api/videos/{video_id}/jikkyo", s.handleVideoJikkyo)
	mux.HandleFunc("GET /api/videos/{video_id}/thumbnail", s.handleVideoThumbnail)
	mux.HandleFunc("PUT /api/videos/{video_id}/thumbnail", s.handleVideoThumbnailUpdate)
	mux.HandleFunc("GET /api/videos/{video_id}/thumbnail/tiled", s.handleVideoThumbnailTile)

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

	// 録画ストリーミング。起動時に同梱エンコーダーを登録し、HLS と KTVODLP を生成する。
	mux.HandleFunc("GET /api/streams/video/{video_id}/{quality}/playlist", s.handleVideoStreamPlaylist)
	mux.HandleFunc("GET /api/streams/video/{video_id}/{quality}/segment", s.handleVideoStreamSegment)
	mux.HandleFunc("GET /api/streams/video/{video_id}/{quality}/buffer", s.handleVideoStreamBuffer)
	mux.HandleFunc("PUT /api/streams/video/{video_id}/{quality}/keep-alive", s.handleVideoStreamKeepAlive)
	mux.HandleFunc("GET /api/streams/video/{video_id}/{quality}/offline-stream", s.handleVideoStreamOffline)

	// IPTV (M3U プレイリストの取り込みとストリームのプロキシ)
	mux.HandleFunc("GET /api/iptv/channels", s.handleIPTVChannels)
	mux.HandleFunc("GET /api/iptv/countries", s.handleIPTVCountries)
	mux.HandleFunc("GET /api/iptv/groups", s.handleIPTVGroups)
	mux.HandleFunc("GET /api/iptv/playlist.m3u", s.handleIPTVPlaylist)
	mux.HandleFunc("GET /api/iptv/sources", s.handleIPTVSources)
	mux.HandleFunc("POST /api/iptv/sources", s.handleIPTVSourceAdd)
	mux.HandleFunc("DELETE /api/iptv/sources", s.handleIPTVSourceDelete)
	mux.HandleFunc("POST /api/iptv/sources/enabled", s.handleIPTVSourceEnabled)
	mux.HandleFunc("GET /api/iptv/tvui", s.handleIPTVTVUIChannels)
	mux.HandleFunc("POST /api/iptv/tvui", s.handleIPTVTVUIRegister)
	mux.HandleFunc("DELETE /api/iptv/tvui", s.handleIPTVTVUIUnregister)
	mux.HandleFunc("GET /api/iptv/proxy", s.handleIPTVProxy)
	mux.HandleFunc("GET /api/iptv/logo", s.handleIPTVLogo)
	// キャプチャ
	// 保存先フォルダのスキャン・EXIF メタデータの抽出・サムネイル生成・フォルダ管理を Go で行う
	mux.HandleFunc("GET /api/captures", s.handleCaptures)
	mux.HandleFunc("POST /api/captures", s.handleCatalogUpload)
	mux.HandleFunc("GET /api/captures/folders", s.handleCaptureFolderList)
	mux.HandleFunc("POST /api/captures/folders", s.handleCaptureFolderCreate)
	mux.HandleFunc("PUT /api/captures/folders/{folder_id}", s.handleCaptureFolderUpdate)
	mux.HandleFunc("DELETE /api/captures/folders/{folder_id}", s.handleCaptureFolderDelete)
	mux.HandleFunc("GET /api/captures/folders/{folder_id}/captures", s.handleCaptureFolderCaptureList)
	mux.HandleFunc("POST /api/captures/folders/{folder_id}/captures", s.handleCaptureFolderCaptureAdd)
	mux.HandleFunc("DELETE /api/captures/folders/{folder_id}/captures", s.handleCaptureFolderCaptureRemove)
	mux.HandleFunc("GET /api/captures/{filename}", s.handleCaptureImage)
	mux.HandleFunc("DELETE /api/captures/{filename}", s.handleCaptureDelete)
	s.registerCaptureTagRoutes(mux)
	s.registerCaptureCatalogRoutes(mux)
	s.registerMaintenanceTaskRoutes(mux)

	// メンテナンス
	// データベース更新は EDCB / Mirakurun と接続し、録画解析は metadata.go で扱う。
	mux.HandleFunc("GET /api/maintenance/logs/{log_type}", s.handleMaintenanceLogs)
	mux.HandleFunc("POST /api/maintenance/update-database", s.handleMaintenanceUpdateDatabase)
	mux.HandleFunc("POST /api/maintenance/restart", s.handleMaintenanceRestart)
	mux.HandleFunc("POST /api/maintenance/shutdown", s.handleMaintenanceShutdown)

	// 録画予約・キーワード自動予約条件・録画設定プリセット
	s.registerReservationRoutes(mux)
	s.registerReservationConditionRoutes(mux)
	s.registerRecordingPresetRoutes(mux)
	// Twitter / Bluesky / ニコニコ連携
	s.registerTwitterRoutes(mux)
	s.registerBlueskyRoutes(mux)
	s.registerNiconicoRoutes(mux)
	// 録画メタデータ解析 (reanalyze / thumbnail/regenerate / run-batch-scan / run-background-analysis)
	s.registerMetadataRoutes(mux)

	// ***** 静的ファイル *****
	// Python 版の app.mount('/assets', StaticFiles(...)) 相当
	mux.HandleFunc("/assets/", s.handleAssets)
	// それ以外のすべて (比較モードの API プロキシと SPA 配信)
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
	handler = accessLogMiddleware(handler, s.accessLogWriters)
	return handler
}
