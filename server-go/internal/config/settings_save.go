package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// settingsSaveMu は同一プロセス内の read/modify/replace を直列化する。
// 更新ごとに固有の一時ファイルを使い、失敗時にも以前の設定を保持する。
var settingsSaveMu sync.Mutex

// SaveServerSettings は YAML の構造を更新し、コメントを保持して安全に保存する。
// 複数行スカラーも値ノード全体を置換するため、旧値の継続行は残らない。
func SaveServerSettings(yamlPath string, settings map[string]any) error {
	settingsSaveMu.Lock()
	defer settingsSaveMu.Unlock()

	// シンボリックリンクのリンク先を更新し、リンク自体を通常ファイルへ置換しない。
	target, err := filepath.EvalSymlinks(yamlPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		if err != io.EOF {
			return fmt.Errorf("decode config: %w", err)
		}
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	// 複数 document を黙って切り捨てない。重複キーなども map への decode で検出する。
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("config must contain a single YAML document")
	}
	var original map[string]any
	if err := document.Decode(&original); err != nil {
		return fmt.Errorf("validate original config: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config must be a YAML mapping")
	}
	patch, err := settingsNode(settings)
	if err != nil {
		return err
	}
	mergeSettingsNode(document.Content[0], patch)
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(4)
	if err := encoder.Encode(&document); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("close config encoder: %w", err)
	}
	// 書き込み前に、YAML と起動時の型・基本制約の両方を検証する。
	var roundTrip map[string]any
	if err := yaml.Unmarshal(output.Bytes(), &roundTrip); err != nil {
		return fmt.Errorf("validate encoded config: %w", err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(output.Bytes(), cfg); err != nil {
		return fmt.Errorf("validate config types: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config values: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(target), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	defer func() { _ = temporary.Close() }()
	// 既存ファイルの mode を継承し、秘密を含む設定の権限を広げない。
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := temporary.Write(output.Bytes()); err != nil {
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := replaceConfigFile(temporary.Name(), target); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// settingsNode は JSON 数値を文字列化せず、決定的な順序の YAML ノードへ変換する。
func settingsNode(value any) (*yaml.Node, error) {
	node := &yaml.Node{}
	switch typed := value.(type) {
	case map[string]any:
		node.Kind, node.Tag = yaml.MappingNode, "!!map"
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := settingsNode(typed[key])
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
	case []any:
		node.Kind, node.Tag = yaml.SequenceNode, "!!seq"
		for _, item := range typed {
			child, err := settingsNode(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
	case json.Number:
		if !json.Valid([]byte(typed.String())) {
			return nil, fmt.Errorf("invalid config number")
		}
		if err := yaml.Unmarshal([]byte(typed.String()), node); err != nil {
			return nil, fmt.Errorf("encode config number: %w", err)
		}
		node = node.Content[0]
	default:
		if err := node.Encode(value); err != nil {
			return nil, fmt.Errorf("encode config value: %w", err)
		}
	}
	return node, nil
}

// mergeSettingsNode は既存キーの順序とコメントを保ち、未知の設定も残す。
func mergeSettingsNode(target, patch *yaml.Node) {
	for index := 0; index < len(patch.Content); index += 2 {
		key, value := patch.Content[index], patch.Content[index+1]
		found := false
		for current := 0; current < len(target.Content); current += 2 {
			if target.Content[current].Value != key.Value {
				continue
			}
			old := target.Content[current+1]
			if old.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
				mergeSettingsNode(old, value)
			} else {
				value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
				// 引用スタイルは文字列間でのみ引き継ぐ。ブロック値の旧継続行は再利用しない。
				if old.Tag == "!!str" && value.Tag == "!!str" {
					value.Style = old.Style
				}
				target.Content[current+1] = value
			}
			found = true
			break
		}
		if !found {
			target.Content = append(target.Content, key, value)
		}
	}
}
