// Package epgupdate はバックエンド (EDCB) からチャンネル情報・番組情報を取得し、DB を更新する。
//
// 移植元: server/app/models/Channel.py (update / updateFromEDCB /
// recalculateRecordingOnlyChannelBranchNumbers) と server/app/models/Program.py (updateFromEDCB)
package epgupdate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo"
)

// EDCBSource は更新処理が使う EDCB の CtrlCmd 呼び出し。
type EDCBSource interface {
	FileCopy(name string) ([]byte, bool)
	EnumService() ([]reservations.ServiceInfo, bool)
	EnumPgInfoEx(serviceTimeList []int64) ([]reservations.ServiceEventInfo, bool)
}

// alreadyClosedBSServiceIDs はすでに閉局済みの BS チャンネルの service_id 。
// 左から「NHK BSプレミアム」「FOXスポーツ&エンターテインメント」「BSスカパー」「BSJapanext」「Dlife」
var alreadyClosedBSServiceIDs = map[int]bool{103: true, 104: true, 238: true, 241: true, 258: true, 263: true}

// channelRow は channels テーブルの 1 行。
type channelRow struct {
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
	persisted         bool
}

// ErrChSet5Unavailable は EDCB から ChSet5.txt を取得できなかったことを表す。
var ErrChSet5Unavailable = errors.New("failed to get channels from EDCB")

const channelColumns = `id, display_channel_id, network_id, service_id, transport_stream_id, remocon_id,
	channel_number, type, name, jikkyo_force, is_subchannel, is_radiochannel, is_watchable`

func scanChannels(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]*channelRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+channelColumns+` FROM channels WHERE `+where+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []*channelRow
	for rows.Next() {
		var row channelRow
		var tsid, jikkyo sql.NullInt64
		if err := rows.Scan(&row.ID, &row.DisplayChannelID, &row.NetworkID, &row.ServiceID, &tsid, &row.RemoconID,
			&row.ChannelNumber, &row.Type, &row.Name, &jikkyo, &row.IsSubchannel, &row.IsRadiochannel, &row.IsWatchable); err != nil {
			return nil, err
		}
		if tsid.Valid {
			value := int(tsid.Int64)
			row.TransportStreamID = &value
		}
		if jikkyo.Valid {
			value := int(jikkyo.Int64)
			row.JikkyoForce = &value
		}
		row.persisted = true
		result = append(result, &row)
	}
	return result, rows.Err()
}

