package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// FileAnalyzer は録画ファイルを解析して番組情報を返す。
// 本番では *Analyzer 、テストではフェイクを差し込む。
type FileAnalyzer interface {
	Analyze(ctx context.Context, path string) (*RecordedProgram, error)
}

// 録画スキャンタスクの定数 (RecordedScanTask) 。
const (
	// ScanTargetExtensions はスキャン対象の拡張子。
	ScanTargetExtensions = ".ts,.m2t,.m2ts,.mts,.mp4"
	// updateThrottleSeconds は録画中ファイルの更新イベントを間引く間隔 (秒) 。
	updateThrottleSeconds = 30
	// RecordingCompleteSeconds は録画完了と判断するまでの無更新時間 (秒) 。
	RecordingCompleteSeconds = 15
	// recordingMaxAgeSeconds は録画中と判断する最大の経過時間 (秒) 。
	recordingMaxAgeSeconds = 300
	// MinimumRecordingSeconds は録画中ファイルの最小データ長 (秒) 。
	MinimumRecordingSeconds = 60
	// continuousUpdateThresholdSeconds は継続更新を録画中と判断する最小時間 (秒) 。
	continuousUpdateThresholdSeconds = 60
	// continuousUpdateMaxSeconds は継続更新を強制的に完了とする時間 (秒) 。
	continuousUpdateMaxSeconds = 86400
)

// FileRecordingInfo は録画中ファイルの状態 (RecordedScanTask.FileRecordingInfo) 。
type FileRecordingInfo struct {
	LastModified           time.Time
	LastChecked            time.Time
	FileSize               int64
	MTIMEContinuousStartAt *time.Time
}

// ServiceConfig は録画スキャンタスクの設定。
type ServiceConfig struct {
	// RecordedFolders は録画ファイルが保存されているフォルダの一覧。
	RecordedFolders []string
	// ExcludeScanPaths は録画フォルダのスキャンから除外するパスの一覧。
	ExcludeScanPaths []string
	// ThumbnailsDir はサムネイル画像ディレクトリ。
	ThumbnailsDir string
}

// Service は録画フォルダのスキャン・メタデータ解析・バックグラウンド解析を行う。
// 移植元: RecordedScanTask (+ MaintenanceRouter の run-background-analysis)
type Service struct {
	// Store は DB 操作。
	Store *Store
	// Config は録画スキャンの設定。
	Config ServiceConfig
	// Analyzer は録画ファイルの解析器。
	Analyzer FileAnalyzer
	// Thumbnails はサムネイル生成器 (nil の場合はサムネイル生成をスキップ) 。
	Thumbnails *ThumbnailGenerator
	// Logger はログ出力先。
	Logger *slog.Logger

	mu             sync.Mutex
	recordingFiles map[string]*FileRecordingInfo
}

// logger は slog.Logger を返す (未指定の場合は標準ロガー) 。
func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// now は現在時刻を返す。
func (s *Service) now() time.Time {
	if s.Store != nil && s.Store.Now != nil {
		return s.Store.Now()
	}
	return time.Now()
}

// batchScanGuardMu / batchScanGuardRunning は録画フォルダ一括スキャンの排他ガード。
//
// ハンドラー (internal/api) はリクエストごとに新しい Service を生成するため、
// ガードを Service インスタンスに持たせると並行リクエストが各々スキャンを実行できてしまう。
// 移植元の Python 実装 (MaintenanceRouter.py のモジュールグローバル batch_scan_task) と同様に、
// プロセスで共有されるスコープで排他する。
var (
	batchScanGuardMu      sync.Mutex
	batchScanGuardRunning bool
)

