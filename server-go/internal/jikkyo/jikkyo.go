// Package jikkyo はニコニコ実況 (NX-Jikkyo) のチャンネル対応表を扱う。
// server/app/utils/JikkyoClient.py のうち、ネットワーク ID + サービス ID から
// 実況チャンネル ID を解決する部分を移植したもの。
package jikkyo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// NXJikkyoBaseURL は NX-Jikkyo の API のベース URL。
const NXJikkyoBaseURL = "https://nx-jikkyo.tsukumijima.net/api/v1"

// channelEntry は static/jikkyo_channels.json の 1 エントリ。
type channelEntry struct {
	JikkyoID    int    `json:"jikkyo_id"`
	NetworkID   int    `json:"network_id"`
	ServiceID   string `json:"service_id"`
	Area        string `json:"area"`
	ChannelName string `json:"channel_name"`
}

// nicoChannelIDs は旧来の実況チャンネル ID とニコニコチャンネル ID のマッピング。
// 現在アクティブ (実況可能) なニコニコ実況チャンネルがここに記載されている。
// nil のチャンネルは NX-Jikkyo にのみ存在する実況チャンネル。
// Python 版 JikkyoClient.JIKKYO_CHANNEL_ID_MAP と同じ内容を維持すること。
var nicoChannelIDs = map[string]*string{
	"jk1":   stringPointer("ch2646436"),
	"jk2":   stringPointer("ch2646437"),
	"jk4":   stringPointer("ch2646438"),
	"jk5":   stringPointer("ch2646439"),
	"jk6":   stringPointer("ch2646440"),
	"jk7":   stringPointer("ch2646441"),
	"jk8":   stringPointer("ch2646442"),
	"jk9":   stringPointer("ch2646485"),
	"jk10":  nil,
	"jk11":  nil,
	"jk12":  nil,
	"jk13":  nil,
	"jk14":  nil,
	"jk101": stringPointer("ch2647992"),
	"jk103": nil,
	"jk141": nil,
	"jk151": nil,
	"jk161": nil,
	"jk171": nil,
	"jk181": nil,
	"jk191": nil,
	"jk192": nil,
	"jk193": nil,
	"jk200": nil,
	"jk201": nil,
	"jk211": stringPointer("ch2646846"),
	"jk222": nil,
	"jk236": nil,
	"jk252": nil,
	"jk260": nil,
	"jk263": nil,
	"jk265": nil,
	"jk333": nil,
}

// stringPointer は文字列のポインタを返すヘルパー。
func stringPointer(value string) *string {
	return &value
}

// ChannelMap は実況チャンネルの対応表。
type ChannelMap struct {
	entries []channelEntry
}

// LoadChannelMap は static/jikkyo_channels.json を読み込む。
func LoadChannelMap(staticDir string) (*ChannelMap, error) {
	path := filepath.Join(staticDir, "jikkyo_channels.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var entries []channelEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return &ChannelMap{entries: entries}, nil
}

// Resolve はネットワーク ID + サービス ID に対応する実況チャンネル ID (ex: jk101) と
// ニコニコチャンネル ID (NX-Jikkyo にのみ存在する場合は空文字列) を取得する。
// 対応する実況チャンネルが存在しない場合は found に false を返す。
func (m *ChannelMap) Resolve(networkID int, serviceID int) (jikkyoID string, nicoChannelID string, found bool) {
	if m == nil {
		return "", "", false
	}
	for _, entry := range m.entries {
		// jikkyo_channels.json に定義されている NID と SID
		jikkyoServiceID, err := strconv.ParseInt(entry.ServiceID, 0, 64)
		if err != nil {
			continue
		}

		matched := false
		// NID と SID が一致する (BS・CS の場合はこれだけで OK)
		if networkID == entry.NetworkID && int64(serviceID) == jikkyoServiceID {
			matched = true
		} else if 0x7880 <= networkID && networkID <= 0x7fef && entry.NetworkID == 15 {
			// jikkyo_channels.json 記載の地上波の NID は 15 で固定なので、地上波なら SID のみで特定する
			// サブチャンネル用に 1つ前・2つ前のサービス ID も許容する
			// (たとえば SID 1025 (NHK総合2・東京) は 1024 (NHK総合1・東京) の定義に一致する)
			if int64(serviceID) == jikkyoServiceID ||
				int64(serviceID)-1 == jikkyoServiceID ||
				int64(serviceID)-2 == jikkyoServiceID {
				matched = true
			}
		}
		if !matched || entry.JikkyoID == -1 {
			continue
		}

		candidate := fmt.Sprintf("jk%d", entry.JikkyoID)
		// jikkyo_channels.json には現在は存在しない実況チャンネルの ID (ex: jk256) が含まれているため、
		// 対照表に存在するかをチェックする
		value, exists := nicoChannelIDs[candidate]
		if !exists {
			continue
		}
		if value == nil {
			return candidate, "", true
		}
		return candidate, *value, true
	}
	return "", "", false
}

// WatchSessionURL は NX-Jikkyo の視聴セッション維持用 WebSocket API の URL を返す。
func WatchSessionURL(jikkyoID string) string {
	return fmt.Sprintf("%s/channels/%s/ws/watch", NXJikkyoBaseURL, jikkyoID)
}

// CommentSessionURL は NX-Jikkyo のコメント受信用 WebSocket API の URL を返す。
func CommentSessionURL(jikkyoID string) string {
	return fmt.Sprintf("%s/channels/%s/ws/comment", NXJikkyoBaseURL, jikkyoID)
}
