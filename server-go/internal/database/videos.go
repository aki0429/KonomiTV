package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RecordedProgramDetail は録画番組情報と、それに紐づく録画ファイル・チャンネルをまとめたもの。
type RecordedProgramDetail struct {
	Program RecordedProgram
	Video   RecordedVideo
	Channel *Channel
}

// ListRecordedProgramDetailsOptions は ListRecordedProgramDetails() のオプション。
type ListRecordedProgramDetailsOptions struct {
	// OrderDesc は並び順 (true なら降順) 。
	OrderDesc bool
	// IDs は対象の録画番組 ID (空の場合はすべての録画番組) 。
	IDs []int64
	// SearchCondition は検索条件の WHERE 句 (空の場合は検索しない) 。
	SearchCondition string
	// SearchParams は検索条件のパラメータ。
	SearchParams []any
	// Limit と Offset はページング (limit が 0 の場合は制限しない) 。
	Limit  int
	Offset int
}

// ListRecordedProgramDetails は録画番組の一覧を、録画ファイルとチャンネルの情報を結合して取得する。
// 移植元: server/app/routers/VideosRouter.py の VideosAPI() / VideosSearchAPI() の生 SQL クエリ
func ListRecordedProgramDetails(
	ctx context.Context,
	db *sql.DB,
	options ListRecordedProgramDetailsOptions,
) ([]RecordedProgramDetail, error) {
	order := "ASC"
	if options.OrderDesc {
		order = "DESC"
	}

	// WHERE 句を構築する
	conditions := []string{"1=1"}
	args := []any{}
	if len(options.IDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(options.IDs)), ",")
		conditions = append(conditions, "rp.id IN ("+placeholders+")")
		for _, id := range options.IDs {
			args = append(args, id)
		}
	}
	if options.SearchCondition != "" {
		conditions = append(conditions, options.SearchCondition)
		args = append(args, options.SearchParams...)
	}

	query := `
		SELECT
			rp.id, rp.recording_start_margin, rp.recording_end_margin, rp.is_partially_recorded,
			rp.channel_id, rp.network_id, rp.service_id, rp.event_id, rp.series_id, rp.series_broadcast_period_id,
			rp.title, rp.series_title, rp.episode_number, rp.subtitle, rp.description, rp.detail,
			rp.start_time, rp.end_time, rp.duration, rp.is_free, rp.genres,
			rp.primary_audio_type, rp.primary_audio_language, rp.secondary_audio_type, rp.secondary_audio_language,
			rp.created_at, rp.updated_at,
			rv.id, rv.recorded_program_id, rv.status, rv.file_path, rv.file_hash, rv.file_size,
			rv.file_created_at, rv.file_modified_at, rv.recording_start_time, rv.recording_end_time, rv.duration,
			rv.container_format, rv.video_codec, rv.video_codec_profile, rv.video_scan_type,
			rv.video_frame_rate, rv.video_resolution_width, rv.video_resolution_height, rv.has_video_stream_changes,
			rv.primary_audio_codec, rv.primary_audio_channel, rv.primary_audio_sampling_rate,
			rv.secondary_audio_codec, rv.secondary_audio_channel, rv.secondary_audio_sampling_rate,
			rv.cm_sections, rv.thumbnail_info, rv.created_at, rv.updated_at,
			ch.id, ch.display_channel_id, ch.network_id, ch.service_id, ch.transport_stream_id,
			ch.remocon_id, ch.channel_number, ch.type, ch.name, ch.jikkyo_force,
			ch.is_subchannel, ch.is_radiochannel, ch.is_watchable
		FROM recorded_programs rp
		JOIN recorded_videos rv ON rp.id = rv.recorded_program_id
		LEFT JOIN channels ch ON rp.channel_id = ch.id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY rp.start_time ` + order + `, rp.id ` + order

	if options.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, options.Limit, options.Offset)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded programs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	details := []RecordedProgramDetail{}
	for rows.Next() {
		detail, err := scanRecordedProgramDetail(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan recorded program: %w", err)
		}
		details = append(details, *detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded programs: %w", err)
	}
	return details, nil
}

