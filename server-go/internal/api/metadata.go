package api

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
)

// このファイルは録画メタデータ解析系 API の Go 実装を提供する。
//
// 移植元:
//   - server/app/routers/VideosRouter.py の VideoReanalyzeAPI() / VideoThumbnailRegenerateAPI()
//   - server/app/routers/MaintenanceRouter.py の BatchScanAPI() / BackgroundAnalysisAPI()
//
// 解析エンジン本体は internal/metadata パッケージにある。
// ルート登録は registerMetadataRoutes() で行う (server.go の Handler() から呼び出す) 。

// newMetadataService は Server の DB・設定から録画メタデータ解析サービスを構築する。
func (s *Server) newMetadataService() *metadata.Service {
	store := &metadata.Store{DB: s.db, WriteDB: s.writeDB}
	analyzer := &metadata.Analyzer{
		// FFprobe は KonomiTV 同梱のライブラリを使う (Python 版 MetadataAnalyzer と同じ引数で起動する) 。
		FFprobePath: s.paths.LibraryPath("FFprobe"),
		Logger:      s.logger,
		// 188-byte MPEG-TS の SDT/EIT/TOT を bounded・stateless に解析する。
		// PSI/SI 書庫 (.psc) と解析失敗時は従来のファイル名フォールバックを使う。
		ProgramAnalyzer: metadata.NewTSInfoProgramAnalyzer(s.db, s.logger),
	}
	return &metadata.Service{
		Store: store,
		Config: metadata.ServiceConfig{
			RecordedFolders:  s.config.Video.RecordedFolders,
			ExcludeScanPaths: s.config.Video.ExcludeScanPaths,
			ThumbnailsDir:    s.paths.ThumbnailsDir,
		},
		Analyzer: analyzer,
		Thumbnails: &metadata.ThumbnailGenerator{
			Store:         store,
			ThumbnailsDir: s.paths.ThumbnailsDir,
			// サムネイル画像の実生成は、同梱 FFmpeg の tile フィルタと libwebp で行う。
			// Linux の ffmpeg.elf は同じディレクトリの共有ライブラリを必要とするため、
			// そのディレクトリを LD_LIBRARY_PATH として渡す。
			Renderer: &metadata.FFMpegThumbnailRenderer{
				FFmpegPath:  s.paths.LibraryPath("FFmpeg"),
				LibraryPath: filepath.Dir(s.paths.LibraryPath("FFmpeg")),
				Logger:      s.logger,
			},
			Logger: s.logger,
		},
		Logger: s.logger,
	}
}

// registerMetadataRoutes は録画メタデータ解析系の 4 ルートを mux に登録する。
// 移植元: server/app/routers/VideosRouter.py・MaintenanceRouter.py
func (s *Server) registerMetadataRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/videos/{video_id}/reanalyze", s.handleMetadataVideoReanalyze)
	mux.HandleFunc("POST /api/videos/{video_id}/thumbnail/regenerate", s.handleMetadataVideoThumbnailRegenerate)
	mux.HandleFunc("POST /api/maintenance/run-batch-scan", s.handleMetadataBatchScan)
	mux.HandleFunc("POST /api/maintenance/run-background-analysis", s.handleMetadataBackgroundAnalysis)
}

// handleMetadataVideoReanalyze は録画番組メタデータ再解析 API を処理する。
// 移植元: VideosRouter.VideoReanalyzeAPI()
func (s *Server) handleMetadataVideoReanalyze(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	service := s.newMetadataService()
	err := service.ProcessRecordedFile(r.Context(), detail.Video.FilePath, metadata.ProcessOptions{
		ForceUpdate: true,
	})
	if err != nil {
		s.logger.Error("[VideoReanalyzeAPI] Failed to reanalyze the video.", "video_id", videoID, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to reanalyze the video: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMetadataVideoThumbnailRegenerate はサムネイル画像再生成 API を処理する。
// 移植元: VideosRouter.VideoThumbnailRegenerateAPI()
func (s *Server) handleMetadataVideoThumbnailRegenerate(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	service := s.newMetadataService()
	// DriveIOLimiter (同一 HDD への同時実行制限) は Go 版では未移植。
	err := service.Thumbnails.GenerateAndSave(r.Context(), detail.Video.FilePath, detail.Video.Duration)
	if err != nil {
		s.logger.Error("[VideoThumbnailRegenerateAPI] Failed to regenerate thumbnails.", "video_id", videoID, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to regenerate thumbnails: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMetadataBatchScan は録画フォルダ一括スキャン API を処理する。
// 移植元: MaintenanceRouter.BatchScanAPI()
func (s *Server) handleMetadataBatchScan(w http.ResponseWriter, r *http.Request) {
	service := s.newMetadataService()
	_, err := service.RunBatchScan(r.Context())
	if err == metadata.ErrBatchScanRunning {
		writeError(w, http.StatusTooManyRequests, "Batch scan of recording folders is already running")
		return
	}
	if err != nil {
		s.logger.Error("[BatchScanAPI] Failed to run batch scan.", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to run batch scan: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMetadataBackgroundAnalysis はバックグラウンド解析タスク手動実行 API を処理する。
// 移植元: MaintenanceRouter.BackgroundAnalysisAPI()
func (s *Server) handleMetadataBackgroundAnalysis(w http.ResponseWriter, r *http.Request) {
	service := s.newMetadataService()
	if err := service.RunBackgroundAnalysis(r.Context()); err != nil {
		s.logger.Error("[BackgroundAnalysisAPI] Failed to run background analysis.", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to run background analysis: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
