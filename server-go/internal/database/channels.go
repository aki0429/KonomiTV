package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Channel は channels テーブルのレコード (server/app/models/Channel.py 互換) 。
type Channel struct {
	ID                string
	DisplayChannelID  string
	NetworkID         int
	ServiceID         int
	TransportStreamID *int
	RemoconID         int
	ChannelNumber     string
	Type              string
	Name              string
	JikkyoForce       *int
	IsSubchannel      bool
	IsRadiochannel    bool
	IsWatchable       bool
}

// Program は programs テーブルのレコード (server/app/models/Program.py 互換) 。
// Detail / Genres は JSON 文字列のまま保持する (API 側でデコードする) 。
type Program struct {
	ID                         string
	ChannelID                  string
	NetworkID                  int
	ServiceID                  int
	EventID                    int
	Title                      string
	Description                string
	Detail                     string
	StartTime                  time.Time
	EndTime                    time.Time
	Duration                   float64
	IsFree                     bool
	Genres                     string
	VideoType                  *string
	VideoCodec                 *string
	VideoResolution            *string
	PrimaryAudioType           string
	PrimaryAudioLanguage       string
	PrimaryAudioSamplingRate   string
	SecondaryAudioType         *string
	SecondaryAudioLanguage     *string
	SecondaryAudioSamplingRate *string
}

// ErrChannelNotFound は指定されたチャンネルが存在しない場合のエラー。
var ErrChannelNotFound = errors.New("channel not found")

// channelColumns は channels テーブルの SELECT で使うカラム一覧。
const channelColumns = `
	id, display_channel_id, network_id, service_id, transport_stream_id,
	remocon_id, channel_number, type, name, jikkyo_force,
	is_subchannel, is_radiochannel, is_watchable
`

// programColumns は programs テーブルの SELECT で使うカラム一覧。
const programColumns = `
	id, channel_id, network_id, service_id, event_id, title, description,
	detail, start_time, end_time, duration, is_free, genres,
	video_type, video_codec, video_resolution,
	primary_audio_type, primary_audio_language, primary_audio_sampling_rate,
	secondary_audio_type, secondary_audio_language, secondary_audio_sampling_rate
`