// RunBatchScan は録画フォルダ以下の一括スキャンと DB への同期を実行する。
// 移植元: RecordedScanTask.runBatchScan()
//
// 既に一括スキャンを実行中 (他リクエストが生成した Service インスタンスを含む) の場合は
// (ErrBatchScanRunning, false) を返す (API では 429 に対応) 。
func (s *Service) RunBatchScan(ctx context.Context) (bool, error) {
	batchScanGuardMu.Lock()
	if batchScanGuardRunning {
		batchScanGuardMu.Unlock()
		return false, ErrBatchScanRunning
	}
	batchScanGuardRunning = true
	batchScanGuardMu.Unlock()
	defer func() {
		batchScanGuardMu.Lock()
		batchScanGuardRunning = false
		batchScanGuardMu.Unlock()
	}()

	s.logger().Info("Batch scan of recording folders has been started.")

	// 現在登録されている全ての RecordedVideo レコードのサマリーを取得する
	allVideoRows, err := s.Store.ListAllVideoSummaries(ctx)
	if err != nil {
		return false, err
	}
	videosByPath := map[string][]VideoSummary{}
	videosToKeep := []VideoSummary{}
	for _, row := range allVideoRows {
		videosByPath[row.FilePath] = append(videosByPath[row.FilePath], row)
	}

	// 同一ファイルパスに対応するレコードが複数存在する場合、最新のものを保持して残りを削除する
	s.logger().Info("Checking for duplicate recorded video records...")
	duplicatesFound := false
	for _, videos := range videosByPath {
		if len(videos) > 1 {
			duplicatesFound = true
			sort.SliceStable(videos, func(left int, right int) bool {
				return videos[left].CreatedAt.After(videos[right].CreatedAt)
			})
			latest := videos[0]
			videosToKeep = append(videosToKeep, latest)
			for _, toDelete := range videos[1:] {
				if err := s.Store.DeleteRecordedProgram(ctx, toDelete.RecordedProgramID); err != nil {
					s.logger().Error("Failed to delete duplicate record.",
						"file_path", toDelete.FilePath, "error", err)
				}
			}
		} else {
			videosToKeep = append(videosToKeep, videos[0])
		}
	}
	if duplicatesFound {
		s.logger().Info("Duplicate record cleanup finished.")
	} else {
		s.logger().Info("No duplicate records found.")
	}

	// 旧 key_frames を segment_map へ移行する処理 (__migrateKeyFramesToSegmentMap) は
	// videostream パッケージ側の関数に依存するため Go 版では未移植。
	// 既存レコードの key_frames はそのまま保持される。

	// 保持すると判断されたレコードのみを使い、ファイルパスを正規化する
	existingDBRecordedVideos := map[string]VideoSummary{}
	for _, video := range videosToKeep {
		video.FilePath = ResolveRecordedPath(video.FilePath)
		existingDBRecordedVideos[video.FilePath] = video
	}

	excludeScanPaths := normalizeExcludePaths(s.Config.ExcludeScanPaths)

	// 各録画フォルダをスキャン
	s.logger().Info("Scanning recorded folders...")
	processed := map[string]bool{}
	for _, folder := range s.Config.RecordedFolders {
		err := filepath.WalkDir(folder, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				// 権限エラーなどは該当エントリのみスキップする
				return nil
			}
			if strings.HasPrefix(entry.Name(), "._") {
				return nil
			}
			// 除外パターンのチェック (シンボリックリンク解決前)
			if isExcluded(normalizePathForPrefixMatch(path), excludeScanPaths) {
				return nil
			}
			// シンボリックリンクを含むパスは実体に解決して処理する
			canonical := ResolveRecordedPath(path)
			// 除外パターンのチェック (シンボリックリンク解決後)
			if isExcluded(normalizePathForPrefixMatch(canonical), excludeScanPaths) {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !isScanTarget(canonical) {
				return nil
			}
			if !isRegularFile(canonical) {
				return nil
			}
			if processed[canonical] {
				return nil
			}
			processed[canonical] = true
			if err := s.ProcessRecordedFile(ctx, canonical, ProcessOptions{
				OriginalPath:             path,
				ExistingDBRecordedVideos: existingDBRecordedVideos,
			}); err != nil {
				s.logger().Error("Failed to process recorded file.", "file_path", path, "error", err)
			}
			return nil
		})
		if err != nil {
			s.logger().Error("Failed to walk recorded folder.", "folder", folder, "error", err)
		}
	}

	// 存在しない録画ファイルに対応するレコードを一括削除
	s.logger().Info("Deleting records for non-existent files...")
	for filePath, summary := range existingDBRecordedVideos {
		if !isRegularFile(filePath) {
			if err := s.Store.DeleteRecordedProgram(ctx, summary.RecordedProgramID); err != nil {
				s.logger().Error("Failed to delete record for non-existent file.", "file_path", filePath, "error", err)
				continue
			}
			s.logger().Info("Deleted record for non-existent file.", "file_path", filePath)
		}
	}

	// サムネイルフォルダ内の不要なファイルを削除
	if err := s.deleteOrphanedThumbnails(ctx); err != nil {
		s.logger().Error("Failed to delete orphaned thumbnail files.", "error", err)
	}

	// かつてのバグでファイルハッシュが衝突している録画ファイルのメタデータを再解析する
	collisionRows, err := s.Store.ListCollisionVideos(ctx)
	if err != nil {
		return false, err
	}
	processedCollisionPaths := map[string]bool{}
	if len(collisionRows) > 0 {
		s.logger().Info("Found videos affected by known hash collisions. Reanalyzing...", "count", len(collisionRows))
		for _, row := range collisionRows {
			if processedCollisionPaths[row.FilePath] || row.Status == "Recording" {
				continue
			}
			if !isRegularFile(row.FilePath) {
				continue
			}
			if err := s.ProcessRecordedFile(ctx, row.FilePath, ProcessOptions{ForceUpdate: true}); err != nil {
				s.logger().Error("Failed to reanalyze known hash collision file.", "file_path", row.FilePath, "error", err)
				continue
			}
			processedCollisionPaths[row.FilePath] = true
		}
	}

	// メタデータ解析に失敗した録画ファイルの数をログ出力
	if count, err := s.Store.CountAnalysisFailed(ctx); err == nil && count > 0 {
		s.logger().Warn("Batch scan completed with files in AnalysisFailed status.", "count", count)
	}
	s.logger().Info("Batch scan of recording folders has been completed.")
	return true, nil
}

