package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// Store は録画メタデータ解析が参照・更新する DB 操作をまとめたもの。
// 書き込みは writeDB (接続数 1) に対して行う。
type Store struct {
	// DB は読み取り用の接続 (read-only) 。
	DB *sql.DB
	// WriteDB は書き込み用の接続。nil の場合は DB を使う (テスト用) 。
	WriteDB *sql.DB
	// Now は現在時刻を返す関数 (テストで差し替えられる) 。
	Now func() time.Time
}

// writeDB は書き込みに使う接続を返す。
func (s *Store) writeDB() *sql.DB {
	if s.WriteDB != nil {
		return s.WriteDB
	}
	return s.DB
}

// now は現在時刻を返す。
func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// VideoSummary は既存の RecordedVideo レコードの必要最小限の情報。
type VideoSummary struct {
	ID                int64
	FilePath          string
	CreatedAt         time.Time
	RecordedProgramID int64
	Status            string
	FileCreatedAt     time.Time
	FileModifiedAt    time.Time
	FileSize          int64
	FileHash          string
}

// ErrVideoNotFound は録画ファイルに対応する RecordedVideo が存在しない場合のエラー。
var ErrVideoNotFound = errors.New("recorded video not found")

// videoSummaryColumns は VideoSummary の SELECT で使うカラム一覧。
const videoSummaryColumns = `
	id, file_path, created_at, recorded_program_id, status,
	file_created_at, file_modified_at, file_size, file_hash
`

// ListAllVideoSummaries は全ての RecordedVideo のサマリーを返す。
// 移植元: RecordedScanTask.runBatchScan() の all_video_rows
func (s *Store) ListAllVideoSummaries(ctx context.Context) ([]VideoSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT`+videoSummaryColumns+`FROM recorded_videos ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded videos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	summaries := []VideoSummary{}
	for rows.Next() {
		summary, err := scanVideoSummary(rows.Scan)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, *summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded videos: %w", err)
	}
	return summaries, nil
}

