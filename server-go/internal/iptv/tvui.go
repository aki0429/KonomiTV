package iptv

import (
	"encoding/json"
	"os"
	"strconv"
)

// LoadTVUIChannelIDs は指定されたユーザーがテレビ視聴 UI に登録した IPTV チャンネルの
// display_channel_id の一覧を読み込む。
//
// 保存形式は {ユーザーキー: [display_channel_id, ...]} の辞書。
// 旧形式 (グローバルな配列) のファイルが存在する場合は、ユーザーごとに分離できないため無視する。
func (m *Manager) LoadTVUIChannelIDs(userKey string) []string {
	displayChannelIDs := loadTVUIRegistry(m.tvuiChannelsPath(), m.logger.Warn)[userKey]
	if displayChannelIDs == nil {
		return []string{}
	}
	return displayChannelIDs
}

// LoadTVUIRegistry はテレビ視聴 UI への IPTV チャンネル登録を、ユーザーキーごとに読み込む。
func (m *Manager) LoadTVUIRegistry() map[string][]string {
	registry := loadTVUIRegistry(m.tvuiChannelsPath(), m.logger.Warn)
	for key, value := range registry {
		if value == nil {
			registry[key] = []string{}
		}
	}
	return registry
}

// loadTVUIRegistry は登録内容のファイルを読み込む。ファイルが無い (または内容が不正な) 場合は空のマップを返す。
func loadTVUIRegistry(path string, warn func(string, ...any)) map[string][]string {
	registry := map[string][]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return registry
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		warn("Failed to load IPTV TV UI channels", "error", err)
		return registry
	}
	for userKey, value := range raw {
		var displayChannelIDs []string
		if err := json.Unmarshal(value, &displayChannelIDs); err != nil {
			continue
		}
		registry[userKey] = displayChannelIDs
	}
	return registry
}

// SaveTVUIChannelIDs は指定されたユーザーの IPTV チャンネルの登録内容を保存する。
func (m *Manager) SaveTVUIChannelIDs(userKey string, displayChannelIDs []string) error {
	registry := m.LoadTVUIRegistry()
	registry[userKey] = displayChannelIDs
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	return writeJSONFile(m.tvuiChannelsPath(), registry)
}

// RegisterTVUIChannel は指定されたユーザーのテレビ視聴 UI に IPTV チャンネルを登録する
// (既に登録済みの場合は末尾に移動する) 。
//
// 登録数が上限を超える場合は、古いものから削除する。
func (m *Manager) RegisterTVUIChannel(userKey string, displayChannelID string) []string {
	displayChannelIDs := m.LoadTVUIChannelIDs(userKey)
	// 既に登録済みの場合は一旦削除して末尾 (最新) に移動する
	filtered := make([]string, 0, len(displayChannelIDs))
	for _, item := range displayChannelIDs {
		if item != displayChannelID {
			filtered = append(filtered, item)
		}
	}
	filtered = append(filtered, displayChannelID)
	// 上限を超えた分は古いものから削除する
	if len(filtered) > TVUIMaxChannels {
		filtered = filtered[len(filtered)-TVUIMaxChannels:]
	}
	if err := m.SaveTVUIChannelIDs(userKey, filtered); err != nil {
		m.logger.Warn("Failed to save IPTV TV UI channels", "error", err)
	}
	return filtered
}

// UnregisterTVUIChannel は指定されたユーザーのテレビ視聴 UI から IPTV チャンネルを削除する。
func (m *Manager) UnregisterTVUIChannel(userKey string, displayChannelID string) []string {
	displayChannelIDs := m.LoadTVUIChannelIDs(userKey)
	removed := false
	filtered := make([]string, 0, len(displayChannelIDs))
	for _, item := range displayChannelIDs {
		if item == displayChannelID {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return displayChannelIDs
	}
	if err := m.SaveTVUIChannelIDs(userKey, filtered); err != nil {
		m.logger.Warn("Failed to save IPTV TV UI channels", "error", err)
	}
	return filtered
}

// GetTVUIChannels は指定されたユーザーがテレビ視聴 UI に登録した IPTV チャンネルの一覧を、登録順で返す。
func (m *Manager) GetTVUIChannels(userKey string) []*Channel {
	channels := make([]*Channel, 0)
	for _, displayChannelID := range m.LoadTVUIChannelIDs(userKey) {
		if channel := m.GetChannelByDisplayChannelID(displayChannelID); channel != nil {
			channels = append(channels, channel)
		}
	}
	return channels
}

// BuildUserKey はテレビ視聴 UI の IPTV 登録をユーザーごとに分離するためのキーを生成する。
//
// ログインしていない場合は 'anonymous' を返すが、呼び出し側でログイン必須とするかどうかを判断する。
func BuildUserKey(userID *int64) string {
	if userID == nil {
		return "anonymous"
	}
	return "user:" + strconv.FormatInt(*userID, 10)
}