// ErrBatchScanRunning は一括スキャンが既に実行中であることを表す。
var ErrBatchScanRunning = fmt.Errorf("batch scan of recording folders is already running")

// ProcessOptions は ProcessRecordedFile のオプション。
type ProcessOptions struct {
	// OriginalPath はシンボリックリンクなどで取得した元のファイルパス。
	OriginalPath string
	// ExistingDBRecordedVideos は既に DB に永続化されている録画ファイルのサマリー (nil の場合は DB から取得) 。
	ExistingDBRecordedVideos map[string]VideoSummary
	// ForceUpdate は既に DB に登録されている録画ファイルを強制的に再解析する。
	ForceUpdate bool
}

// ProcessRecordedFile は指定された録画ファイルのメタデータを解析し、DB に永続化する。
// 移植元: RecordedScanTask.processRecordedFile()
func (s *Service) ProcessRecordedFile(ctx context.Context, filePath string, options ProcessOptions) error {
	filePath = ResolveRecordedPath(filePath)
	originalPath := ""
	if options.OriginalPath != "" {
		originalPath = ResolveRecordedPath(options.OriginalPath)
	}

	if !isRegularFile(filePath) {
		s.logger().Warn("File does not exist after acquiring lock! ignored.", "file_path", filePath)
		return nil
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	now := s.now()
	fileSize := fileInfo.Size()
	fileCreatedAt := fileCreationTime(fileInfo)
	fileModifiedAt := fileInfo.ModTime().In(constants.JST)

	if fileSize == 0 {
		s.logger().Warn("File size is 0. ignored.", "file_path", filePath)
		return nil
	}

	// 同じファイルパスの既存レコードのサマリーを取り出す
	var existing *VideoSummary
	if options.ExistingDBRecordedVideos != nil {
		if summary, ok := options.ExistingDBRecordedVideos[filePath]; ok {
			existing = &summary
		} else if originalPath != "" {
			if summary, ok := options.ExistingDBRecordedVideos[originalPath]; ok {
				existing = &summary
			}
		}
	}
	if existing == nil {
		queryPaths := []string{filePath}
		if originalPath != "" && originalPath != filePath {
			queryPaths = append(queryPaths, originalPath)
		}
		rows, err := s.Store.FindVideoSummariesByPaths(ctx, queryPaths)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			summary := rows[0]
			summary.FilePath = filePath
			existing = &summary
		}
	}

	// ファイルの基本情報が前回と一致する場合はスキップ (録画中はスキップしない)
	s.mu.Lock()
	recordingInfo, isRecording := s.recordingFiles[filePath]
	s.mu.Unlock()
	if !options.ForceUpdate && existing != nil && existing.Status == "Recorded" &&
		existing.FileCreatedAt.Equal(fileCreatedAt) &&
		existing.FileModifiedAt.Equal(fileModifiedAt) &&
		existing.FileSize == fileSize {
		return nil
	}

	// 現在録画中とマークされているファイルの処理
	if isRecording {
		if existing != nil && existing.Status == "Recording" {
			return nil
		}
		if recordingInfo != nil && fileSize == recordingInfo.FileSize {
			if recordingInfo.MTIMEContinuousStartAt == nil {
				s.logger().Warn("File is not recording. ignored.", "file_path", filePath)
				return nil
			}
			continuousDuration := now.Sub(*recordingInfo.MTIMEContinuousStartAt).Seconds()
			if continuousDuration < continuousUpdateThresholdSeconds || continuousDuration >= continuousUpdateMaxSeconds {
				return nil
			}
		}
	}

	// メタデータを解析
	program, err := s.Analyzer.Analyze(ctx, filePath)
	if err != nil {
		s.logger().Error("Error analyzing metadata.", "file_path", filePath, "error", err)
		if existing != nil {
			if err := s.Store.UpdateVideoStatus(ctx, existing.ID, "AnalysisFailed"); err != nil {
				s.logger().Error("Failed to update status.", "file_path", filePath, "error", err)
			}
		}
		s.forgetRecordingFile(filePath)
		return nil
	}
	if program == nil {
		s.logger().Error("Failed to analyze metadata.", "file_path", filePath)
		if existing != nil {
			if err := s.Store.UpdateVideoStatus(ctx, existing.ID, "AnalysisFailed"); err != nil {
				s.logger().Error("Failed to update status.", "file_path", filePath, "error", err)
			}
		}
		s.forgetRecordingFile(filePath)
		return nil
	}

	// 60 秒未満のファイルは録画失敗または切り抜きとみなしてスキップ
	if program.Video.Duration < MinimumRecordingSeconds {
		s.logger().Debug("This file is too short. Skipped.",
			"file_path", filePath, "duration", program.Video.Duration)
		return nil
	}

	// メタデータ解析後に再度ファイルパスに対応するレコードを取得する
	var existingAfter []VideoSummary
	queryPaths := []string{filePath}
	if originalPath != "" && originalPath != filePath {
		queryPaths = append(queryPaths, originalPath)
	}
	if rows, err := s.Store.FindVideoSummariesByPaths(ctx, queryPaths); err == nil && len(rows) > 0 {
		existingAfter = rows
	}
	var existingProgramID int64
	if len(existingAfter) > 0 {
		if !options.ForceUpdate && existingAfter[0].Status == "Recorded" &&
			existingAfter[0].FileHash == program.Video.FileHash {
			return nil
		}
		existingProgramID = existingAfter[0].RecordedProgramID
	}

	// 録画中のファイルとして処理
	if isRecording || now.Sub(fileModifiedAt).Seconds() < RecordingCompleteSeconds {
		program.Video.Status = "Recording"
		continuousStart := fileModifiedAt
		s.mu.Lock()
		if s.recordingFiles == nil {
			s.recordingFiles = map[string]*FileRecordingInfo{}
		}
		s.recordingFiles[filePath] = &FileRecordingInfo{
			LastModified:           fileModifiedAt,
			LastChecked:            now,
			FileSize:               fileSize,
			MTIMEContinuousStartAt: &continuousStart,
		}
		s.mu.Unlock()
	} else {
		program.Video.Status = "Recorded"
	}

	// DB に永続化
	if _, err := s.Store.SaveRecordedMetadata(ctx, program, existingProgramID); err != nil {
		return err
	}
	s.logger().Info("Saved metadata to DB.", "file_path", filePath, "status", program.Video.Status)

	// 録画完了後のバックグラウンド解析タスクを開始
	// Python 版は asyncio.create_task() で非同期に実行するが、Go 版は HDD への同時アクセスを避けるため
	// ProcessRecordedFile 内で直列に実行する (外部から見た最終状態は同じ) 。
	if program.Video.Status == "Recorded" {
		s.runBackgroundAnalysis(ctx, filePath, program.Video.Duration)
	}
	return nil
}

