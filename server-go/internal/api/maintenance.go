package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleMaintenanceLogs はサーバーログストリーミング API (GET /api/maintenance/logs/{log_type}) を処理する。
// サーバーログまたはアクセスログを Server-Sent Events で随時配信する。
// 移植元: server/app/routers/MaintenanceRouter.py の LogStreamAPI()
func (s *Server) handleMaintenanceLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}
	logType := r.PathValue("log_type")
	var logPath string
	switch logType {
	case "server":
		logPath = s.paths.ServerLogPath
	case "access":
		logPath = s.paths.AccessLogPath
	default:
		writeError(w, http.StatusUnprocessableEntity, "Invalid log_type")
		return
	}

	// ログファイルが存在しない場合はエラー
	if _, err := os.Stat(logPath); err != nil {
		s.logger.Error("[MaintenanceRouter][LogStreamAPI] Log file not found.", "path", logPath)
		writeError(w, http.StatusNotFound, fmt.Sprintf("Log file not found: %s", logPath))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming is not supported")
		return
	}

	// ファイルを開く (UTF-8 としてデコードできないバイト列は置換文字に置き換えて読み取る)
	file, err := os.Open(logPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to open the log file")
		return
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 初回接続時に全ての行を送信する (空行は除外)
	allLines := []string{}
	reader := bufio.NewReader(file)
	for {
		line, err := readLogLine(reader)
		if strings.TrimSpace(line) != "" {
			allLines = append(allLines, line)
		}
		if err != nil {
			break
		}
	}
	writeSSEEvent(w, "initial_log_update", allLines)
	flusher.Flush()

	// 継続的に新しい行を監視する
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	// sse_starlette と同じく 15 秒間隔で ping を送信する
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-pingTicker.C:
			_, _ = io.WriteString(w, ": ping - "+time.Now().Format(time.RFC3339Nano)+"\r\n\r\n")
			flusher.Flush()
		case <-ticker.C:
			// ファイルが更新されたかチェックする
			statResult, err := os.Stat(logPath)
			if err != nil {
				return
			}
			position, err := file.Seek(0, 1)
			if err != nil {
				return
			}
			if statResult.Size() <= position {
				continue
			}
			for {
				line, err := readLogLine(reader)
				if line != "" && strings.TrimSpace(line) != "" {
					writeSSEEvent(w, "log_update", line)
				}
				if err != nil {
					break
				}
			}
			flusher.Flush()
		}
	}
}

// readLogLine はログファイルから 1 行を読み取る (UTF-8 としてデコードできないバイト列は置換文字に置き換える) 。
// 行末の改行文字は取り除く。ファイル末尾に達した場合は io.EOF を返す。
func readLogLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return strings.ToValidUTF8(line, "\uFFFD"), err
}

// handleMaintenanceUpdateDatabase はデータベース更新 API (POST /api/maintenance/update-database) を処理する。
// 移植元: server/app/routers/MaintenanceRouter.py の UpdateDatabaseAPI()
func (s *Server) handleMaintenanceUpdateDatabase(w http.ResponseWriter, r *http.Request) {
	// IPTV バックエンドではチャンネル情報・番組情報を DB に保存しないため、
	// Python 版と同じく何も更新しない (プレイリストの再取得のみ行う) 。
	if s.config.General.Backend != "IPTV" {
		// プロキシ無効時は Go 版で直接更新する (Python 版と同じく失敗はログのみで 204) 。
		// プロキシ有効時は既存どおり Python 版へ転送する。
		if s.proxy == nil {
			s.updateDatabaseNative(context.WithoutCancel(r.Context()))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.proxyRequest(w, r)
		return
	}
	if s.iptv != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s.iptv.Refresh(ctx, false)
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// /api/maintenance/run-batch-scan と /api/maintenance/run-background-analysis は、
// 録画メタデータ解析系 API として metadata.go のネイティブ実装
// (handleMetadataBatchScan / handleMetadataBackgroundAnalysis) が
// registerMetadataRoutes() で登録されている (Python 版へはプロキシしない) 。

// handleMaintenanceRestart はサーバー再起動 API (POST /api/maintenance/restart) を処理する。
// 移植元: server/app/routers/MaintenanceRouter.py の ServerRestartAPI()
func (s *Server) handleMaintenanceRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireCurrentAdminUserOrLocal(w, r) {
		return
	}
	// 再起動が必要であることを示すロックファイルを作成する
	// (KonomiTV.py などのスーパーバイザーがこのファイルを確認して再起動する)
	lockPath := filepath.Join(s.paths.DataDir, "restart_required.lock")
	if err := os.MkdirAll(s.paths.DataDir, 0o755); err != nil {
		s.logger.Error("[MaintenanceRouter][ServerRestartAPI] Failed to create the data directory.", "error", err)
	}
	if err := os.WriteFile(lockPath, []byte{}, 0o644); err != nil {
		s.logger.Error("[MaintenanceRouter][ServerRestartAPI] Failed to create the restart lock file.", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
	// レスポンスを返してからサーバーを終了する
	go func() {
		time.Sleep(100 * time.Millisecond)
		if s.restart != nil {
			s.restart()
		} else if s.shutdown != nil {
			s.shutdown()
		}
	}()
}

// handleMaintenanceShutdown はサーバー終了 API (POST /api/maintenance/shutdown) を処理する。
// 移植元: server/app/routers/MaintenanceRouter.py の ServerShutdownAPI()
func (s *Server) handleMaintenanceShutdown(w http.ResponseWriter, r *http.Request) {
	if !s.requireCurrentAdminUserOrLocal(w, r) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
	go func() {
		time.Sleep(100 * time.Millisecond)
		if s.shutdown != nil {
			s.shutdown()
		}
	}()
}

// requireCurrentAdminUserOrLocal は管理者ユーザーであるか、
// 内部ポート (server.port + 10) からのアクセスであるかを確認する。
// 移植元: server/app/routers/MaintenanceRouter.py の GetCurrentAdminUserOrLocal()
func (s *Server) requireCurrentAdminUserOrLocal(w http.ResponseWriter, r *http.Request) bool {
	// HTTP リクエストの Host ヘッダーが 127.0.0.77:<server.port + 10> である場合、
	// Windows サービスプロセスからのアクセスと見なす
	validHost := fmt.Sprintf("127.0.0.77:%d", s.config.Server.Port+10)
	if strings.TrimSpace(r.Host) == validHost {
		return true
	}
	_, ok := s.requireCurrentAdminUser(w, r)
	return ok
}
