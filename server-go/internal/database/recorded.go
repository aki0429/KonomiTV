package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Series は series テーブルのレコード (server/app/models/Series.py 互換) 。
type Series struct {
	ID          int64
	Title       string
	Description string
	Genres      string // JSON 文字列
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SeriesBroadcastPeriod は series_broadcast_periods テーブルのレコード。
type SeriesBroadcastPeriod struct {
	ID        int64
	SeriesID  int64
	ChannelID string
	StartDate string // "YYYY-MM-DD"
	EndDate   string // "YYYY-MM-DD"
}

// RecordedProgram は recorded_programs テーブルのレコード (server/app/models/RecordedProgram.py 互換) 。
type RecordedProgram struct {
	ID                      int64
	RecordingStartMargin    float64
	RecordingEndMargin      float64
	IsPartiallyRecorded     bool
	ChannelID               *string
	NetworkID               *int
	ServiceID               *int
	EventID                 *int
	SeriesID                *int64
	SeriesBroadcastPeriodID *int64
	Title                   string
	SeriesTitle             *string
	EpisodeNumber           *string
	Subtitle                *string
	Description             string
	Detail                  string // JSON 文字列
	StartTime               time.Time
	EndTime                 time.Time
	Duration                float64
	IsFree                  bool
	Genres                  string // JSON 文字列
	PrimaryAudioType        string
	PrimaryAudioLanguage    string
	SecondaryAudioType      *string
	SecondaryAudioLanguage  *string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// RecordedVideo は recorded_videos テーブルのレコード (server/app/models/RecordedVideo.py 互換) 。
type RecordedVideo struct {
	ID                         int64
	RecordedProgramID          int64
	Status                     string
	FilePath                   string
	FileHash                   string
	FileSize                   int64
	FileCreatedAt              time.Time
	FileModifiedAt             time.Time
	RecordingStartTime         *time.Time
	RecordingEndTime           *time.Time
	Duration                   float64
	ContainerFormat            string
	VideoCodec                 string
	VideoCodecProfile          string
	VideoScanType              string
	VideoFrameRate             float64
	VideoResolutionWidth       int
	VideoResolutionHeight      int
	HasVideoStreamChanges      bool
	PrimaryAudioCodec          string
	PrimaryAudioChannel        string
	PrimaryAudioSamplingRate   int
	SecondaryAudioCodec        *string
	SecondaryAudioChannel      *string
	SecondaryAudioSamplingRate *int
	// cm_sections と thumbnail_info は JSON 文字列のまま保持する (未解析の場合は空文字列)
	CMSectionsJSON    *string
	ThumbnailInfoJSON *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ErrSeriesNotFound は指定されたシリーズが存在しない場合のエラー。
var ErrSeriesNotFound = errors.New("series not found")

// seriesColumns は series テーブルの SELECT で使うカラム一覧。
const seriesColumns = `id, title, description, genres, created_at, updated_at`

// recordedProgramColumns は recorded_programs テーブルの SELECT で使うカラム一覧。
const recordedProgramColumns = `
	id, recording_start_margin, recording_end_margin, is_partially_recorded,
	channel_id, network_id, service_id, event_id, series_id, series_broadcast_period_id,
	title, series_title, episode_number, subtitle, description, detail,
	start_time, end_time, duration, is_free, genres,
	primary_audio_type, primary_audio_language, secondary_audio_type, secondary_audio_language,
	created_at, updated_at
`

// recordedVideoColumns は recorded_videos テーブルの SELECT で使うカラム一覧。
const recordedVideoColumns = `
	id, recorded_program_id, status, file_path, file_hash, file_size,
	file_created_at, file_modified_at, recording_start_time, recording_end_time, duration,
	container_format, video_codec, video_codec_profile, video_scan_type,
	video_frame_rate, video_resolution_width, video_resolution_height, has_video_stream_changes,
	primary_audio_codec, primary_audio_channel, primary_audio_sampling_rate,
	secondary_audio_codec, secondary_audio_channel, secondary_audio_sampling_rate,
	cm_sections, thumbnail_info, created_at, updated_at
`

// scanSeries は series テーブルの 1 行を Series に読み込む。
func scanSeries(scan func(dest ...any) error) (*Series, error) {
	var (
		series               Series
		createdAt, updatedAt sql.NullString
	)
	if err := scan(&series.ID, &series.Title, &series.Description, &series.Genres, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if series.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if series.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}
	return &series, nil
}

// scanRecordedProgram は recorded_programs テーブルの 1 行を RecordedProgram に読み込む。
func scanRecordedProgram(scan func(dest ...any) error) (*RecordedProgram, error) {
	var (
		program                                           RecordedProgram
		isPartiallyRecorded, isFree                       int64
		channelID                                         sql.NullString
		networkID, serviceID, eventID, seriesID, periodID sql.NullInt64
		seriesTitle, episodeNumber, subtitle              sql.NullString
		secondaryAudioType, secondaryAudioLanguage        sql.NullString
		startTime, endTime, createdAt, updatedAt          sql.NullString
	)
	err := scan(
		&program.ID, &program.RecordingStartMargin, &program.RecordingEndMargin, &isPartiallyRecorded,
		&channelID, &networkID, &serviceID, &eventID, &seriesID, &periodID,
		&program.Title, &seriesTitle, &episodeNumber, &subtitle, &program.Description, &program.Detail,
		&startTime, &endTime, &program.Duration, &isFree, &program.Genres,
		&program.PrimaryAudioType, &program.PrimaryAudioLanguage, &secondaryAudioType, &secondaryAudioLanguage,
		&createdAt, &updatedAt,
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
	return &program, nil
}

// scanRecordedVideo は recorded_videos テーブルの 1 行を RecordedVideo に読み込む。
func scanRecordedVideo(scan func(dest ...any) error) (*RecordedVideo, error) {
	var (
		video                                               RecordedVideo
		hasVideoStreamChanges                               int64
		recordingStartTime, recordingEndTime                sql.NullString
		secondaryAudioCodec, secondaryAudioChannel          sql.NullString
		secondaryAudioSamplingRate                          sql.NullInt64
		cmSections, thumbnailInfo                           sql.NullString
		fileCreatedAt, fileModifiedAt, createdAt, updatedAt sql.NullString
	)
	err := scan(
		&video.ID, &video.RecordedProgramID, &video.Status, &video.FilePath, &video.FileHash, &video.FileSize,
		&fileCreatedAt, &fileModifiedAt, &recordingStartTime, &recordingEndTime, &video.Duration,
		&video.ContainerFormat, &video.VideoCodec, &video.VideoCodecProfile, &video.VideoScanType,
		&video.VideoFrameRate, &video.VideoResolutionWidth, &video.VideoResolutionHeight, &hasVideoStreamChanges,
		&video.PrimaryAudioCodec, &video.PrimaryAudioChannel, &video.PrimaryAudioSamplingRate,
		&secondaryAudioCodec, &secondaryAudioChannel, &secondaryAudioSamplingRate,
		&cmSections, &thumbnailInfo, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	video.HasVideoStreamChanges = hasVideoStreamChanges != 0
	video.SecondaryAudioCodec = nullableString(secondaryAudioCodec)
	video.SecondaryAudioChannel = nullableString(secondaryAudioChannel)
	if secondaryAudioSamplingRate.Valid {
		value := int(secondaryAudioSamplingRate.Int64)
		video.SecondaryAudioSamplingRate = &value
	}
	if video.RecordingStartTime, err = ParseNullableDBTime(nullableString(recordingStartTime)); err != nil {
		return nil, fmt.Errorf("failed to parse recording_start_time: %w", err)
	}
	if video.RecordingEndTime, err = ParseNullableDBTime(nullableString(recordingEndTime)); err != nil {
		return nil, fmt.Errorf("failed to parse recording_end_time: %w", err)
	}
	if video.FileCreatedAt, err = ParseDBTime(fileCreatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_created_at: %w", err)
	}
	if video.FileModifiedAt, err = ParseDBTime(fileModifiedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse file_modified_at: %w", err)
	}
	if video.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if video.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}
	video.CMSectionsJSON = nullableString(cmSections)
	video.ThumbnailInfoJSON = nullableString(thumbnailInfo)
	return &video, nil
}

// nullableInt は sql.NullInt64 を *int に変換する。
func nullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

// nullableInt64 は sql.NullInt64 を *int64 に変換する。
func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

// escapeLike は LIKE 検索用の文字列をエスケープする (Tortoise の escape_like と同じ) 。
func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

// ListSeries はシリーズ番組の一覧を取得する。
// searchQuery が空でない場合は title / description の部分一致検索を行う。
// Python 版 SeriesRouter と同じ SQL (UPPER(...) LIKE UPPER(?) ESCAPE '\') を発行する。
func ListSeries(
	ctx context.Context,
	db *sql.DB,
	searchQuery string,
	orderDesc bool,
	offset int,
	limit int,
) ([]Series, int64, error) {
	order := "ASC"
	if orderDesc {
		order = "DESC"
	}
	where := ""
	countQuery := `SELECT COUNT(*) FROM series`
	listArgs := []any{}
	countArgs := []any{}
	if searchQuery != "" {
		where = ` WHERE UPPER(CAST("title" AS VARCHAR)) LIKE UPPER(?) ESCAPE '\'` +
			` OR UPPER(CAST("description" AS VARCHAR)) LIKE UPPER(?) ESCAPE '\'`
		countArgs = append(countArgs, "%"+escapeLike(searchQuery)+"%", "%"+escapeLike(searchQuery)+"%")
		listArgs = append(listArgs, countArgs...)
	}
	countQuery += where
	listQuery := `SELECT ` + seriesColumns + ` FROM series` + where +
		` ORDER BY "updated_at" ` + order + ` LIMIT ? OFFSET ?`
	listArgs = append(listArgs, limit, offset)

	seriesList := make([]Series, 0)
	rows, err := db.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		series, err := scanSeries(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to scan series row: %w", err)
		}
		seriesList = append(seriesList, *series)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to iterate series rows: %w", err)
	}

	var total int64
	if searchQuery == "" {
		if err := db.QueryRowContext(ctx, countQuery).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("failed to count series: %w", err)
		}
	} else {
		if err := db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("failed to count series: %w", err)
		}
	}
	return seriesList, total, nil
}

// ListBroadcastPeriodsBySeriesIDs は指定されたシリーズに紐づく放送期間を取得する。
func ListBroadcastPeriodsBySeriesIDs(ctx context.Context, db *sql.DB, seriesIDs []int64) ([]SeriesBroadcastPeriod, error) {
	if len(seriesIDs) == 0 {
		return []SeriesBroadcastPeriod{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(seriesIDs)), ",")
	args := make([]any, 0, len(seriesIDs))
	for _, seriesID := range seriesIDs {
		args = append(args, seriesID)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, series_id, channel_id, start_date, end_date FROM series_broadcast_periods
		 WHERE series_id IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list broadcast periods: %w", err)
	}
	defer func() { _ = rows.Close() }()

	periods := make([]SeriesBroadcastPeriod, 0)
	for rows.Next() {
		var period SeriesBroadcastPeriod
		if err := rows.Scan(&period.ID, &period.SeriesID, &period.ChannelID, &period.StartDate, &period.EndDate); err != nil {
			return nil, fmt.Errorf("failed to scan broadcast period: %w", err)
		}
		periods = append(periods, period)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate broadcast periods: %w", err)
	}
	return periods, nil
}

// ListRecordedProgramsByBroadcastPeriodIDs は指定された放送期間に紐づく録画番組を取得する。
func ListRecordedProgramsByBroadcastPeriodIDs(ctx context.Context, db *sql.DB, periodIDs []int64) ([]RecordedProgram, error) {
	if len(periodIDs) == 0 {
		return []RecordedProgram{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(periodIDs)), ",")
	args := make([]any, 0, len(periodIDs))
	for _, periodID := range periodIDs {
		args = append(args, periodID)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT`+recordedProgramColumns+`FROM recorded_programs
		 WHERE series_broadcast_period_id IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded programs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	programs := make([]RecordedProgram, 0)
	for rows.Next() {
		program, err := scanRecordedProgram(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan recorded program: %w", err)
		}
		programs = append(programs, *program)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded programs: %w", err)
	}
	return programs, nil
}

// GetRecordedVideosByProgramIDs は指定された録画番組に紐づく録画ファイルを取得する。
// 1つの番組に複数の録画ファイルがある場合は ID が一番小さいものを返す (OneToOne 相当) 。
func GetRecordedVideosByProgramIDs(ctx context.Context, db *sql.DB, programIDs []int64) (map[int64]RecordedVideo, error) {
	result := map[int64]RecordedVideo{}
	if len(programIDs) == 0 {
		return result, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(programIDs)), ",")
	args := make([]any, 0, len(programIDs))
	for _, programID := range programIDs {
		args = append(args, programID)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT`+recordedVideoColumns+`FROM recorded_videos
		 WHERE recorded_program_id IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list recorded videos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		video, err := scanRecordedVideo(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan recorded video: %w", err)
		}
		if _, exists := result[video.RecordedProgramID]; !exists {
			result[video.RecordedProgramID] = *video
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate recorded videos: %w", err)
	}
	return result, nil
}

// GetSeriesByID は指定された ID のシリーズ番組を取得する。存在しない場合は (nil, nil) を返す。
func GetSeriesByID(ctx context.Context, db *sql.DB, id int64) (*Series, error) {
	row := db.QueryRowContext(ctx, `SELECT `+seriesColumns+` FROM series WHERE id = ?`, id)
	series, err := scanSeries(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get series (%d): %w", id, err)
	}
	return series, nil
}
