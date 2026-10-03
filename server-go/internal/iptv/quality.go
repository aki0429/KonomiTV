package iptv

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	// streamInfResolutionPattern は EXT-X-STREAM-INF 行から解像度を抽出する。
	streamInfResolutionPattern = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
	// streamInfBandwidthPattern は EXT-X-STREAM-INF 行からビットレートを抽出する。
	streamInfBandwidthPattern = regexp.MustCompile(`(?:AVERAGE-)?BANDWIDTH=(\d+)`)
	// streamInfCodecsPattern は EXT-X-STREAM-INF 行から CODECS 属性を抽出する。
	streamInfCodecsPattern = regexp.MustCompile(`CODECS="([^"]+)"`)
)

// ParseHLSQualities は HLS のマスタープレイリストから、配信されている画質 (バリアント) の一覧を抽出する。
//
// メディアプレイリスト (バリアントを含まないもの) の場合は空の一覧を返す。
func ParseHLSQualities(content string) *QualityResult {
	type entry struct {
		quality   Quality
		height    int
		bandwidth int
	}
	entries := make([]entry, 0)
	var codec *string

	for _, line := range SplitLines(content) {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		resolution := streamInfResolutionPattern.FindStringSubmatch(line)
		bandwidth := streamInfBandwidthPattern.FindStringSubmatch(line)
		codecs := streamInfCodecsPattern.FindStringSubmatch(line)
		item := entry{}
		if resolution != nil {
			width, _ := strconv.Atoi(resolution[1])
			height, _ := strconv.Atoi(resolution[2])
			item.quality.Width = &width
			item.quality.Height = &height
			item.height = height
			item.quality.Name = BuildQualityName(&height)
		}
		if bandwidth != nil {
			value, _ := strconv.Atoi(bandwidth[1])
			item.quality.Bandwidth = &value
			item.bandwidth = value
		}
		if codec == nil && codecs != nil {
			codec = BuildCodecName(&codecs[1])
		}
		entries = append(entries, item)
	}

	// 解像度とビットレートの高い順に並べ、同じ画質名の重複を除外する
	sort.SliceStable(entries, func(i int, j int) bool {
		if entries[i].height != entries[j].height {
			return entries[i].height > entries[j].height
		}
		return entries[i].bandwidth > entries[j].bandwidth
	})
	qualities := make([]Quality, 0, len(entries))
	seenNames := map[string]bool{}
	for _, item := range entries {
		name := ""
		if item.quality.Name != nil {
			name = *item.quality.Name
		}
		if seenNames[name] {
			continue
		}
		seenNames[name] = true
		qualities = append(qualities, item.quality)
	}

	result := &QualityResult{Qualities: qualities, Codec: codec}
	if len(qualities) > 0 {
		// 最も高画質なバリアントの画質を「元配信の画質」とする
		sourceQuality := qualities[0].Name
		result.SourceQuality = sourceQuality
	}
	return result
}

// DetectChannelQualities は IPTV チャンネルの元配信の画質 (解像度・ビットレート) と映像コーデックを検出する。
//
// 検出結果はストリームの URL ごとにキャッシュし、2回目以降は再取得しない。
func (m *Manager) DetectChannelQualities(ctx context.Context, channel *Channel) *QualityResult {
	m.qualityMutex.Lock()
	if result, exists := m.qualityCache[channel.URL]; exists {
		m.qualityMutex.Unlock()
		return result
	}
	m.qualityMutex.Unlock()

	result := &QualityResult{Qualities: []Quality{}}

	// HLS のプレイリストを取得して解析する
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, channel.URL, nil)
	if err == nil {
		userAgent := m.config.IPTV.UserAgent
		if channel.UserAgent != nil {
			userAgent = *channel.UserAgent
		}
		request.Header.Set("User-Agent", userAgent)
		if channel.Referrer != nil {
			request.Header.Set("Referer", *channel.Referrer)
		}
		response, requestErr := m.client.Do(request)
		if requestErr == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == 200 {
				content := string(body)
				if strings.HasPrefix(strings.TrimLeft(content, " \t\r\n"), "#EXTM3U") {
					result = ParseHLSQualities(content)
				}
			}
		} else {
			m.logger.Debug("Failed to detect IPTV channel qualities", "channel", channel.Name, "error", requestErr)
		}
	}

	m.qualityMutex.Lock()
	m.qualityCache[channel.URL] = result
	m.qualityMutex.Unlock()
	return result
}

// DetectChannelsQualities は複数の IPTV チャンネルの元配信の画質を並行して検出する。
//
// すべての検出が完了するまで待つが、1つのチャンネルの失敗が他に影響しないようにする。
func (m *Manager) DetectChannelsQualities(ctx context.Context, channels []*Channel) map[string]*QualityResult {
	semaphore := make(chan struct{}, QualityDetectionConcurrency)
	detected := map[string]*QualityResult{}
	var mutex sync.Mutex
	var waitGroup sync.WaitGroup

	for _, channel := range channels {
		waitGroup.Add(1)
		go func(channel *Channel) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			result := m.DetectChannelQualities(ctx, channel)
			mutex.Lock()
			detected[channel.ID] = result
			mutex.Unlock()
		}(channel)
	}
	waitGroup.Wait()
	return detected
}