// scanRecordedProgramDetail は録画番組 + 録画ファイル + チャンネルの結合行を読み込む。
func scanRecordedProgramDetail(scan func(dest ...any) error) (*RecordedProgramDetail, error) {
	var (
		program                                           RecordedProgram
		video                                             RecordedVideo
		isPartiallyRecorded, isFree                       int64
		hasVideoStreamChanges                             int64
		channelID                                         sql.NullString
		networkID, serviceID, eventID, seriesID, periodID sql.NullInt64
		seriesTitle, episodeNumber, subtitle              sql.NullString
		secondaryAudioType, secondaryAudioLanguage        sql.NullString
		startTime, endTime, createdAt, updatedAt          sql.NullString
		cmSections, thumbnailInfo                         sql.NullString
		videoRecordingStartTime, videoRecordingEndTime    sql.NullString
		fileCreatedAt, fileModifiedAt                     sql.NullString
		videoCreatedAt, videoUpdatedAt                    sql.NullString
		secondaryAudioCodec, secondaryAudioChannel        sql.NullString
		secondaryAudioSamplingRate                        sql.NullInt64
		channelIDJoined, displayChannelID                 sql.NullString
		channelNetworkID, channelServiceID                sql.NullInt64
		transportStreamID, remoconID, jikkyoForce         sql.NullInt64
		channelNumber, channelType, channelName           sql.NullString
		isSubchannel, isRadiochannel, isWatchable         sql.NullInt64
	)
	err := scan(
		&program.ID, &program.RecordingStartMargin, &program.RecordingEndMargin, &isPartiallyRecorded,
		&channelID, &networkID, &serviceID, &eventID, &seriesID, &periodID,
		&program.Title, &seriesTitle, &episodeNumber, &subtitle, &program.Description, &program.Detail,
		&startTime, &endTime, &program.Duration, &isFree, &program.Genres,
		&program.PrimaryAudioType, &program.PrimaryAudioLanguage, &secondaryAudioType, &secondaryAudioLanguage,
		&createdAt, &updatedAt,
		&video.ID, &video.RecordedProgramID, &video.Status, &video.FilePath, &video.FileHash, &video.FileSize,
		&fileCreatedAt, &fileModifiedAt, &videoRecordingStartTime, &videoRecordingEndTime, &video.Duration,
		&video.ContainerFormat, &video.VideoCodec, &video.VideoCodecProfile, &video.VideoScanType,
		&video.VideoFrameRate, &video.VideoResolutionWidth, &video.VideoResolutionHeight, &hasVideoStreamChanges,
		&video.PrimaryAudioCodec, &video.PrimaryAudioChannel, &video.PrimaryAudioSamplingRate,
		&secondaryAudioCodec, &secondaryAudioChannel, &secondaryAudioSamplingRate,
		&cmSections, &thumbnailInfo, &videoCreatedAt, &videoUpdatedAt,
		&channelIDJoined, &displayChannelID, &channelNetworkID, &channelServiceID, &transportStreamID,
		&remoconID, &channelNumber, &channelType, &channelName, &jikkyoForce,
		&isSubchannel, &isRadiochannel, &isWatchable,
	)
	if err != nil {
		return nil, err
	}

	program.IsPartiallyRecorded = isPartiallyRecorded != 0
	program.IsFree = isFree != 0
	program.ChannelID = nullableString(channelID)
	program.NetworkID = nullableInt(networkID)
	program.ServiceID = nullableInt(serviceID)
	program.EventID = nullableInt(eventID)
	program.SeriesID = nullableInt64(seriesID)
	program.SeriesBroadcastPeriodID = nullableInt64(periodID)
	program.SeriesTitle = nullableString(seriesTitle)
	program.EpisodeNumber = nullableString(episodeNumber)
	program.Subtitle = nullableString(subtitle)
	program.SecondaryAudioType = nullableString(secondaryAudioType)
	program.SecondaryAudioLanguage = nullableString(secondaryAudioLanguage)
	if program.StartTime, err = ParseDBTime(startTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse start_time: %w", err)
	}
	if program.EndTime, err = ParseDBTime(endTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse end_time: %w", err)
	}
	if program.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if program.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	video.HasVideoStreamChanges = hasVideoStreamChanges != 0
	video.SecondaryAudioCodec = nullableString(secondaryAudioCodec)
	video.SecondaryAudioChannel = nullableString(secondaryAudioChannel)
	video.SecondaryAudioSamplingRate = nullableInt(secondaryAudioSamplingRate)
	video.CMSectionsJSON = nullableString(cmSections)
	video.ThumbnailInfoJSON = nullableString(thumbnailInfo)
	if video.FileCreatedAt, err = ParseDBTime(fileCreatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_created_at: %w", err)
	}
	if video.FileModifiedAt, err = ParseDBTime(fileModifiedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_modified_at: %w", err)
	}
	if video.RecordingStartTime, err = parseOptionalTime(videoRecordingStartTime); err != nil {
		return nil, fmt.Errorf("failed to parse recording_start_time: %w", err)
	}
	if video.RecordingEndTime, err = parseOptionalTime(videoRecordingEndTime); err != nil {
		return nil, fmt.Errorf("failed to parse recording_end_time: %w", err)
	}
	if video.CreatedAt, err = ParseDBTime(videoCreatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if video.UpdatedAt, err = ParseDBTime(videoUpdatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	var channel *Channel
	if channelIDJoined.Valid {
		channel = &Channel{
			ID:                channelIDJoined.String,
			DisplayChannelID:  displayChannelID.String,
			NetworkID:         int(channelNetworkID.Int64),
			ServiceID:         int(channelServiceID.Int64),
			TransportStreamID: nullableInt(transportStreamID),
			RemoconID:         int(remoconID.Int64),
			ChannelNumber:     channelNumber.String,
			Type:              channelType.String,
			Name:              channelName.String,
			JikkyoForce:       nullableInt(jikkyoForce),
			IsSubchannel:      isSubchannel.Int64 != 0,
			IsRadiochannel:    isRadiochannel.Int64 != 0,
			IsWatchable:       isWatchable.Int64 != 0,
		}
	}

	return &RecordedProgramDetail{Program: program, Video: video, Channel: channel}, nil
}

// CountRecordedPrograms は録画番組の総数を取得する。
func CountRecordedPrograms(
	ctx context.Context,
	db *sql.DB,
	ids []int64,
	searchCondition string,
	searchParams []any,
) (int64, error) {
	query := `SELECT COUNT(*) FROM recorded_programs rp LEFT JOIN channels ch ON rp.channel_id = ch.id WHERE 1=1`
	args := []any{}
	if len(ids) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		query += ` AND rp.id IN (` + placeholders + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if searchCondition != "" {
		query += ` AND ` + searchCondition
		args = append(args, searchParams...)
	}
	var count int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count recorded programs: %w", err)
	}
	return count, nil
}

// GetRecordedProgramDetail は指定された ID の録画番組を、録画ファイルとチャンネルの情報を結合して取得する。
// 存在しない場合は (nil, nil) を返す。
func GetRecordedProgramDetail(ctx context.Context, db *sql.DB, id int64) (*RecordedProgramDetail, error) {
	details, err := ListRecordedProgramDetails(ctx, db, ListRecordedProgramDetailsOptions{IDs: []int64{id}})
	if err != nil {
		return nil, err
	}
	if len(details) == 0 {
		return nil, nil
	}
	return &details[0], nil
}

// DeleteRecordedProgram は録画番組を削除する。
// recorded_videos は CASCADE 制約でも削除されるが、foreign_keys プラグマの設定に依存しないよう明示的に削除する。
func DeleteRecordedProgram(ctx context.Context, db *sql.DB, id int64) error {
	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin the transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	if _, err := transaction.ExecContext(ctx, `DELETE FROM recorded_videos WHERE recorded_program_id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete recorded videos (%d): %w", id, err)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM recorded_programs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete recorded program (%d): %w", id, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("failed to commit the transaction: %w", err)
	}
	return nil
}

// CountRecordedProgramsWithFileHash は指定されたファイルハッシュを持つ録画番組の数を取得する。
// excludeID で指定された録画番組は数えない。
func CountRecordedProgramsWithFileHash(ctx context.Context, db *sql.DB, fileHash string, excludeID int64) (int64, error) {
	var count int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM recorded_programs rp
		JOIN recorded_videos rv ON rp.id = rv.recorded_program_id
		WHERE rv.file_hash = ? AND rp.id != ?
	`, fileHash, excludeID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count recorded programs with the file hash: %w", err)
	}
	return count, nil
}

// parseOptionalTime は NULL 許容の日時カラムを *time.Time に変換する。
func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := ParseDBTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
