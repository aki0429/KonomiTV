package epgupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// ErrProgramsUnavailable は EDCB から番組情報を取得できなかったことを表す。
var ErrProgramsUnavailable = errors.New("failed to get programs from EDCB")

// allProgramsServiceTimeList は開始時間未定を除く全サービス・全期間の番組を指定する
// (前2要素は全番組、残り2要素は全期間) 。
var allProgramsServiceTimeList = []int64{0xffffffffffff, 0xffffffffffff, 1, 0x7fffffffffffffff}

// existingProgram は更新要否の判定に使う既存番組の値。
type existingProgram struct {
	title       string
	description string
	detailLen   int
	startTime   time.Time
	endTime     time.Time
}

// programRecord は programs テーブルへ保存する 1 行。
type programRecord struct {
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

func loadExistingPrograms(ctx context.Context, tx *sql.Tx) (map[string]existingProgram, []string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, title, description, detail, start_time, end_time FROM programs ORDER BY rowid`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]existingProgram{}
	var order []string
	for rows.Next() {
		var id, title, description, detail, start, end string
		if err := rows.Scan(&id, &title, &description, &detail, &start, &end); err != nil {
			return nil, nil, err
		}
		startTime, err := database.ParseDBTime(start)
		if err != nil {
			return nil, nil, fmt.Errorf("program %s start_time: %w", id, err)
		}
		endTime, err := database.ParseDBTime(end)
		if err != nil {
			return nil, nil, fmt.Errorf("program %s end_time: %w", id, err)
		}
		var fields map[string]json.RawMessage
		detailLen := 0
		if json.Unmarshal([]byte(detail), &fields) == nil {
			detailLen = len(fields)
		}
		result[id] = existingProgram{title: title, description: description, detailLen: detailLen, startTime: startTime, endTime: endTime}
		order = append(order, id)
	}
	return result, order, rows.Err()
}

// decodeProgram は Program.updateFromEDCB() の EventInfo → Program 変換を移植したもの。
func decodeProgram(event reservations.EventInfo) (record programRecord, detail reservations.ProgramDetail) {
	if event.ShortInfo != nil {
		record.Title = strings.TrimSpace(reservations.FormatString(event.ShortInfo.EventName))
		record.Description = strings.TrimSpace(reservations.FormatString(event.ShortInfo.TextChar))
	}
	if event.ExtInfo != nil {
		detail = reservations.ParseProgramExtendedText(event.ExtInfo.TextChar).Normalized()
		// 番組概要が空の場合、番組詳細の最初の本文を概要として使う
		for _, entry := range detail.Entries() {
			if strings.TrimSpace(record.Description) == "" {
				record.Description = entry.Body
			}
		}
	}
	entries := make([]metadata.DetailEntry, 0, detail.Len())
	for _, entry := range detail.Entries() {
		entries = append(entries, metadata.DetailEntry{Key: entry.Heading, Value: entry.Body})
	}
	record.Detail = metadata.DetailJSON(entries)

	start := time.Date(1970, 1, 1, 9, 0, 0, 0, constants.JST)
	if event.StartTime != nil {
		start = *event.StartTime
	}
	duration := 300
	if event.DurationSec != nil {
		duration = *event.DurationSec
	}
	record.StartTime = start
	record.EndTime = start.Add(time.Duration(duration) * time.Second)
	record.IsFree = event.FreeCaFlag == 0

	genres := []metadata.Genre{}
	if event.ContentInfo != nil {
		for _, content := range event.ContentInfo.NibbleList {
			major, ok := reservations.FindGenreMajor(content.ContentNibble >> 8)
			if !ok {
				continue
			}
			genre := metadata.Genre{Major: strings.ReplaceAll(major.Major, "／", "・"), Middle: "未定義"}
			for _, middle := range major.Middle {
				if middle.Key == content.ContentNibble&0xf {
					genre.Middle = strings.ReplaceAll(middle.Name, "／", "・")
					break
				}
			}
			if genre.Major == "拡張" {
				if genre.Middle != "BS/地上デジタル放送用番組付属情報" {
					continue
				}
				genre.Middle = "未定義"
				user := content.UserNibble>>8<<4 | content.UserNibble&0xf
				for _, entry := range reservations.UserTypes() {
					if entry.Key == user {
						genre.Middle = entry.Name
						break
					}
				}
			}
			genres = append(genres, genre)
		}
	}
	record.Genres = metadata.GenresJSON(genres)

	if video := event.ComponentInfo; video != nil {
		if types, ok := reservations.ComponentTypes[video.StreamContent]; ok {
			if value, ok := types[video.ComponentType]; ok {
				record.VideoType = &value
			}
		}
		if value, ok := reservations.VideoCodecs[video.StreamContent]; ok {
			record.VideoCodec = &value
		}
		if value, ok := reservations.VideoResolutions[video.ComponentType]; ok {
			record.VideoResolution = &value
		}
	}

	if audio := event.AudioInfo; audio != nil && len(audio.ComponentList) > 0 {
		lookup := func(item reservations.AudioComponentInfoData, base string) (string, string, string) {
			typeName, ok := reservations.ComponentTypes[0x02][item.ComponentType]
			if !ok {
				typeName = "Unknown"
			}
			rate, ok := reservations.SamplingRates[item.SamplingRate]
			if !ok {
				rate = "Unknown"
			}
			language := base
			if typeName == "1/0+1/0モード(デュアルモノ)" {
				if item.EsMultiLingualFlag != 0 {
					language += "+英語"
				} else {
					language += "+副音声"
				}
			}
			return typeName, language, rate
		}
		record.PrimaryAudioType, record.PrimaryAudioLanguage, record.PrimaryAudioSamplingRate = lookup(audio.ComponentList[0], "日本語")
		if len(audio.ComponentList) > 1 {
			typeName, language, rate := lookup(audio.ComponentList[1], "副音声")
			record.SecondaryAudioType, record.SecondaryAudioLanguage, record.SecondaryAudioSamplingRate = &typeName, &language, &rate
		}
	}
	return record, detail
}

// UpdateProgramsFromEDCB は EDCB の EPG から番組情報を取得し、programs テーブルを更新する。
// 移植元: Program.updateFromEDCB()
func UpdateProgramsFromEDCB(ctx context.Context, db *sql.DB, edcb EDCBSource, now time.Time, logger *slog.Logger) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	services, ok := edcb.EnumPgInfoEx(allProgramsServiceTimeList)
	if !ok {
		logger.Error("Failed to get programs from EDCB.")
		return ErrProgramsUnavailable
	}
	duplicates, order, err := loadExistingPrograms(ctx, tx)
	if err != nil {
		return err
	}

	for _, service := range services {
		nid, sid, tsid := service.ServiceInfo.Onid, service.ServiceInfo.Sid, service.ServiceInfo.Tsid
		// TSID まで一致するチャンネルだけを対象にする (BS トランスポンダ再編時の重複追加を避ける)
		var channelID string
		var channelNID, channelSID int
		lookupErr := tx.QueryRowContext(ctx, `SELECT id, network_id, service_id FROM channels
			WHERE network_id = ? AND service_id = ? AND transport_stream_id = ? ORDER BY rowid LIMIT 1`, nid, sid, tsid).
			Scan(&channelID, &channelNID, &channelSID)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			continue
		}
		if lookupErr != nil {
			return lookupErr
		}

		for _, event := range service.EventList {
			// メインの番組でない (イベント共有の副側) なら弾く
			if group := event.EventGroupInfo; group != nil && len(group.EventDataList) == 1 {
				primary := group.EventDataList[0]
				if primary.Onid != nid || primary.Tsid != tsid || primary.Sid != sid || primary.Eid != event.Eid {
					continue
				}
			}
			record, detail := decodeProgram(event)
			// 番組終了時刻が現在時刻より 12 時間以上前の番組を弾く
			if now.Sub(record.EndTime) > 12*time.Hour {
				continue
			}
			programID := fmt.Sprintf("NID%d-SID%03d-EID%d", nid, sid, event.Eid)
			existing, isDuplicate := duplicates[programID]
			if isDuplicate && existing.title == record.Title && existing.description == record.Description &&
				existing.detailLen == detail.Len() && existing.startTime.Equal(record.StartTime) && existing.endTime.Equal(record.EndTime) {
				delete(duplicates, programID)
				continue
			}
			if isDuplicate {
				delete(duplicates, programID)
			}
			record.ID = programID
			record.ChannelID = channelID
			record.NetworkID = channelNID
			record.ServiceID = channelSID
			record.EventID = event.Eid
			if err = saveProgram(ctx, tx, record, isDuplicate); err != nil {
				return err
			}
		}
	}

	// 残っている番組は放送が終わって EPG から削除されたものなので、まとめて削除する
	for _, id := range order {
		if _, remaining := duplicates[id]; !remaining {
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM programs WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// saveProgram は Tortoise ORM の save() と同じく、既存行は全列 UPDATE、新規行は INSERT する。
func saveProgram(ctx context.Context, tx *sql.Tx, record programRecord, exists bool) error {
	duration := record.EndTime.Sub(record.StartTime).Seconds()
	values := []any{record.ChannelID, record.NetworkID, record.ServiceID, record.EventID, record.Title, record.Description,
		record.Detail, database.FormatDBTime(record.StartTime), database.FormatDBTime(record.EndTime), duration, record.IsFree,
		record.Genres, record.VideoType, record.VideoCodec, record.VideoResolution, record.PrimaryAudioType,
		record.PrimaryAudioLanguage, record.PrimaryAudioSamplingRate, record.SecondaryAudioType,
		record.SecondaryAudioLanguage, record.SecondaryAudioSamplingRate}
	if exists {
		_, err := tx.ExecContext(ctx, `UPDATE programs SET channel_id=?, network_id=?, service_id=?, event_id=?, title=?,
			description=?, detail=?, start_time=?, end_time=?, duration=?, is_free=?, genres=?, video_type=?, video_codec=?,
			video_resolution=?, primary_audio_type=?, primary_audio_language=?, primary_audio_sampling_rate=?,
			secondary_audio_type=?, secondary_audio_language=?, secondary_audio_sampling_rate=? WHERE id=?`,
			append(values, record.ID)...)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO programs (id, channel_id, network_id, service_id, event_id, title, description,
		detail, start_time, end_time, duration, is_free, genres, video_type, video_codec, video_resolution,
		primary_audio_type, primary_audio_language, primary_audio_sampling_rate, secondary_audio_type,
		secondary_audio_language, secondary_audio_sampling_rate) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		append([]any{record.ID}, values...)...)
	return err
}