// forgetRecordingFile は録画中ファイルの状態を削除する。
func (s *Service) forgetRecordingFile(filePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recordingFiles, filePath)
}

// runBackgroundAnalysis は録画完了後のバックグラウンド解析 (CM 区間検出・サムネイル生成) を実行する。
// 移植元: RecordedScanTask.__runBackgroundAnalysis()
func (s *Service) runBackgroundAnalysis(ctx context.Context, filePath string, durationSec float64) {
	s.logger().Info("Starting background analysis task...", "file_path", filePath)

	// CM 区間検出: チャプターファイルから取得して DB に保存する
	sections := detectCMSections(filePath, durationSec)
	if err := s.Store.UpdateCMSections(ctx, filePath, sections); err != nil {
		s.logger().Error("Failed to save CM sections.", "file_path", filePath, "error", err)
	}

	// サムネイル生成
	if s.Thumbnails != nil {
		if err := s.Thumbnails.GenerateAndSave(ctx, filePath, durationSec); err != nil {
			s.logger().Error("Failed to generate thumbnails.", "file_path", filePath, "error", err)
		}
	}
	s.logger().Info("Background analysis task completed.", "file_path", filePath)
}

// RunBackgroundAnalysis は CM 区間情報やサムネイルが未生成の録画ファイルに対して解析を実行する。
// 移植元: MaintenanceRouter.BackgroundAnalysisAPI() の BackgroundAnalysis()
func (s *Service) RunBackgroundAnalysis(ctx context.Context) error {
	s.logger().Info("Manual background analysis has started.")
	rows, err := s.Store.ListVideosForBackgroundAnalysis(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !isRegularFile(row.FilePath) {
			s.logger().Warn("File not found. Skipping...", "file_path", row.FilePath)
			continue
		}
		// CM 区間情報が未解析 (NULL) の場合のみ検出する。
		// cm_sections が [] の場合は「解析済みだが CM 区間がなかった」ことを表す。
		if row.CMSections == nil {
			sections := detectCMSections(row.FilePath, row.Duration)
			if err := s.Store.UpdateCMSections(ctx, row.FilePath, sections); err != nil {
				s.logger().Error("Failed to save CM sections.", "file_path", row.FilePath, "error", err)
				continue
			}
		}

		// サムネイルが未生成の場合は生成する
		if s.Thumbnails != nil {
			tilePath, representativePath := ThumbnailPaths(s.Config.ThumbnailsDir, row.FileHash)
			if !isRegularFile(tilePath) || !isRegularFile(representativePath) {
				if err := s.Thumbnails.GenerateAndSave(ctx, row.FilePath, row.Duration); err != nil {
					s.logger().Error("Failed to generate thumbnails.", "file_path", row.FilePath, "error", err)
					continue
				}
			}
		}
	}
	s.logger().Info("Manual background analysis has finished processing all recorded files.")
	return nil
}