// saveChannel は Tortoise ORM の save() と同じく、既存行は全列 UPDATE、新規行は INSERT する。
func saveChannel(ctx context.Context, tx *sql.Tx, row *channelRow) error {
	args := []any{row.DisplayChannelID, row.NetworkID, row.ServiceID, row.TransportStreamID, row.RemoconID,
		row.ChannelNumber, row.Type, row.Name, row.JikkyoForce, row.IsSubchannel, row.IsRadiochannel, row.IsWatchable}
	if row.persisted {
		_, err := tx.ExecContext(ctx, `UPDATE channels SET display_channel_id=?, network_id=?, service_id=?, transport_stream_id=?,
			remocon_id=?, channel_number=?, type=?, name=?, jikkyo_force=?, is_subchannel=?, is_radiochannel=?, is_watchable=?
			WHERE id=?`, append(args, row.ID)...)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO channels (`+channelColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		append([]any{row.ID}, args...)...)
	if err == nil {
		row.persisted = true
	}
	return err
}

// isUniqueViolation は SQLite の制約違反 (Tortoise の IntegrityError 相当) かを判定する。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

// channelCounters は TSInformation.calculateChannelNumber に渡す同一 NID / 同一リモコン番号のカウンタ。
type channelCounters struct {
	sameNetworkID map[int]int
	sameRemoconID map[int]int
}

func newChannelCounters() *channelCounters {
	return &channelCounters{sameNetworkID: map[int]int{}, sameRemoconID: map[int]int{}}
}

// channelNumber は calculateChannelNumber のカウンタ付き経路 (チャンネル情報更新時) を移植したもの。
func (c *channelCounters) channelNumber(channelType string, networkID, serviceID, remoconID int) string {
	channelNumber := fmt.Sprintf("%03d", serviceID)
	switch channelType {
	case "GR":
		if _, exists := c.sameRemoconID[remoconID]; !exists {
			// 011(-0), 011-1, 011-2 のように枝番をつけるため -1 を基点とする
			c.sameRemoconID[remoconID] = -1
		}
		// 同じネットワーク内にある最初のサービスのときだけ、同じリモコン番号のカウントを追加する
		if c.sameNetworkID[networkID] == 1 {
			c.sameRemoconID[remoconID]++
		}
		channelNumber = fmt.Sprintf("%02d", remoconID) + strconv.Itoa(c.sameNetworkID[networkID])
		if c.sameRemoconID[remoconID] > 0 {
			channelNumber += "-" + strconv.Itoa(c.sameRemoconID[remoconID])
		}
	case "SKY":
		channelNumber = fmt.Sprintf("%03d", serviceID%1024)
	}
	return channelNumber
}

// sortServicesByPreferredRegion は優先地域を先頭にし、NID-SID の数値順に並べる (安定ソート) 。
func sortServicesByPreferredRegion(services []reservations.ChSet5Item, preferredRegion *string) {
	var preferredIDs []int
	if preferredRegion != nil {
		preferredIDs = tsinfo.TerrestrialRegionToRegionIDs(*preferredRegion)
	}
	key := func(service reservations.ChSet5Item) (int, int) {
		base := service.Onid*100000 + service.Sid
		if len(preferredIDs) > 0 {
			regionID := tsinfo.GetRegionIDFromNetworkID(service.Onid)
			for _, id := range preferredIDs {
				if id == regionID {
					return 0, base
				}
			}
		}
		return 1, base
	}
	sort.SliceStable(services, func(i, j int) bool {
		gi, bi := key(services[i])
		gj, bj := key(services[j])
		if gi != gj {
			return gi < gj
		}
		return bi < bj
	})
}

// UpdateChannelsFromEDCB は EDCB の ChSet5.txt と EPG 由来のサービス情報からチャンネル情報を更新する。
// 移植元: Channel.updateFromEDCB()
func UpdateChannelsFromEDCB(ctx context.Context, db *sql.DB, edcb EDCBSource, preferredRegion *string, logger *slog.Logger) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	watchable, err := scanChannels(ctx, tx, "is_watchable = 1")
	if err != nil {
		return err
	}
	// 更新対象を取り除いていき、残った古いチャンネル情報を最後にまとめて削除する
	duplicates := map[string]*channelRow{}
	backupRemoconIDs := map[string]int{}
	for _, row := range watchable {
		duplicates[row.ID] = row
		backupRemoconIDs[row.ID] = row.RemoconID
	}

	chset5, ok := edcb.FileCopy("ChSet5.txt")
	if !ok || chset5 == nil {
		logger.Error("Failed to get channels from EDCB.")
		return ErrChSet5Unavailable
	}
	services := reservations.ParseChSet5(reservations.ConvertEDCBBytesToString(chset5))
	sortServicesByPreferredRegion(services, preferredRegion)

	// 優先エリア設定の変更で枝番が変わる場合に UNIQUE 制約へ衝突しないよう、地デジの表示用 ID を一時値にする
	for _, row := range watchable {
		if row.Type == "GR" {
			temp := "_temp_" + row.ID
			if row.DisplayChannelID != temp {
				row.DisplayChannelID = temp
				if err = saveChannel(ctx, tx, row); err != nil {
					return err
				}
			}
		}
	}

	// EPG 由来のサービス情報 (番組情報が無いサービスは含まれない) 。取得できなくても続行する
	epgServices, _ := edcb.EnumService()

	counters := newChannelCounters()
	for _, service := range services {
		switch service.ServiceType {
		case 0x01, 0x02, 0xa1, 0xa2, 0xad:
		default:
			continue
		}
		channelType := reservations.GetNetworkType(service.Onid)
		if channelType == "OTHER" {
			continue
		}
		channelID := fmt.Sprintf("NID%d-SID%03d", service.Onid, service.Sid)

		row, exists := duplicates[channelID]
		if exists {
			delete(duplicates, channelID)
		} else {
			unwatchable, err := scanChannels(ctx, tx, "id = ? AND is_watchable = 0", channelID)
			if err != nil {
				return err
			}
			if len(unwatchable) > 0 {
				row = unwatchable[0]
				// 閉局済み BS の録画専用チャンネルは以降の処理をすべてスキップする
				if row.Type == "BS" && alreadyClosedBSServiceIDs[row.ServiceID] {
					continue
				}
				row.IsWatchable = true
				logger.Warn(fmt.Sprintf("Channel: %s (%s) is already registered but is_watchable = False.", row.Name, row.ID))
			} else {
				row = &channelRow{}
			}
		}

		row.ID = channelID
		row.ServiceID = service.Sid
		row.NetworkID = service.Onid
		tsid := service.Tsid
		row.TransportStreamID = &tsid
		row.RemoconID = service.RemoconID
		row.Type = channelType
		row.Name = reservations.FormatString(service.ServiceName)
		row.JikkyoForce = nil
		row.IsWatchable = true
		if row.Type == "BS" && alreadyClosedBSServiceIDs[row.ServiceID] {
			row.IsWatchable = false
		}
		if strings.HasPrefix(row.Name, "試験チャンネル") {
			row.IsWatchable = false
		}
		row.IsRadiochannel = service.ServiceType == 0x02

		counters.sameNetworkID[row.NetworkID]++

		if row.Type == "GR" {
			// EPG 由来のサービス情報からリモコン番号を取得する (EDCB-240213 未満の ChSet5.txt には無い)
			var epgService *reservations.ServiceInfo
			for index := range epgServices {
				if epgServices[index].Onid == row.NetworkID && epgServices[index].Sid == row.ServiceID {
					epgService = &epgServices[index]
					break
				}
			}
			if epgService != nil {
				row.RemoconID = epgService.RemoteControlKeyID
			} else {
				if backup, exists := backupRemoconIDs[row.ID]; row.RemoconID <= 0 && exists {
					row.RemoconID = backup
				}
				// それでも不明なら、同じネットワーク ID を持つ別サービスのリモコン番号を使う (地上波の臨時サービス対策)
				if row.RemoconID <= 0 {
					for _, other := range epgServices {
						if other.Onid == row.NetworkID && other.Sid != row.ServiceID {
							row.RemoconID = other.RemoteControlKeyID
							break
						}
					}
				}
			}
		} else {
			row.RemoconID = reservations.CalculateRemoconID(row.Type, row.ServiceID)
		}

		row.ChannelNumber = counters.channelNumber(row.Type, row.NetworkID, row.ServiceID, row.RemoconID)
		row.DisplayChannelID = strings.ToLower(row.Type) + row.ChannelNumber
		row.IsSubchannel = reservations.CalculateIsSubchannel(row.Type, row.ServiceID)

		if saveErr := saveChannel(ctx, tx, row); saveErr != nil {
			if !isUniqueViolation(saveErr) {
				return saveErr
			}
			logger.Warn(fmt.Sprintf("Channel: %s (%s) is already registered.", row.Name, row.ID))
		}
	}

	// 不要なチャンネル情報を削除する (Python の dict と同じく取得順で処理する)
	for _, row := range watchable {
		if _, remaining := duplicates[row.ID]; !remaining {
			continue
		}
		var referenced bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recorded_programs WHERE channel_id = ?)`, row.ID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			// 削除すると CASCADE 制約で録画番組情報も消えるため、視聴不可として残す
			row.IsWatchable = false
			if err = saveChannel(ctx, tx, row); err != nil {
				return err
			}
			logger.Info(fmt.Sprintf("Channel: %s (%s) is referenced by RecordedProgram, set is_watchable to False.", row.Name, row.ID))
		} else {
			if _, err = tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, row.ID); err != nil {
				return err
			}
			logger.Info("Delete Channel: " + row.ID)
		}
	}

	if err = recalculateRecordingOnlyChannelBranchNumbers(ctx, tx, counters, logger); err != nil {
		return err
	}
	return tx.Commit()
}

