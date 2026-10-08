package epgupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
)

// NXJikkyoChannelsURL は NX-Jikkyo のチャンネル情報 API 。
const NXJikkyoChannelsURL = jikkyo.NXJikkyoBaseURL + "/channels"

// nxJikkyoChannel は NX-Jikkyo のチャンネル情報 API の 1 チャンネル。
type nxJikkyoChannel struct {
	ID      string `json:"id"`
	Threads []struct {
		StartAt     string `json:"start_at"`
		EndAt       string `json:"end_at"`
		JikkyoForce int    `json:"jikkyo_force"`
	} `json:"threads"`
}

// JikkyoStatuses は実況チャンネルごとの最新の実況勢いを保持する。
// Python 版 JikkyoClient のクラス変数と同じく、取得に失敗した回は前回の値を使い続ける。
type JikkyoStatuses struct {
	mu     sync.Mutex
	forces map[string]int
}

// FetchJikkyoChannels は NX-Jikkyo のチャンネル情報 API を取得する関数。
type FetchJikkyoChannels func(ctx context.Context) ([]byte, error)

// HTTPFetchJikkyoChannels は client で NX-Jikkyo のチャンネル情報 API を取得する (タイムアウト 5 秒) 。
func HTTPFetchJikkyoChannels(client *http.Client) FetchJikkyoChannels {
	return func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, NXJikkyoChannelsURL, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("NX-Jikkyo channels API returned HTTP %d", response.StatusCode)
		}
		return io.ReadAll(io.LimitReader(response.Body, 16<<20))
	}
}

// parseJikkyoTime は Python の datetime.fromisoformat() + JST 正規化 (naive は JST とみなす) に相当する。
func parseJikkyoTime(value string) (time.Time, error) {
	normalized := strings.Replace(value, " ", "T", 1)
	if parsed, err := time.Parse(time.RFC3339Nano, normalized); err == nil {
		return parsed.In(constants.JST), nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05.999999999", normalized, constants.JST)
}

// update は API の応答から現在時刻を含むスレッドの実況勢いを記録する (JikkyoClient.updateStatuses) 。
func (s *JikkyoStatuses) update(body []byte, now time.Time, isKnown func(string) bool) error {
	var channels []nxJikkyoChannel
	if err := json.Unmarshal(body, &channels); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forces == nil {
		s.forces = map[string]int{}
	}
	for _, channel := range channels {
		if !isKnown(channel.ID) {
			continue
		}
		for _, thread := range channel.Threads {
			start, err := parseJikkyoTime(thread.StartAt)
			if err != nil {
				return err
			}
			end, err := parseJikkyoTime(thread.EndAt)
			if err != nil {
				return err
			}
			if !now.Before(start) && !now.After(end) {
				s.forces[channel.ID] = thread.JikkyoForce
				break
			}
		}
	}
	return nil
}

func (s *JikkyoStatuses) force(jikkyoID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.forces[jikkyoID]
	return value, ok
}

// UpdateJikkyoStatus は視聴可能なチャンネルの jikkyo_force を NX-Jikkyo の実況勢いで更新する。
// 移植元: Channel.updateJikkyoStatus()
func UpdateJikkyoStatus(ctx context.Context, db *sql.DB, channelMap *jikkyo.ChannelMap, statuses *JikkyoStatuses,
	fetch FetchJikkyoChannels, now time.Time, logger *slog.Logger) error {
	// 取得・解析に失敗した場合は Python 版と同じくステータス更新を中断し、前回の値を使う
	if body, err := fetch(ctx); err == nil {
		if err := statuses.update(body, now, jikkyo.IsKnownJikkyoID); err != nil {
			logger.Warn("Failed to parse NX-Jikkyo channel statuses.", "error", err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id, network_id, service_id FROM channels WHERE is_watchable = 1 ORDER BY rowid`)
	if err != nil {
		return err
	}
	type target struct {
		id       string
		nid, sid int
	}
	var targets []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.nid, &item.sid); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range targets {
		jikkyoID, _, found := channelMap.Resolve(item.nid, item.sid)
		if !found {
			continue
		}
		force, ok := statuses.force(jikkyoID)
		// 実況枠が無い、または force が -1 (何らかのエラー) の場合は更新しない
		if !ok || force == -1 {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE channels SET jikkyo_force = ? WHERE id = ?`, force, item.id); err != nil {
			return err
		}
	}
	return nil
}
