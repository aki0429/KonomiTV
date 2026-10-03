package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ProgramDateRange は番組データの日付範囲。
type ProgramDateRange struct {
	Earliest *time.Time
	Latest   *time.Time
}

// GetProgramDateRange は番組データの最も早い開始日時と最も遅い終了日時を取得する。
// 番組がない場合は nil になる。
func GetProgramDateRange(ctx context.Context, db *sql.DB) (*ProgramDateRange, error) {
	var earliest, latest sql.NullString
	if err := db.QueryRowContext(
		ctx, `SELECT MIN(start_time) AS earliest, MAX(end_time) AS latest FROM programs`,
	).Scan(&earliest, &latest); err != nil {
		return nil, fmt.Errorf("failed to get program date range: %w", err)
	}
	result := &ProgramDateRange{}
	if earliest.Valid && earliest.String != "" {
		parsed, err := ParseDBTime(earliest.String)
		if err != nil {
			return nil, fmt.Errorf("failed to parse earliest: %w", err)
		}
		result.Earliest = &parsed
	}
	if latest.Valid && latest.String != "" {
		parsed, err := ParseDBTime(latest.String)
		if err != nil {
			return nil, fmt.Errorf("failed to parse latest: %w", err)
		}
		result.Latest = &parsed
	}
	return result, nil
}

// ListWatchableChannelsForTimeTable は番組表向けに視聴可能なチャンネルを取得する。
// channelType が nil でない場合はチャンネル種別で、pinnedChannelIDs が nil でない場合は ID で絞り込む
// (pinnedChannelIDs が優先される) 。
func ListWatchableChannelsForTimeTable(
	ctx context.Context,
	db *sql.DB,
	channelType *string,
	pinnedChannelIDs []string,
) ([]Channel, error) {
	var (
		query string
		args  []any
	)
	if pinnedChannelIDs != nil {
		if len(pinnedChannelIDs) == 0 {
			return []Channel{}, nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(pinnedChannelIDs)), ",")
		query = `SELECT` + channelColumns + `FROM channels WHERE id IN (` + placeholders + `) AND is_watchable = 1`
		for _, channelID := range pinnedChannelIDs {
			args = append(args, channelID)
		}
	} else if channelType != nil {
		query = `SELECT` + channelColumns + `FROM channels WHERE type = ? AND is_watchable = 1 ORDER BY channel_number, remocon_id`
		args = append(args, *channelType)
	} else {
		query = `SELECT` + channelColumns + `FROM channels WHERE is_watchable = 1 ORDER BY channel_number, remocon_id`
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels for timetable: %w", err)
	}
	defer func() { _ = rows.Close() }()

	channels := make([]Channel, 0)
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

// ListProgramsForTimeTable は指定されたチャンネル・期間の番組を取得する。
// Python 版 TimeTableAPI と同じく、期間内に開始する番組・期間内に終了する番組・期間をまたぐ番組を取得する。
func ListProgramsForTimeTable(
	ctx context.Context,
	db *sql.DB,
	channelIDs []string,
	startTime string,
	endTime string,
) ([]Program, error) {
	if len(channelIDs) == 0 {
		return []Program{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(channelIDs)), ",")
	query := `SELECT` + programColumns + `FROM programs
		WHERE
			channel_id IN (` + placeholders + `)
			AND (
				(start_time >= ? AND start_time < ?)
				OR (end_time > ? AND end_time <= ?)
				OR (start_time < ? AND end_time > ?)
			)
		ORDER BY channel_id, start_time`

	args := make([]any, 0, len(channelIDs)+6)
	for _, channelID := range channelIDs {
		args = append(args, channelID)
	}
	args = append(args, startTime, endTime, startTime, endTime, startTime, endTime)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list programs for timetable: %w", err)
	}
	defer func() { _ = rows.Close() }()

	programs := make([]Program, 0)
	for rows.Next() {
		program, err := scanProgram(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan program row: %w", err)
		}
		programs = append(programs, *program)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate program rows: %w", err)
	}
	return programs, nil
}

// SubchannelDuration はサブチャンネルの1日あたりの放送時間の集計結果。
type SubchannelDuration struct {
	ChannelID         string
	Type              string
	NetworkID         int
	ServiceID         int
	TransportStreamID *int
	BroadcastDate     string
	TotalDuration     float64
}

// GetSubchannelDurations はサブチャンネルの放送時間をチャンネル ID ごとに集計する。
// 放送日は 04:00 を境にした日付 (DATE(start_time, '-4 hours')) で判定する。
func GetSubchannelDurations(ctx context.Context, db *sql.DB) ([]SubchannelDuration, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			c.id,
			c.type,
			c.network_id,
			p.service_id,
			c.transport_stream_id,
			DATE(p.start_time, '-4 hours') AS broadcast_date,
			SUM(p.duration) AS total_duration
		FROM programs p
		INNER JOIN channels c ON p.channel_id = c.id
		WHERE c.is_subchannel = 1
		GROUP BY c.id, c.type, c.network_id, p.service_id, c.transport_stream_id, broadcast_date
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get subchannel durations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	durations := make([]SubchannelDuration, 0)
	for rows.Next() {
		var (
			duration          SubchannelDuration
			transportStreamID sql.NullInt64
			broadcastDate     sql.NullString
			totalDuration     sql.NullFloat64
		)
		if err := rows.Scan(
			&duration.ChannelID, &duration.Type, &duration.NetworkID, &duration.ServiceID,
			&transportStreamID, &broadcastDate, &totalDuration,
		); err != nil {
			return nil, fmt.Errorf("failed to scan subchannel duration: %w", err)
		}
		if transportStreamID.Valid {
			value := int(transportStreamID.Int64)
			duration.TransportStreamID = &value
		}
		duration.BroadcastDate = broadcastDate.String
		duration.TotalDuration = totalDuration.Float64
		durations = append(durations, duration)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate subchannel durations: %w", err)
	}
	return durations, nil
}
