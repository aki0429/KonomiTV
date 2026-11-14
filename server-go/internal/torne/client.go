// Package torne は本家 TorneClient のトル数取得・キャッシュを提供する。
package torne

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// Count は本家 schemas.ToruCount と同じ番組ごとのトル数。
type Count struct {
	Count         int64  `json:"count"`
	HighlightBase *int64 `json:"highlight_base"`
}

// Channel はトル数の対象サービスと地域を決めるためのチャンネル情報。
type Channel struct {
	Type                 string
	NetworkID, ServiceID int
}

// Credentials は本家 Interlaced(4..9) に対応するリクエスト認証情報。
type Credentials struct {
	Username, Password, MobileIDAuth, UserID, DeviceID, Version string
}

// Cache は全地域の取得成功時だけトル数を一括更新する。
type Cache struct {
	mu     sync.RWMutex
	counts map[string]Count
}

// Snapshot は更新と競合しない読み取り用のコピーを返す。
func (c *Cache) Snapshot() map[string]Count {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]Count, len(c.counts))
	for id, count := range c.counts {
		if count.HighlightBase != nil {
			base := *count.HighlightBase
			count.HighlightBase = &base
		}
		result[id] = count
	}
	return result
}

// Fetch は一地域の生レスポンスを取得する。試験ではネットワークを使わず差し替える。
type Fetch func(context.Context, int, time.Time) ([]byte, error)

// HTTPFetcher は本家と同じ認証・フォームで 8 日分のトル数を取得する。
func HTTPFetcher(client *http.Client, endpoint string, credentials Credentials) Fetch {
	return func(ctx context.Context, region int, start time.Time) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		form := url.Values{
			"op": {"opGetToruCount"}, "userid": {credentials.UserID}, "deviceid": {credentials.DeviceID},
			"version": {credentials.Version}, "regionid": {strconv.Itoa(region)},
			"startdate": {start.In(constants.JST).Format("2006-01-02T15:04:05-0700")}, "duration": {"8"},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, fmt.Errorf("create Toru count request: %w", err)
		}
		request.SetBasicAuth(credentials.Username, credentials.Password)
		request.Header.Set("User-Agent", "torne 150 (Android)")
		request.Header.Set("mobileIdAuth", credentials.MobileIDAuth)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			// リクエスト URL や認証情報をエラー文字列へ含めない。
			return nil, fmt.Errorf("fetch Toru counts: request failed")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("fetch Toru counts: HTTP %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		if err != nil {
			return nil, fmt.Errorf("read Toru count response: %w", err)
		}
		if len(body) > 64<<20 {
			return nil, fmt.Errorf("Toru count response exceeds 64 MiB")
		}
		return body, nil
	}
}

type serviceKey struct{ group, sid int }
type eventKey struct{ group, sid, eid int }
type record struct {
	key   eventKey
	count int64
}

// parse はグループヘッダーと全レコードを境界検証し、同一キーの先頭だけ採用する。
func parse(payload []byte) ([]record, map[int]*int64, error) {
	if len(payload) < 8 {
		return nil, nil, fmt.Errorf("Toru count response is shorter than its header")
	}
	groups := int(payload[1])
	offset := 8 * (groups + 1)
	if offset > len(payload) {
		return nil, nil, fmt.Errorf("Toru count response is shorter than its group headers")
	}
	records := []record{}
	seen := map[eventKey]bool{}
	byGroup := map[int][]int64{}
	for i := 0; i < groups; i++ {
		header := binary.BigEndian.Uint64(payload[8*(i+1):])
		group := int(header >> 60)
		size := uint64(uint32(header)) * 8
		if size > uint64(len(payload)-offset) {
			return nil, nil, fmt.Errorf("Toru count response is shorter than its records")
		}
		end := offset + int(size)
		for offset < end {
			key := eventKey{group, int(binary.BigEndian.Uint16(payload[offset:])), int(binary.BigEndian.Uint16(payload[offset+2:]))}
			count := int64(binary.BigEndian.Uint32(payload[offset+4:]))
			if !seen[key] {
				seen[key] = true
				records = append(records, record{key, count})
				byGroup[group] = append(byGroup[group], count)
			}
			offset += 8
		}
	}
	if offset != len(payload) {
		return nil, nil, fmt.Errorf("Toru count response has unexpected trailing data")
	}
	bases := map[int]*int64{}
	for group, counts := range byGroup {
		var sum float64
		var maximum int64
		for _, count := range counts {
			sum += float64(count)
			if count > maximum {
				maximum = count
			}
		}
		if maximum < 2 {
			continue
		}
		mean := sum / float64(len(counts))
		var variance float64
		for _, count := range counts {
			difference := float64(count) - mean
			variance += difference * difference
		}
		threshold := int64(mean + 2*math.Sqrt(variance/float64(len(counts))))
		if threshold >= maximum {
			threshold = maximum - 1
		}
		base := threshold + 1
		bases[group] = &base
	}
	return records, bases, nil
}

// Update は本家同様に地域別の最大値を選び、一地域でも失敗した回は前回値を保持する。
func (c *Cache) Update(ctx context.Context, channels []Channel, now time.Time, fetch Fetch) error {
	networkIDs := map[serviceKey]int{}
	regions := map[int]bool{}
	for _, channel := range channels {
		group := map[string]int{"GR": 1, "BS": 2, "CS": 5}[channel.Type]
		if group == 0 {
			continue
		}
		networkIDs[serviceKey{group, channel.ServiceID}] = channel.NetworkID
		region := channel.ServiceID >> 10
		if channel.Type == "GR" && region >= 10 && region <= 62 {
			regions[region] = true
		}
	}
	if len(networkIDs) == 0 {
		return nil
	}
	if len(regions) == 0 {
		regions[23] = true
	}
	regionIDs := make([]int, 0, len(regions))
	for region := range regions {
		regionIDs = append(regionIDs, region)
	}
	sort.Ints(regionIDs)
	now = now.In(constants.JST)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, constants.JST)
	updated := map[string]Count{}
	for _, region := range regionIDs {
		payload, err := fetch(ctx, region, start)
		if err != nil {
			return err
		}
		records, bases, err := parse(payload)
		if err != nil {
			return err
		}
		for _, record := range records {
			nid, found := networkIDs[serviceKey{record.key.group, record.key.sid}]
			if !found || record.key.eid == 0 {
				continue
			}
			id := fmt.Sprintf("NID%d-SID%03d-EID%d", nid, record.key.sid, record.key.eid)
			if current, exists := updated[id]; !exists || record.count > current.Count {
				updated[id] = Count{record.count, bases[record.key.group]}
			}
		}
	}
	c.mu.Lock()
	c.counts = updated
	c.mu.Unlock()
	return nil
}
