package epgupdate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo"
)

// MirakurunSource は Mirakurun / mirakc の API 応答 (JSON 本文) を返す。
type MirakurunSource interface {
	Services(ctx context.Context) ([]byte, error)
	Programs(ctx context.Context) ([]byte, error)
}

// HTTPMirakurun は Mirakurun / mirakc の HTTP API に接続する MirakurunSource 。
type HTTPMirakurun struct {
	// BaseURL は config.yaml の mirakurun_url 。
	BaseURL string
	Client  *http.Client
}

// endpoint は GetMirakurunAPIEndpointURL() と同じく末尾のスラッシュを除いてからパスを付ける。
func (m HTTPMirakurun) endpoint(path string) string {
	return strings.TrimRight(m.BaseURL, "/") + path
}

func (m HTTPMirakurun) get(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint(path), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP Error %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// Services は /api/services を取得する (タイムアウト 5 秒) 。
func (m HTTPMirakurun) Services(ctx context.Context) ([]byte, error) {
	return m.get(ctx, "/api/services", 5*time.Second)
}

// Programs は /api/programs を取得する (タイムアウト 10 秒) 。
func (m HTTPMirakurun) Programs(ctx context.Context) ([]byte, error) {
	return m.get(ctx, "/api/programs", 10*time.Second)
}

// mirakurunService は /api/services の 1 サービス。
type mirakurunService struct {
	ServiceID          int    `json:"serviceId"`
	NetworkID          int    `json:"networkId"`
	Name               string `json:"name"`
	Type               int    `json:"type"`
	RemoteControlKeyID *int   `json:"remoteControlKeyId"`
}

// UpdateChannelsFromMirakurun は Mirakurun / mirakc のサービス一覧からチャンネル情報を更新する。
// 移植元: Channel.updateFromMirakurun()
func UpdateChannelsFromMirakurun(ctx context.Context, db *sql.DB, mirakurun MirakurunSource, preferredRegion *string, logger *slog.Logger) (err error) {
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
	duplicates := map[string]*channelRow{}
	for _, row := range watchable {
		duplicates[row.ID] = row
	}

	body, err := mirakurun.Services(ctx)
	if err != nil {
		logger.Error("Failed to get channels from Mirakurun / mirakc.", "error", err)
		return fmt.Errorf("failed to get channels from Mirakurun / mirakc: %w", err)
	}
	var services []mirakurunService
	if err = json.Unmarshal(body, &services); err != nil {
		return err
	}

	// 優先地域を先頭にし、NID-SID の数値順に並べる
	var preferredIDs []int
	if preferredRegion != nil {
		preferredIDs = tsinfo.TerrestrialRegionToRegionIDs(*preferredRegion)
	}
	group := func(service mirakurunService) int {
		if len(preferredIDs) > 0 {
			regionID := tsinfo.GetRegionIDFromNetworkID(service.NetworkID)
			for _, id := range preferredIDs {
				if id == regionID {
					return 0
				}
			}
		}
		return 1
	}
	sort.SliceStable(services, func(i, j int) bool {
		gi, gj := group(services[i]), group(services[j])
		if gi != gj {
			return gi < gj
		}
		return services[i].NetworkID*100000+services[i].ServiceID < services[j].NetworkID*100000+services[j].ServiceID
	})

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

	counters := newChannelCounters()
	for _, service := range services {
		switch service.Type {
		case 0x01, 0x02, 0xa1, 0xa2, 0xad:
		default:
			continue
		}
		channelType := reservations.GetNetworkType(service.NetworkID)
		if channelType == "OTHER" {
			continue
		}
		channelID := fmt.Sprintf("NID%d-SID%03d", service.NetworkID, service.ServiceID)

		row, exists := duplicates[channelID]
		if exists {
			delete(duplicates, channelID)
		} else {
			unwatchable, scanErr := scanChannels(ctx, tx, "id = ? AND is_watchable = 0", channelID)
			if scanErr != nil {
				return scanErr
			}
			if len(unwatchable) > 0 {
				row = unwatchable[0]
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
		row.ServiceID = service.ServiceID
		row.NetworkID = service.NetworkID
		// Mirakurun は TSID を返さないが、地デジは運用上 NID と TSID が同一なので補完する
		// (BS/CS などでは既存レコードの transport_stream_id を触らない)
		if channelType == "GR" {
			tsid := row.NetworkID
			row.TransportStreamID = &tsid
		}
		row.RemoconID = 0
		if service.RemoteControlKeyID != nil {
			row.RemoconID = *service.RemoteControlKeyID
		}
		row.Type = channelType
		row.Name = reservations.FormatString(service.Name)
		row.JikkyoForce = nil
		row.IsWatchable = true
		if row.Type == "BS" && alreadyClosedBSServiceIDs[row.ServiceID] {
			row.IsWatchable = false
		}
		if strings.HasPrefix(row.Name, "試験チャンネル") {
			row.IsWatchable = false
		}
		row.IsRadiochannel = service.Type == 0x02
		counters.sameNetworkID[row.NetworkID]++
		if row.Type != "GR" {
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

	if err = removeStaleChannels(ctx, tx, watchable, duplicates, logger); err != nil {
		return err
	}
	if err = recalculateRecordingOnlyChannelBranchNumbers(ctx, tx, counters, logger); err != nil {
		return err
	}
	return tx.Commit()
}

// removeStaleChannels は更新されなかった視聴可能チャンネルを削除する
// (録画番組から参照されているものは削除せず視聴不可にする) 。
func removeStaleChannels(ctx context.Context, tx *sql.Tx, watchable []*channelRow, duplicates map[string]*channelRow, logger *slog.Logger) error {
	for _, row := range watchable {
		if _, remaining := duplicates[row.ID]; !remaining {
			continue
		}
		var referenced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recorded_programs WHERE channel_id = ?)`, row.ID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			row.IsWatchable = false
			if err := saveChannel(ctx, tx, row); err != nil {
				return err
			}
			logger.Info(fmt.Sprintf("Channel: %s (%s) is referenced by RecordedProgram, set is_watchable to False.", row.Name, row.ID))
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, row.ID); err != nil {
			return err
		}
		logger.Info("Delete Channel: " + row.ID)
	}
	return nil
}

// mirakurunProgram は /api/programs の 1 番組。
type mirakurunProgram struct {
	EventID     int             `json:"eventId"`
	ServiceID   int             `json:"serviceId"`
	NetworkID   int             `json:"networkId"`
	StartAt     int64           `json:"startAt"`
	Duration    int64           `json:"duration"`
	IsFree      bool            `json:"isFree"`
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Extended    json.RawMessage `json:"extended"`
	Genres      []struct {
		Lv1 int `json:"lv1"`
		Lv2 int `json:"lv2"`
		Un1 int `json:"un1"`
		Un2 int `json:"un2"`
	} `json:"genres"`
	Video *struct {
		Type          *string `json:"type"`
		Resolution    *string `json:"resolution"`
		StreamContent *int    `json:"streamContent"`
		ComponentType *int    `json:"componentType"`
	} `json:"video"`
	Audios *[]struct {
		ComponentType int      `json:"componentType"`
		SamplingRate  float64  `json:"samplingRate"`
		Langs         []string `json:"langs"`
	} `json:"audios"`
	Audio *struct {
		ComponentType int     `json:"componentType"`
		SamplingRate  float64 `json:"samplingRate"`
	} `json:"audio"`
	RelatedItems *[]struct {
		Type      *string `json:"type"`
		ServiceID int     `json:"serviceId"`
		EventID   int     `json:"eventId"`
	} `json:"relatedItems"`
}

// isMainProgram は relatedItems からメインの番組情報か判定する (EIT[p/f] 由来の重複を除く) 。
func (p mirakurunProgram) isMainProgram() bool {
	if p.RelatedItems == nil {
		return true
	}
	for _, item := range *p.RelatedItems {
		// Mirakurun 3.8 以下では type が存在しない
		if item.Type == nil {
			return true
		}
		switch *item.Type {
		case "movement", "relay":
			return true
		case "shared":
			return item.ServiceID == p.ServiceID && item.EventID == p.EventID
		}
	}
	return false
}

// orderedDetail は Python dict と同じく、同名キーは最初の位置のまま値だけを上書きする。
type orderedDetail struct {
	entries   []metadata.DetailEntry
	positions map[string]int
}

func (d *orderedDetail) set(key, value string) {
	if d.positions == nil {
		d.positions = map[string]int{}
	}
	if index, ok := d.positions[key]; ok {
		d.entries[index].Value = value
		return
	}
	d.positions[key] = len(d.entries)
	d.entries = append(d.entries, metadata.DetailEntry{Key: key, Value: value})
}

// iterateJSONObject は JSON object の文字列メンバーを出現順に返す。
func iterateJSONObject(raw json.RawMessage, visit func(key, value string)) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return errors.New("extended is not an object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		visit(keyToken.(string), value)
	}
	return nil
}

// iso639LanguageName は TSInformation.getISO639LanguageCodeName() を移植したもの。
func iso639LanguageName(code string) string {
	switch code {
	case "jpn":
		return "日本語"
	case "eng":
		return "英語"
	case "deu":
		return "ドイツ語"
	case "fra":
		return "フランス語"
	case "ita":
		return "イタリア語"
	case "rus":
		return "ロシア語"
	case "zho":
		return "中国語"
	case "kor":
		return "韓国語"
	case "spa":
		return "スペイン語"
	}
	return "その他の言語"
}

// samplingRateText は str(int(rate / 1000)) + 'kHz' と同じ表記を返す。
func samplingRateText(rate float64) string {
	return strconv.FormatInt(int64(rate/1000), 10) + "kHz"
}

// audioTypeName は COMPONENT_TYPE[0x02].get(componentType, 'Unknown') 。
func audioTypeName(componentType int) string {
	if value, ok := reservations.ComponentTypes[0x02][componentType]; ok {
		return value
	}
	return "Unknown"
}

// UpdateProgramsFromMirakurun は Mirakurun / mirakc の番組情報から programs テーブルを更新する。
// 移植元: Program.updateFromMirakurun()
func UpdateProgramsFromMirakurun(ctx context.Context, db *sql.DB, mirakurun MirakurunSource, now time.Time, logger *slog.Logger) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	body, err := mirakurun.Programs(ctx)
	if err != nil {
		logger.Error("Failed to get programs from Mirakurun / mirakc.", "error", err)
		return fmt.Errorf("failed to get programs from Mirakurun / mirakc: %w", err)
	}
	var programs []mirakurunProgram
	if err = json.Unmarshal(body, &programs); err != nil {
		return err
	}
	duplicates, order, err := loadExistingPrograms(ctx, tx)
	if err != nil {
		return err
	}
	channels, err := scanChannels(ctx, tx, "is_watchable = 1")
	if err != nil {
		return err
	}
	channelByID := map[string]*channelRow{}
	for _, channel := range channels {
		channelByID[channel.ID] = channel
	}

	for _, info := range programs {
		channel, ok := channelByID[fmt.Sprintf("NID%d-SID%03d", info.NetworkID, info.ServiceID)]
		if !ok || !info.isMainProgram() || info.Name == nil {
			continue
		}
		var record programRecord
		record.Title = strings.TrimSpace(reservations.FormatString(*info.Name))
		if info.Description != nil {
			record.Description = strings.TrimSpace(reservations.FormatString(*info.Description))
		}
		var detail orderedDetail
		if len(info.Extended) > 0 && string(info.Extended) != "null" {
			if err = iterateJSONObject(info.Extended, func(head, text string) {
				heading := strings.TrimSpace(strings.ReplaceAll(reservations.FormatString(head), "◇", ""))
				if heading == "" {
					heading = "番組内容"
				}
				body := strings.TrimSpace(reservations.FormatString(text))
				detail.set(heading, body)
				if strings.TrimSpace(record.Description) == "" {
					record.Description = body
				}
			}); err != nil {
				return err
			}
		}
		record.Detail = metadata.DetailJSON(detail.entries)

		start := time.UnixMilli(info.StartAt).In(constants.JST)
		end := time.UnixMilli(info.StartAt + info.Duration).In(constants.JST)
		if now.Sub(end) > 12*time.Hour {
			continue
		}
		programID := fmt.Sprintf("NID%d-SID%03d-EID%d", info.NetworkID, info.ServiceID, info.EventID)
		existing, isDuplicate := duplicates[programID]
		if isDuplicate && existing.title == record.Title && existing.description == record.Description &&
			existing.detailLen == len(detail.entries) && existing.startTime.Equal(start) && existing.endTime.Equal(end) {
			delete(duplicates, programID)
			continue
		}
		if isDuplicate {
			delete(duplicates, programID)
		}
		record.ID = programID
		record.ChannelID = channel.ID
		record.NetworkID = channel.NetworkID
		record.ServiceID = channel.ServiceID
		record.EventID = info.EventID
		record.StartTime = start
		record.IsFree = info.IsFree
		// 終了時間未定 (duration == 1) は、未取得なら 5 分、取得済みなら以前の終了時刻を使う
		if info.Duration == 1 {
			if isDuplicate {
				record.EndTime = existing.endTime
			} else {
				record.EndTime = start.Add(5 * time.Minute)
			}
		} else {
			record.EndTime = end
		}

		genres := []metadata.Genre{}
		for _, genre := range info.Genres {
			major, ok := reservations.FindGenreMajor(genre.Lv1)
			if !ok {
				continue
			}
			item := metadata.Genre{Major: strings.ReplaceAll(major.Major, "／", "・"), Middle: "未定義"}
			for _, middle := range major.Middle {
				if middle.Key == genre.Lv2 {
					item.Middle = strings.ReplaceAll(middle.Name, "／", "・")
					break
				}
			}
			if item.Major == "拡張" {
				if item.Middle != "BS/地上デジタル放送用番組付属情報" {
					continue
				}
				item.Middle = "未定義"
				user := genre.Un1*0x10 + genre.Un2
				for _, entry := range reservations.UserTypes() {
					if entry.Key == user {
						item.Middle = entry.Name
						break
					}
				}
			}
			genres = append(genres, item)
		}
		record.Genres = metadata.GenresJSON(genres)

		if video := info.Video; video != nil {
			unknown := "Unknown"
			if video.StreamContent != nil {
				types, ok := reservations.ComponentTypes[*video.StreamContent]
				if !ok {
					// Python では COMPONENT_TYPE[streamContent] の KeyError で更新全体が失敗する
					return fmt.Errorf("unknown video streamContent %d (%s)", *video.StreamContent, programID)
				}
				record.VideoType = &unknown
				if video.ComponentType != nil {
					if value, ok := types[*video.ComponentType]; ok {
						record.VideoType = &value
					}
				}
			} else {
				record.VideoType = &unknown
			}
			record.VideoCodec = video.Type
			record.VideoResolution = video.Resolution
		}

		if info.Audios != nil {
			audios := *info.Audios
			if len(audios) == 0 || len(audios[0].Langs) == 0 {
				return fmt.Errorf("audios has no primary audio language (%s)", programID)
			}
			describe := func(index int) (string, string, string, error) {
				item := audios[index]
				if len(item.Langs) == 0 {
					return "", "", "", fmt.Errorf("audios[%d] has no language (%s)", index, programID)
				}
				typeName := audioTypeName(item.ComponentType)
				language := iso639LanguageName(item.Langs[0])
				if typeName == "1/0+1/0モード(デュアルモノ)" {
					if len(item.Langs) == 2 {
						language += "+" + iso639LanguageName(item.Langs[1])
					} else {
						language += "+副音声"
					}
				}
				return typeName, language, samplingRateText(item.SamplingRate), nil
			}
			if record.PrimaryAudioType, record.PrimaryAudioLanguage, record.PrimaryAudioSamplingRate, err = describe(0); err != nil {
				return err
			}
			if len(audios) == 2 {
				typeName, language, rate, describeErr := describe(1)
				if describeErr != nil {
					return describeErr
				}
				record.SecondaryAudioType, record.SecondaryAudioLanguage, record.SecondaryAudioSamplingRate = &typeName, &language, &rate
			}
		} else {
			// Mirakurun 3.8 以下向け (副音声・言語コードは取得できない)
			if info.Audio == nil {
				return fmt.Errorf("program has neither audios nor audio (%s)", programID)
			}
			record.PrimaryAudioType = audioTypeName(info.Audio.ComponentType)
			record.PrimaryAudioSamplingRate = samplingRateText(info.Audio.SamplingRate)
			record.PrimaryAudioLanguage = "日本語"
			if record.PrimaryAudioType == "1/0+1/0モード(デュアルモノ)" {
				record.PrimaryAudioLanguage = "日本語+英語"
			}
		}

		if err = saveProgram(ctx, tx, record, isDuplicate); err != nil {
			return err
		}
	}

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