// deleteOrphanedThumbnails は DB に存在しないハッシュのサムネイルファイルを削除する。
// 移植元: RecordedScanTask.runBatchScan() の不要なサムネイル削除処理
func (s *Service) deleteOrphanedThumbnails(ctx context.Context) error {
	hashes, err := s.Store.ListRecordedVideoHashes(ctx)
	if err != nil {
		return err
	}
	knownHashes := map[string]bool{}
	for _, hash := range hashes {
		knownHashes[hash] = true
	}
	directory := s.Config.ThumbnailsDir
	if directory == "" || !isDirectory(directory) {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".git") || entry.IsDir() {
			continue
		}
		extension := filepath.Ext(entry.Name())
		if extension != ".webp" {
			continue
		}
		fileHash := strings.TrimSuffix(entry.Name(), extension)
		fileHash = strings.TrimSuffix(fileHash, "_tile")
		if knownHashes[fileHash] {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if err := os.Remove(path); err != nil {
			s.logger().Error("Error deleting orphaned thumbnail file.", "path", path, "error", err)
			continue
		}
		s.logger().Info("Deleted orphaned thumbnail file.", "name", entry.Name())
	}
	return nil
}

// normalizeExcludePaths は除外パターンの一覧を正規化する (空文字列は除外) 。
func normalizeExcludePaths(patterns []string) []string {
	result := []string{}
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		result = append(result, normalizePathForPrefixMatch(pattern))
	}
	return result
}

// normalizePathForPrefixMatch は前方一致用にパス区切りを統一する。
// 移植元: RecordedScanTask.__normalizePathForPrefixMatch()
func normalizePathForPrefixMatch(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

// isExcluded は正規化済みパスが除外パターンのいずれかに前方一致するかを返す。
func isExcluded(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(path, pattern) {
			return true
		}
	}
	return false
}

// isScanTarget は指定されたパスがスキャン対象の拡張子かを返す。
func isScanTarget(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	for _, target := range strings.Split(ScanTargetExtensions, ",") {
		if extension == target {
			return true
		}
	}
	return false
}

// isRegularFile は指定されたパスが存在する通常のファイルかを返す。
func isRegularFile(path string) bool {
	fileInfo, err := os.Stat(path)
	return err == nil && fileInfo.Mode().IsRegular()
}

// isDirectory は指定されたパスがディレクトリかを返す。
func isDirectory(path string) bool {
	fileInfo, err := os.Stat(path)
	return err == nil && fileInfo.IsDir()
}
