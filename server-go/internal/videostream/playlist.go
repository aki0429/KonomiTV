package videostream

import (
	"fmt"
	"math"
	"strings"
)

// BuildVirtualPlaylist は Python VideoStream.getVirtualPlaylist と同じ仮想 VOD を構築する。
// セッション管理・キャッシュキー生成は呼び出し側の責務。まだエンコード済みとは限らない。
func BuildVirtualPlaylist(sessionID, cacheKey string, frameRate, duration float64, audio string) string {
	segmentDuration := ComputeSegmentDurationSeconds(frameRate)
	count := max(1, int(math.Ceil(duration/segmentDuration)))
	// 最終セグメントを録画時間へ切り詰め、ゼロ長録画でも Python と同じ 1ms を保持する。
	durations := make([]float64, count)
	target := 0.0
	for i := range count {
		durations[i] = math.Min(segmentDuration, math.Max(duration-float64(i)*segmentDuration, 0.001))
		target = math.Max(target, durations[i])
	}
	var playlist strings.Builder
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(target)))
	suffix := ""
	if audio == "secondary" {
		suffix = "&audio=secondary"
	}
	for i, seconds := range durations {
		fmt.Fprintf(&playlist, "#EXTINF:%.6f,\nsegment?session_id=%s&sequence=%d&cache_key=%s%s\n", seconds, sessionID, i, cacheKey, suffix)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	return playlist.String()
}