// FindVideoSummariesByPaths は指定されたファイルパスの RecordedVideo サマリーを返す (最大 1 件) 。
// 移植元: RecordedScanTask.processRecordedFile() の summary_rows 取得処理
func (s *Store) FindVideoSummariesByPaths(ctx context.Context, paths []string) ([]VideoSummary, error) {
	if len(paths) == 0 {
		return []VideoSummary{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, 0, len(paths))
	for _, path := range paths {
		args = append(args, path)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT`+videoSummaryColumns+`FROM recorded_videos WHERE file_path IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to find recorded videos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	summaries := []VideoSummary{}
	for rows.Next() {
		summary, err := scanVideoSummary(rows.Scan)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, *summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded videos: %w", err)
	}
	return summaries, nil
}

// scanVideoSummary は recorded_videos のサマリー行を読み込む。
func scanVideoSummary(scan func(dest ...any) error) (*VideoSummary, error) {
	var (
		summary                                  VideoSummary
		createdAt, fileCreatedAt, fileModifiedAt sql.NullString
	)
	if err := scan(
		&summary.ID, &summary.FilePath, &createdAt, &summary.RecordedProgramID, &summary.Status,
		&fileCreatedAt, &fileModifiedAt, &summary.FileSize, &summary.FileHash,
	); err != nil {
		return nil, err
	}
	var err error
	if summary.CreatedAt, err = database.ParseDBTime(createdAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if summary.FileCreatedAt, err = database.ParseDBTime(fileCreatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_created_at: %w", err)
	}
	if summary.FileModifiedAt, err = database.ParseDBTime(fileModifiedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_modified_at: %w", err)
	}
	return &summary, nil
}

// VideoRow はバックグラウンド解析対象の RecordedVideo の情報。
type VideoRow struct {
	ID                int64
	RecordedProgramID int64
	FilePath          string
	FileHash          string
	Duration          float64
	CMSections        *string
}

// ListVideosForBackgroundAnalysis は status='Recorded' の録画ファイル一覧を返す。
// 移植元: MaintenanceRouter.BackgroundAnalysisAPI() の video_rows
func (s *Store) ListVideosForBackgroundAnalysis(ctx context.Context) ([]VideoRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, recorded_program_id, file_path, file_hash, duration, cm_sections
		 FROM recorded_videos WHERE status = 'Recorded' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded videos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []VideoRow{}
	for rows.Next() {
		var (
			row        VideoRow
			cmSections sql.NullString
		)
		if err := rows.Scan(&row.ID, &row.RecordedProgramID, &row.FilePath, &row.FileHash, &row.Duration, &cmSections); err != nil {
			return nil, err
		}
		if cmSections.Valid {
			value := cmSections.String
			row.CMSections = &value
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded videos: %w", err)
	}
	return result, nil
}

// UpdateCMSections は指定されたファイルパスの RecordedVideo の cm_sections を更新する。
// 移植元: CMSectionsDetector.detectAndSave() の DB 保存処理
func (s *Store) UpdateCMSections(ctx context.Context, filePath string, sections []CMSection) error {
	_, err := s.writeDB().ExecContext(ctx,
		`UPDATE recorded_videos SET cm_sections = ?, updated_at = ? WHERE file_path = ?`,
		CMSectionsJSON(sections), database.FormatDBTime(s.now()), filePath,
	)
	if err != nil {
		return fmt.Errorf("failed to update cm_sections: %w", err)
	}
	return nil
}

// UpdateThumbnailInfo は指定されたファイルパスの RecordedVideo の thumbnail_info を更新する。
// 移植元: ThumbnailGenerator.__saveThumbnailInfoToDB()
func (s *Store) UpdateThumbnailInfo(ctx context.Context, filePath string, info ThumbnailInfo) error {
	payload, err := marshalThumbnailInfo(info)
	if err != nil {
		return err
	}
	_, err = s.writeDB().ExecContext(ctx,
		`UPDATE recorded_videos SET thumbnail_info = ?, updated_at = ? WHERE file_path = ?`,
		payload, database.FormatDBTime(s.now()), filePath,
	)
	if err != nil {
		return fmt.Errorf("failed to update thumbnail_info: %w", err)
	}
	return nil
}

// UpdateVideoStatus は RecordedVideo のステータスを更新する。
// 移植元: RecordedScanTask.processRecordedFile() の AnalysisFailed 更新
func (s *Store) UpdateVideoStatus(ctx context.Context, id int64, status string) error {
	_, err := s.writeDB().ExecContext(ctx,
		`UPDATE recorded_videos SET status = ?, updated_at = ? WHERE id = ?`,
		status, database.FormatDBTime(s.now()), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	return nil
}

// DeleteRecordedProgram は指定された録画番組を削除する (CASCADE で RecordedVideo も削除される) 。
// 移植元: RecordedScanTask の重複レコード削除・存在しないファイルのレコード削除
func (s *Store) DeleteRecordedProgram(ctx context.Context, programID int64) error {
	if _, err := s.writeDB().ExecContext(ctx, `DELETE FROM recorded_videos WHERE recorded_program_id = ?`, programID); err != nil {
		return fmt.Errorf("failed to delete recorded video: %w", err)
	}
	if _, err := s.writeDB().ExecContext(ctx, `DELETE FROM recorded_programs WHERE id = ?`, programID); err != nil {
		return fmt.Errorf("failed to delete recorded program: %w", err)
	}
	return nil
}

// KnownCollisionFileHashes は既知のハッシュ衝突が発生しうる file_hash の集合 (RecordedScanTask.KNOWN_COLLISION_FILE_HASHES) 。
var KnownCollisionFileHashes = []string{"d1dd210d6b1312cb342b56d02bd5e651"}

// CollisionVideoRow はハッシュ衝突の再解析対象の録画ファイル。
type CollisionVideoRow struct {
	Status   string
	FilePath string
	FileHash string
}

// ListCollisionVideos は既知のハッシュ衝突が発生しうる録画ファイルを返す。
func (s *Store) ListCollisionVideos(ctx context.Context) ([]CollisionVideoRow, error) {
	if len(KnownCollisionFileHashes) == 0 {
		return []CollisionVideoRow{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(KnownCollisionFileHashes)), ",")
	args := make([]any, 0, len(KnownCollisionFileHashes))
	for _, hash := range KnownCollisionFileHashes {
		args = append(args, hash)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT status, file_path, file_hash FROM recorded_videos WHERE file_hash IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list collision videos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []CollisionVideoRow{}
	for rows.Next() {
		var row CollisionVideoRow
		if err := rows.Scan(&row.Status, &row.FilePath, &row.FileHash); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate collision videos: %w", err)
	}
	return result, nil
}

// CountAnalysisFailed は status='AnalysisFailed' の録画ファイル数を返す。
func (s *Store) CountAnalysisFailed(ctx context.Context) (int64, error) {
	var count int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recorded_videos WHERE status = 'AnalysisFailed'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count analysis failed videos: %w", err)
	}
	return count, nil
}

// ListRecordedVideoHashes は全ての RecordedVideo の file_hash を返す。
func (s *Store) ListRecordedVideoHashes(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT file_hash FROM recorded_videos`)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded video hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hashes := []string{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded video hashes: %w", err)
	}
	return hashes, nil
}

// SaveRecordedMetadata は録画メタデータ解析結果を DB に保存する (新規作成または更新) 。
// 移植元: RecordedScanTask.__saveRecordedMetadataToDB()
//
// existingProgramID が 0 の場合は RecordedProgram を新規作成し、それ以外は既存の録画番組を更新する。
func (s *Store) SaveRecordedMetadata(ctx context.Context, program *RecordedProgram, existingProgramID int64) (int64, error) {
	writeDB := s.writeDB()
	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Channel の保存 (まだ当該チャンネルが DB に存在しない場合のみ)
	var channelID *string
	if program.Channel != nil {
		id, err := s.saveChannel(ctx, tx, program.Channel)
		if err != nil {
			return 0, err
		}
		channelID = id
	}

	now := database.FormatDBTime(s.now())
	var programID int64
	if existingProgramID != 0 {
		programID = existingProgramID
		_, err = tx.ExecContext(ctx, `
			UPDATE recorded_programs SET
				recording_start_margin = ?, recording_end_margin = ?, is_partially_recorded = ?,
				channel_id = ?, network_id = ?, service_id = ?, event_id = ?, series_id = ?, series_broadcast_period_id = ?,
				title = ?, series_title = ?, episode_number = ?, subtitle = ?, description = ?, detail = ?,
				start_time = ?, end_time = ?, duration = ?, is_free = ?, genres = ?,
				primary_audio_type = ?, primary_audio_language = ?, secondary_audio_type = ?, secondary_audio_language = ?,
				updated_at = ?
			WHERE id = ?`,
			program.RecordingStartMargin, program.RecordingEndMargin, boolToInt(program.IsPartiallyRecorded),
			channelID, program.NetworkID, program.ServiceID, program.EventID, program.SeriesID, program.SeriesBroadcastPeriodID,
			program.Title, program.SeriesTitle, program.EpisodeNumber, program.Subtitle, program.Description, detailJSON(program.Detail),
			database.FormatDBTime(program.StartTime), database.FormatDBTime(program.EndTime), program.Duration,
			boolToInt(program.IsFree), genresJSON(program.Genres),
			program.PrimaryAudioType, program.PrimaryAudioLanguage, program.SecondaryAudioType, program.SecondaryAudioLanguage,
			now, programID,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to update recorded program: %w", err)
		}
	} else {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO recorded_programs (
				recording_start_margin, recording_end_margin, is_partially_recorded,
				channel_id, network_id, service_id, event_id, series_id, series_broadcast_period_id,
				title, series_title, episode_number, subtitle, description, detail,
				start_time, end_time, duration, is_free, genres,
				primary_audio_type, primary_audio_language, secondary_audio_type, secondary_audio_language,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			program.RecordingStartMargin, program.RecordingEndMargin, boolToInt(program.IsPartiallyRecorded),
			channelID, program.NetworkID, program.ServiceID, program.EventID, program.SeriesID, program.SeriesBroadcastPeriodID,
			program.Title, program.SeriesTitle, program.EpisodeNumber, program.Subtitle, program.Description, detailJSON(program.Detail),
			database.FormatDBTime(program.StartTime), database.FormatDBTime(program.EndTime), program.Duration,
			boolToInt(program.IsFree), genresJSON(program.Genres),
			program.PrimaryAudioType, program.PrimaryAudioLanguage, program.SecondaryAudioType, program.SecondaryAudioLanguage,
			now, now,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to insert recorded program: %w", err)
		}
		if programID, err = result.LastInsertId(); err != nil {
			return 0, fmt.Errorf("failed to get recorded program ID: %w", err)
		}
	}

	// RecordedVideo の保存または更新
	video := program.Video
	existingVideoID, err := s.findVideoIDByPath(ctx, tx, video.FilePath)
	if err != nil {
		return 0, err
	}
	if existingVideoID != 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE recorded_videos SET
				recorded_program_id = ?, status = ?, file_hash = ?, file_size = ?,
				file_created_at = ?, file_modified_at = ?, recording_start_time = ?, recording_end_time = ?,
				duration = ?, container_format = ?, video_codec = ?, video_codec_profile = ?, video_scan_type = ?,
				video_frame_rate = ?, video_resolution_width = ?, video_resolution_height = ?, has_video_stream_changes = ?,
				primary_audio_codec = ?, primary_audio_channel = ?, primary_audio_sampling_rate = ?,
				secondary_audio_codec = ?, secondary_audio_channel = ?, secondary_audio_sampling_rate = ?,
				key_frames = '[]', segment_map = '[]', cm_sections = NULL,
				updated_at = ?
			WHERE id = ?`,
			programID, video.Status, video.FileHash, video.FileSize,
			database.FormatDBTime(video.FileCreatedAt), database.FormatDBTime(video.FileModifiedAt),
			formatNullableDBTime(video.RecordingStartTime), formatNullableDBTime(video.RecordingEndTime),
			video.Duration, video.ContainerFormat, video.VideoCodec, video.VideoCodecProfile, video.VideoScanType,
			video.VideoFrameRate, video.VideoResolutionWidth, video.VideoResolutionHeight, boolToInt(video.HasVideoStreamChanges),
			video.PrimaryAudioCodec, video.PrimaryAudioChannel, video.PrimaryAudioSamplingRate,
			video.SecondaryAudioCodec, video.SecondaryAudioChannel, video.SecondaryAudioSamplingRate,
			now, existingVideoID,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to update recorded video: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO recorded_videos (
				recorded_program_id, status, file_path, file_hash, file_size,
				file_created_at, file_modified_at, recording_start_time, recording_end_time, duration,
				container_format, video_codec, video_codec_profile, video_scan_type,
				video_frame_rate, video_resolution_width, video_resolution_height, has_video_stream_changes,
				primary_audio_codec, primary_audio_channel, primary_audio_sampling_rate,
				secondary_audio_codec, secondary_audio_channel, secondary_audio_sampling_rate,
				key_frames, segment_map, cm_sections, thumbnail_info, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', NULL, NULL, ?, ?)`,
			programID, video.Status, video.FilePath, video.FileHash, video.FileSize,
			database.FormatDBTime(video.FileCreatedAt), database.FormatDBTime(video.FileModifiedAt),
			formatNullableDBTime(video.RecordingStartTime), formatNullableDBTime(video.RecordingEndTime),
			video.Duration, video.ContainerFormat, video.VideoCodec, video.VideoCodecProfile, video.VideoScanType,
			video.VideoFrameRate, video.VideoResolutionWidth, video.VideoResolutionHeight, boolToInt(video.HasVideoStreamChanges),
			video.PrimaryAudioCodec, video.PrimaryAudioChannel, video.PrimaryAudioSamplingRate,
			video.SecondaryAudioCodec, video.SecondaryAudioChannel, video.SecondaryAudioSamplingRate,
			now, now,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to insert recorded video: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return programID, nil
}

// findVideoIDByPath は指定されたファイルパスの RecordedVideo の ID を返す (存在しない場合は 0) 。
func (s *Store) findVideoIDByPath(ctx context.Context, tx *sql.Tx, filePath string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM recorded_videos WHERE file_path = ? LIMIT 1`, filePath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to find recorded video: %w", err)
	}
	return id, nil
}

// saveChannel はチャンネル情報を保存する (存在しない場合のみ新規作成) 。
// 移植元: RecordedScanTask.__saveRecordedMetadataToDB() の Channel 保存処理
func (s *Store) saveChannel(ctx context.Context, tx *sql.Tx, channel *Channel) (*string, error) {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE id = ?`, channel.ID).Scan(&existingID)
	if err == nil {
		// 既存チャンネルに TSID がない場合だけ、録画メタデータから得た値で補完する
		if channel.TransportStreamID != nil {
			var current *int
			if err := tx.QueryRowContext(ctx, `SELECT transport_stream_id FROM channels WHERE id = ?`, channel.ID).Scan(&current); err == nil {
				if current == nil {
					if _, err := tx.ExecContext(ctx,
						`UPDATE channels SET transport_stream_id = ? WHERE id = ?`,
						*channel.TransportStreamID, channel.ID); err != nil {
						return nil, fmt.Errorf("failed to update transport_stream_id: %w", err)
					}
				}
			}
		}
		return &channel.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("failed to look up channel: %w", err)
	}

	// 録画専用の地デジチャンネルは枝番を再計算する
	saved := *channel
	if saved.Type == "GR" && !saved.IsWatchable {
		recalculated, err := s.calculateRecordingOnlyChannelNumber(ctx, tx, &saved)
		if err != nil {
			return nil, err
		}
		saved.ChannelNumber = recalculated
		saved.DisplayChannelID = strings.ToLower(saved.Type) + recalculated
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO channels (
			id, display_channel_id, network_id, service_id, transport_stream_id, remocon_id,
			channel_number, type, name, jikkyo_force, is_subchannel, is_radiochannel, is_watchable
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		saved.ID, saved.DisplayChannelID, saved.NetworkID, saved.ServiceID, saved.TransportStreamID, saved.RemoconID,
		saved.ChannelNumber, saved.Type, saved.Name, saved.JikkyoForce,
		boolToInt(saved.IsSubchannel), boolToInt(saved.IsRadiochannel), boolToInt(saved.IsWatchable),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to insert channel: %w", err)
	}
	return &saved.ID, nil
}

// calculateRecordingOnlyChannelNumber は録画専用の地デジチャンネルのチャンネル番号を再計算する。
// 移植元: TSInformation.calculateChannelNumber() の GR (録画番組向け) 分岐
func (s *Store) calculateRecordingOnlyChannelNumber(ctx context.Context, tx *sql.Tx, channel *Channel) (string, error) {
	// 地デジのサービス ID からサービス番号のみを取得する (1~8)
	sameNetworkIDCount := (channel.ServiceID & 0x0007) + 1
	// 上2桁はリモコン番号から、下1桁は同じネットワーク内にあるサービスのカウント
	channelNumber := fmt.Sprintf("%02d%d", channel.RemoconID, sameNetworkIDCount)

	// 同じベースチャンネル番号を持つサービスを DB から取得 (地デジのみ) 。
	// 既存の枝番も考慮し、最初に空いている枝番を割り当てる。
	rows, err := tx.QueryContext(ctx,
		`SELECT channel_number FROM channels
		 WHERE type = 'GR' AND NOT (network_id = ? AND service_id = ?)
		   AND (channel_number = ? OR channel_number LIKE ? ESCAPE '\')`,
		channel.NetworkID, channel.ServiceID, channelNumber, escapeLike(channelNumber+"-")+"%")
	if err != nil {
		return "", fmt.Errorf("failed to query same channel numbers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	isBaseUsed := false
	usedBranchNumbers := map[int]bool{}
	for rows.Next() {
		var sameChannelNumber string
		if err := rows.Scan(&sameChannelNumber); err != nil {
			return "", err
		}
		if sameChannelNumber == channelNumber {
			isBaseUsed = true
			continue
		}
		prefix := channelNumber + "-"
		if strings.HasPrefix(sameChannelNumber, prefix) {
			var branchNumber int
			if _, err := fmt.Sscanf(sameChannelNumber[len(prefix):], "%d", &branchNumber); err == nil {
				usedBranchNumbers[branchNumber] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("failed to iterate same channel numbers: %w", err)
	}
	if isBaseUsed || len(usedBranchNumbers) > 0 {
		branchNumber := 1
		for usedBranchNumbers[branchNumber] {
			branchNumber++
		}
		channelNumber += "-" + fmt.Sprint(branchNumber)
	}
	return channelNumber, nil
}

// escapeLike は LIKE 検索用の文字列をエスケープする。
func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

// boolToInt は bool を SQLite の INTEGER (0/1) に変換する。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// formatNullableDBTime は NULL 許容の日時を DB に保存する文字列に変換する。
func formatNullableDBTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := database.FormatDBTime(*value)
	return &formatted
}

// JSTNow は現在時刻を JST で返す。
func JSTNow() time.Time {
	return time.Now().In(constants.JST)
}

// ResolveRecordedPath はシンボリックリンクを解決して実体のパスを返す (解決に失敗した場合は元のパス) 。
// 移植元: RecordedScanTask.resolveRecordedPath()
func ResolveRecordedPath(filePath string) string {
	resolved, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		return filePath
	}
	if absolute, err := filepath.Abs(resolved); err == nil {
		return absolute
	}
	return resolved
}