// scanChannel は channels テーブルの 1 行を Channel に読み込む。
func scanChannel(scan func(dest ...any) error) (*Channel, error) {
	var (
		channel           Channel
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

// scanProgram は programs テーブルの 1 行を Program に読み込む。
func scanProgram(scan func(dest ...any) error) (*Program, error) {
	var (
		program                    Program
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
	program.VideoType = nullableString(videoType)
	program.VideoCodec = nullableString(videoCodec)
	program.VideoResolution = nullableString(videoResolution)
	program.SecondaryAudioType = nullableString(secondaryAudioType)
	program.SecondaryAudioLanguage = nullableString(secondaryAudioLanguage)
	program.SecondaryAudioSamplingRate = nullableString(secondaryAudioSamplingRate)
	if program.StartTime, err = ParseDBTime(startTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse start_time: %w", err)
	}
	if program.EndTime, err = ParseDBTime(endTime.String); err != nil {
		return nil, fmt.Errorf("failed to parse end_time: %w", err)
	}
	return &program, nil
}

// nullableString は sql.NullString を *string に変換する。
func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// GetChannelByIDOrDisplayChannelID はチャンネル ID (id または display_channel_id) からチャンネルを取得する。
// Python 版の GetChannel() と同じく、'NID' と 'SID' を含む場合は id として、それ以外は display_channel_id として扱う。
// 存在しない場合は ErrChannelNotFound を返す。
func GetChannelByIDOrDisplayChannelID(ctx context.Context, db *sql.DB, channelID string) (*Channel, error) {
	var row *sql.Row
	if strings.Contains(channelID, "NID") && strings.Contains(channelID, "SID") {
		row = db.QueryRowContext(ctx, `SELECT`+channelColumns+`FROM channels WHERE id = ?`, channelID)
	} else {
		row = db.QueryRowContext(ctx, `SELECT`+channelColumns+`FROM channels WHERE display_channel_id = ?`, channelID)
	}
	channel, err := scanChannel(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get channel (%q): %w", channelID, err)
	}
	return channel, nil
}

// GetChannelByNetworkIDAndServiceID はネットワーク ID とサービス ID からチャンネルを取得する。
// BS サブチャンネルのロゴ取得で使う。存在しない場合は ErrChannelNotFound を返す。
func GetChannelByNetworkIDAndServiceID(ctx context.Context, db *sql.DB, networkID int, serviceID int) (*Channel, error) {
	row := db.QueryRowContext(
		ctx,
		`SELECT`+channelColumns+`FROM channels WHERE network_id = ? AND service_id = ?`,
		networkID, serviceID,
	)
	channel, err := scanChannel(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get channel (network_id=%d, service_id=%d): %w", networkID, serviceID, err)
	}
	return channel, nil
}

// GetMainChannelByNetworkID は同じネットワーク ID のチャンネルのうち、サービス ID が一番若いチャンネルを取得する。
// 地デジサブチャンネルのロゴ取得で使う。存在しない場合は ErrChannelNotFound を返す。
func GetMainChannelByNetworkID(ctx context.Context, db *sql.DB, networkID int) (*Channel, error) {
	row := db.QueryRowContext(
		ctx,
		`SELECT`+channelColumns+`FROM channels WHERE network_id = ? ORDER BY service_id ASC LIMIT 1`,
		networkID,
	)
	channel, err := scanChannel(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get main channel (network_id=%d): %w", networkID, err)
	}
	return channel, nil
}

// GetCurrentAndNextProgram は指定されたチャンネルの現在と次の番組情報を取得する。
// Python 版 Channel.getCurrentAndNextProgram() と同じクエリ。
// 番組が見つからない場合は nil を返す。
func GetCurrentAndNextProgram(ctx context.Context, db *sql.DB, channelID string) (present *Program, following *Program, err error) {
	// Tortoise ORM は datetime を "YYYY-MM-DD HH:MM:SS.ffffff+09:00" 形式の文字列として
	// バインドするため、SQLite 上では文字列比較になる。同じ形式で比較する。
	now := NowForDB()

	row := db.QueryRowContext(
		ctx,
		`SELECT`+programColumns+`FROM programs
		 WHERE channel_id = ? AND start_time <= ? AND end_time >= ?
		 ORDER BY start_time DESC LIMIT 1`,
		channelID, now, now,
	)
	if present, err = scanProgram(row.Scan); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("failed to get present program (%q): %w", channelID, err)
		}
		present = nil
	}

	row = db.QueryRowContext(
		ctx,
		`SELECT`+programColumns+`FROM programs
		 WHERE channel_id = ? AND start_time >= ?
		 ORDER BY start_time ASC LIMIT 1`,
		channelID, now,
	)
	if following, err = scanProgram(row.Scan); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("failed to get following program (%q): %w", channelID, err)
		}
		following = nil
	}

	return present, following, nil
}

// ListWatchableChannels は視聴可能なチャンネルをリモコン番号 → チャンネル番号の順で返す。
func ListWatchableChannels(ctx context.Context, db *sql.DB) ([]*Channel, error) {
	// Tortoise ORM の order_by('remocon_id', 'channel_number') と同じ並び順にする
	rows, err := db.QueryContext(
		ctx,
		`SELECT`+channelColumns+`FROM channels
		 WHERE is_watchable = 1
		 ORDER BY remocon_id ASC, channel_number ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list watchable channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	channels := []*Channel{}
	for rows.Next() {
		channel, err := scanChannel(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan channel row: %w", err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate channel rows: %w", err)
	}
	return channels, nil
}

// ListChannelsByIDs は指定された ID のチャンネルを取得する (存在しない ID は無視する) 。
func ListChannelsByIDs(ctx context.Context, db *sql.DB, channelIDs []string) (map[string]Channel, error) {
	result := map[string]Channel{}
	if len(channelIDs) == 0 {
		return result, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(channelIDs)), ",")
	args := make([]any, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		args = append(args, channelID)
	}
	rows, err := db.QueryContext(ctx, `SELECT`+channelColumns+`FROM channels WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		channel, err := scanChannel(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan channel row: %w", err)
		}
		result[channel.ID] = *channel
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate channel rows: %w", err)
	}
	return result, nil
}

// SearchChannelsByName はチャンネル名の部分一致でチャンネルを検索する。
// 移植元: Python 版 CapturesRouter の Channel.filter(name__icontains=search)
func SearchChannelsByName(ctx context.Context, db *sql.DB, search string) ([]Channel, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT`+channelColumns+`FROM channels WHERE LOWER(name) LIKE ?`,
		"%"+strings.ToLower(search)+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to search channels by name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := []Channel{}
	for rows.Next() {
		channel, err := scanChannel(rows.Scan)
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