// recalculateRecordingOnlyChannelBranchNumbers は地デジの録画専用チャンネルの枝番を再計算する。
// 移植元: Channel.recalculateRecordingOnlyChannelBranchNumbers()
func recalculateRecordingOnlyChannelBranchNumbers(ctx context.Context, tx *sql.Tx, counters *channelCounters, logger *slog.Logger) error {
	channels, err := scanChannels(ctx, tx, "is_watchable = 0 AND type = 'GR'")
	if err != nil {
		return err
	}
	for _, row := range channels {
		temp := "_temp_" + row.ID
		if row.DisplayChannelID != temp {
			row.DisplayChannelID = temp
			if err := saveChannel(ctx, tx, row); err != nil {
				return err
			}
		}
	}
	sorted := append([]*channelRow(nil), channels...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].NetworkID*100000+sorted[i].ServiceID < sorted[j].NetworkID*100000+sorted[j].ServiceID
	})
	for _, row := range sorted {
		counters.sameNetworkID[row.NetworkID]++
		number := counters.channelNumber(row.Type, row.NetworkID, row.ServiceID, row.RemoconID)
		row.ChannelNumber = number
		row.DisplayChannelID = strings.ToLower(row.Type) + number
		if err := saveChannel(ctx, tx, row); err != nil {
			if !isUniqueViolation(err) {
				return err
			}
			logger.Warn(fmt.Sprintf("Channel: %s (%s) display_channel_id conflict, skipped.", row.Name, row.ID))
			continue
		}
		logger.Info(fmt.Sprintf("Updated recording-only channel branch number: %s -> %s", row.ID, row.DisplayChannelID))
	}
	return nil
}
