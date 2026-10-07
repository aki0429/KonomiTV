// konomitv-go は KonomiTV サーバーの Go 版エントリーポイント。
//
// 段階的な移行のため、未実装の API は Python 版サーバー (uvicorn) へリバースプロキシする。
// Python 版サーバーは server.port + 10 (デフォルト: 7010) で 127.0.0.77 にリッスンしている。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/api"
	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
	"github.com/aki0429/KonomiTV/server-go/internal/logging"
	"github.com/aki0429/KonomiTV/server-go/internal/stream"
	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

func main() {
	var (
		listenAddress = flag.String("listen", "127.0.0.77:7002", "Go 版サーバーがリッスンするアドレス")
		pythonBackend = flag.String("python-backend", "", "未移行 API の転送先 URL (省略時は http://127.0.0.77:<server.port+10>/)")
		noProxy       = flag.Bool("no-proxy", false, "Python 版サーバーへのプロキシを無効化する (完全移行後の検証用)")
		serverDirFlag = flag.String("server-dir", "", "Python 版サーバーのディレクトリ (例: <repo>/server)。省略時は自動検出する")
		debugLog      = flag.Bool("debug", false, "デバッグログを出力する (config.yaml の general.debug より優先)")
		versionFlag   = flag.Bool("version", false, "バージョン情報を表示して終了する")
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("KonomiTV version %s (server-go)\n", constants.Version)
		return
	}

	// ***** サーバー設定のロード *****

	serverDir, err := detectServerDir(*serverDirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] %v\n", err)
		os.Exit(1)
	}
	paths := constants.NewPaths(serverDir)

	cfg, err := config.Load(paths.ConfigYAMLPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] Failed to load server settings: %v\n", err)
		os.Exit(1)
	}

	// ***** プロキシ先の決定と自己転送の拒否 *****
	// 誤設定は DB やバックグラウンド処理を開始する前に停止する。
	backendURL := *pythonBackend
	if *noProxy {
		backendURL = ""
	} else if backendURL == "" {
		backendURL = cfg.BackendAPIURL()
	}
	if err := validateProxyTarget(*listenAddress, backendURL); err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] Invalid proxy configuration: %v\n", err)
		os.Exit(1)
	}

	// ***** ロガーの初期化 *****

	logLevel := slog.LevelInfo
	if *debugLog || cfg.General.Debug {
		logLevel = slog.LevelDebug
	}
	// サーバーログは標準出力と server/logs/KonomiTV-Server.log の両方に出力する
	// (Python 版と同じくクライアントのログビューアから参照できるようにするため)
	serverLogWriter, err := logging.NewRotatingWriter(paths.ServerLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Warning] Failed to open the server log file: %v\n", err)
	}
	logWriters := []io.Writer{os.Stdout}
	if serverLogWriter != nil {
		defer func() { _ = serverLogWriter.Close() }()
		logWriters = append(logWriters, serverLogWriter)
	}
	logger := slog.New(logging.NewPythonHandler(logWriters, logLevel))

	// アクセスログは標準出力と server/logs/KonomiTV-Access.log の両方に出力する
	accessLogWriters := []io.Writer{os.Stdout}
	accessLogWriter, err := logging.NewRotatingWriter(paths.AccessLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Warning] Failed to open the access log file: %v\n", err)
	} else {
		defer func() { _ = accessLogWriter.Close() }()
		accessLogWriters = append(accessLogWriters, accessLogWriter)
	}

	// ***** データベースのオープン (読み取り専用) *****

	db, err := database.OpenReadOnly(paths.DatabasePath)
	if err != nil {
		logger.Error("failed to open database", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// 書き込み用接続 (ユーザーの作成・更新・削除などで使う) 。SQLite の書き込みは直列化する。
	writeDB, err := database.OpenReadWrite(paths.DatabasePath)
	if err != nil {
		logger.Error("failed to open database for writing", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() { _ = writeDB.Close() }()

	// ***** 認証マネージャーの初期化 *****

	authManager, err := auth.New(paths, db, cfg, logger)
	if err != nil {
		logger.Error("failed to initialize auth manager", slog.Any("error", err))
		os.Exit(1)
	}

	// ***** HTTP サーバーの起動 *****

	iptvManager := iptv.New(iptv.Options{
		Config:  cfg,
		DataDir: paths.DataDir,
		Logger:  logger,
	})
	// 起動時に IPTV チャンネル一覧を取得しておく (Python 版 app.py の IPTVUtil.RefreshChannels() 相当)
	// プレイリストの取得に時間がかかるため、サーバーの起動をブロックしないようにバックグラウンドで実行する
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		iptvManager.Refresh(ctx, false)
	}()

	// シグナルを受信したらグレースフルシャットダウンする
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := api.New(api.Options{
		Config:           cfg,
		Paths:            paths,
		DB:               db,
		WriteDB:          writeDB,
		Auth:             authManager,
		Logger:           logger,
		IPTV:             iptvManager,
		PythonBackendURL: backendURL,
		AccessLogWriters: accessLogWriters,
		// メンテナンス API から呼び出されるサーバーの終了・再起動処理
		Shutdown: stop,
		Restart:  stop,
	})
	if err != nil {
		logger.Error("failed to initialize server", slog.Any("error", err))
		os.Exit(1)
	}

	// 録画視聴 (HLS) のエンコーダーを登録する (VideoEncodingTask 相当)
	server.SetVideoSegmentEncoderFactory(videostream.NewSessionSegmentEncoderFactory(videostream.EncodingTaskOptions{
		LibraryPath: paths.LibraryPath,
		IsHWEncCOptionAvailable: func(encoderType string, option string) bool {
			return stream.HWEncCOptionAvailable(paths.LibraryPath(encoderType), option)
		},
		Logger: logger,
	}))

	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// ストリーミング配信があるため WriteTimeout は設定しない
	}

	go func() {
		logger.Info(
			"KonomiTV server (Go) started",
			slog.String("version", constants.Version),
			slog.String("listen", httpServer.Addr),
			slog.String("backend", backendURL),
			slog.String("server_dir", paths.ServerDir),
			slog.String("backend_type", cfg.General.Backend),
			slog.String("encoder", cfg.General.Encoder),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", slog.Any("error", err))
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server gracefully", slog.Any("error", err))
	}
	logger.Info("server stopped")
}

// detectServerDir は Python 版サーバーのディレクトリ (server/) を検出する。
//
// 優先順位:
//  1. -server-dir フラグ
//  2. KONOMITV_SERVER_DIR 環境変数
//  3. カレントディレクトリまたは実行ファイルのディレクトリから上方に辿り、
//     config.yaml と server/app/ の両方が存在するリポジトリルートを探す
func detectServerDir(flagValue string) (string, error) {
	if flagValue != "" {
		absolute, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("invalid server-dir: %w", err)
		}
		if !isDirectory(absolute) {
			return "", fmt.Errorf("server-dir does not exist: %s", absolute)
		}
		return absolute, nil
	}
	if envValue := os.Getenv("KONOMITV_SERVER_DIR"); envValue != "" {
		absolute, err := filepath.Abs(envValue)
		if err != nil {
			return "", fmt.Errorf("invalid KONOMITV_SERVER_DIR: %w", err)
		}
		if !isDirectory(absolute) {
			return "", fmt.Errorf("KONOMITV_SERVER_DIR does not exist: %s", absolute)
		}
		return absolute, nil
	}

	// リポジトリルートの探索開始地点 (カレントディレクトリと実行ファイルのディレクトリ)
	starts := []string{}
	if workingDirectory, err := os.Getwd(); err == nil {
		starts = append(starts, workingDirectory)
	}
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}

	for _, start := range starts {
		if repoRoot := findRepoRoot(start); repoRoot != "" {
			return filepath.Join(repoRoot, "server"), nil
		}
	}

	return "", errors.New(
		"failed to detect the KonomiTV server directory. " +
			"Run this binary from the repository (or pass -server-dir <repo>/server)",
	)
}

// findRepoRoot は start から上方に辿り、リポジトリルート (config.yaml と server/app/ が存在するディレクトリ) を探す。
func findRepoRoot(start string) string {
	directory, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for depth := 0; depth < 6; depth++ {
		hasConfig := isRegularFile(filepath.Join(directory, "config.yaml"))
		hasServer := isDirectory(filepath.Join(directory, "server", "app"))
		if hasConfig && hasServer {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory || strings.TrimSpace(parent) == "" {
			break
		}
		directory = parent
	}
	return ""
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
