package reservations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// このファイルは録画予約系ルーターが必要とするデータベースアクセスをまとめたもの。
//
// Python 版の ReservationsRouter / ReservationConditionsRouter は Tortoise ORM の
// Channel.filter() / Program.filter() / Channel.all() を使っており、
// internal/database にはそれらに相当する公開関数がないため、このパッケージ内に実装している。

// ErrNotFound は該当するレコードが存在しない場合のエラー。
var ErrNotFound = errors.New("record not found")

// ChannelKey はチャンネルを一意に識別するキー (network_id, transport_stream_id, service_id) 。
type ChannelKey struct {
	NetworkID         int
	TransportStreamID int
	ServiceID         int
}

// ProgramKey は番組を一意に識別するキー (network_id, service_id, event_id) 。
type ProgramKey struct {
	NetworkID int
	ServiceID int
	EventID   int
}

// ここでは internal/database の未公開のカラム定義と同じカラム順を使う。
const storeChannelColumns = `
	id, display_channel_id, network_id, service_id, transport_stream_id,
	remocon_id, channel_number, type, name, jikkyo_force,
	is_subchannel, is_radiochannel, is_watchable
`

const storeProgramColumns = `
	id, channel_id, network_id, service_id, event_id, title, description,
	detail, start_time, end_time, duration, is_free, genres,
	video_type, video_codec, video_resolution,
	primary_audio_type, primary_audio_language, primary_audio_sampling_rate,
	secondary_audio_type, secondary_audio_language, secondary_audio_sampling_rate
`

// scanChannelRow は channels テーブルの 1 行を database.Channel に読み込む。
func scanChannelRow(scan func(dest ...any) error) (*database.Channel, error) {
	var (
		channel           database.Channel
		transportStreamID sql.NullInt64
		jikkyoForce       sql.NullInt64
		isSubchannel      int64
		isRadiochannel    int64
		isWatchable       int64
	)
	err := scan(
		&channel.ID, &channel.DisplayChannelID, &channel.NetworkID, &channel.ServiceID, &transportStreamID,
		&channel.RemoconID, &channel.ChannelNumber, &channel.Type, &channel.Name, &jikkyoForce,
		&isSubchannel, &isRadiochannel, &isWatchable,
	)
	if err != nil {
		return nil, err
	}
	if transportStreamID.Valid {
		value := int(transportStreamID.Int64)
		channel.TransportStreamID = &value
	}
	if jikkyoForce.Valid {
		value := int(jikkyoForce.Int64)
		channel.JikkyoForce = &value
	}
	channel.IsSubchannel = isSubchannel != 0
	channel.IsRadiochannel = isRadiochannel != 0
	channel.IsWatchable = isWatchable != 0
	return &channel, nil
}

// scanProgramRow は programs テーブルの 1 行を database.Program に読み込む。
func scanProgramRow(scan func(dest ...any) error) (*database.Program, error) {
	var (
		program                    database.Program
		startTime                  sql.NullString
		endTime                    sql.NullString
		isFree                     int64
		videoType                  sql.NullString
		videoCodec                 sql.NullString
		videoResolution            sql.NullString
		secondaryAudioType         sql.NullString
		secondaryAudioLanguage     sql.NullString
		secondaryAudioSamplingRate sql.NullString
	)
	err := scan(
		&program.ID, &program.ChannelID, &program.NetworkID, &program.ServiceID, &program.EventID,
		&program.Title, &program.Description, &program.Detail, &startTime, &endTime,
		&program.Duration, &isFree, &program.Genres,
		&videoType, &videoCodec, &videoResolution,
		&program.PrimaryAudioType, &program.PrimaryAudioLanguage, &program.PrimaryAudioSamplingRate,
		&secondaryAudioType, &secondaryAudioLanguage, &secondaryAudioSamplingRate,
	)
	if err != nil {
		return nil, err
	}
	program.IsFree = isFree != 0
	program.VideoType = nullString(videoType)
	program.VideoCodec = nullString(videoCodec)
	program.VideoResolution = nullString(videoResolution)
	program.SecondaryAudioType = nullString(secondaryAudioType)
	program.SecondaryAudioLanguage = nullString(secondaryAudioLanguage)
	program.SecondaryAudioSamplingRate = nullString(secondaryAudioSamplingRate)
	if program.StartTime, err = database.ParseDBTime(startTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse start_time: %w", err)
	}
	if program.EndTime, err = database.ParseDBTime(endTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse end_time: %w", err)
	}
	return &program, nil
}

// nullString は sql.NullString を *string に変換する。
func nullString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// ListAllChannels は channels テーブルのすべてのチャンネルを取得する (Python 版の Channel.all() 相当) 。
func ListAllChannels(ctx context.Context, db *sql.DB) ([]database.Channel, error) {
	rows, err := db.QueryContext(ctx, `SELECT`+storeChannelColumns+`FROM channels`)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	channels := make([]database.Channel, 0)
	for rows.Next() {
		channel, err := scanChannelRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan channel row: %w", err)
		}
		channels = append(channels, *channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate channel rows: %w", err)
	}
	return channels, nil
}

// GetChannelByKey は ONID / TSID / SID が一致するチャンネルを取得する。
// 存在しない場合は ErrNotFound を返す。
func GetChannelByKey(ctx context.Context, db *sql.DB, key ChannelKey) (*database.Channel, error) {
	row := db.QueryRowContext(
		ctx,
		`SELECT`+storeChannelColumns+`FROM channels WHERE network_id = ? AND service_id = ? AND transport_stream_id = ? LIMIT 1`,
		key.NetworkID, key.ServiceID, key.TransportStreamID,
	)
	channel, err := scanChannelRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	return channel, nil
}

// GetChannelByID はチャンネル ID が一致するチャンネルを取得する。
// 存在しない場合は ErrNotFound を返す。
func GetChannelByID(ctx context.Context, db *sql.DB, id string) (*database.Channel, error) {
	row := db.QueryRowContext(ctx, `SELECT`+storeChannelColumns+`FROM channels WHERE id = ? LIMIT 1`, id)
	channel, err := scanChannelRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	return channel, nil
}

// GetProgramByID は番組 ID が一致する番組を取得する。
// 存在しない場合は ErrNotFound を返す。
func GetProgramByID(ctx context.Context, db *sql.DB, id string) (*database.Program, error) {
	row := db.QueryRowContext(ctx, `SELECT`+storeProgramColumns+`FROM programs WHERE id = ? LIMIT 1`, id)
	program, err := scanProgramRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get program: %w", err)
	}
	return program, nil
}

// GetProgramByKey は ONID / SID / EID が一致する番組を取得する。
// 存在しない場合は ErrNotFound を返す。
func GetProgramByKey(ctx context.Context, db *sql.DB, key ProgramKey) (*database.Program, error) {
	row := db.QueryRowContext(
		ctx,
		`SELECT`+storeProgramColumns+`FROM programs WHERE network_id = ? AND service_id = ? AND event_id = ? LIMIT 1`,
		key.NetworkID, key.ServiceID, key.EventID,
	)
	program, err := scanProgramRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get program: %w", err)
	}
	return program, nil
}

// GetProgramsByKeys は複数の (ONID, SID, EID) に該当する番組をまとめて取得する。
//
// Python 版と同じく row-value IN を使い、予約件数が多くても SQLite の式木深さ上限に達しないようにする。
func GetProgramsByKeys(ctx context.Context, db *sql.DB, keys []ProgramKey) (map[ProgramKey]database.Program, error) {
	result := map[ProgramKey]database.Program{}
	if len(keys) == 0 {
		return result, nil
	}

	placeholders := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*3)
	for _, key := range keys {
		placeholders = append(placeholders, "(?, ?, ?)")
		args = append(args, key.NetworkID, key.ServiceID, key.EventID)
	}

	query := `SELECT` + storeProgramColumns + `FROM programs WHERE (network_id, service_id, event_id) IN (` +
		strings.Join(placeholders, ", ") + `)`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list programs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		program, err := scanProgramRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan program row: %w", err)
		}
		result[ProgramKey{NetworkID: program.NetworkID, ServiceID: program.ServiceID, EventID: program.EventID}] = *program
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate program rows: %w", err)
	}
	return result, nil
}
