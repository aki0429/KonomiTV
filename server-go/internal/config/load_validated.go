package config

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ServerSettingsLoadOptions は起動時の実機検査と Docker のホストパス変換を指定する。
type ServerSettingsLoadOptions struct {
	Validation ServerSettingsValidationOptions
	Docker     bool
	// AllowLegacyIPTV は旧 Go 版固有のバックエンドを既存の利用者向けに保持する。
	// 設定 API からは有効にしない。
	AllowLegacyIPTV bool
}

// ServerSettingsLoadError は構文の読み込み失敗と、項目ごとの設定検証失敗を区別する。
type ServerSettingsLoadError struct {
	Details []SettingsValidationError
	Cause   error
}

func (e *ServerSettingsLoadError) Error() string {
	if e.Cause != nil {
		return "failed to load server settings: " + e.Cause.Error()
	}
	lines := []string{"invalid server settings"}
	for _, detail := range e.Details {
		names := []string{}
		for _, part := range detail.Loc {
			names = append(names, fmt.Sprint(part))
		}
		lines = append(lines, strings.Join(names, ".")+": "+detail.Msg)
	}
	return strings.Join(lines, "\n")
}
func (e *ServerSettingsLoadError) Unwrap() error { return e.Cause }

// LoadValidated は本家 LoadConfig と同じく既定値を先にマージし、全項目を検証してから
// 実行用の構造体を生成する。失敗時は DB やバックグラウンド処理を開始しない。
func LoadValidated(ctx context.Context, path string, options ServerSettingsLoadOptions) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ServerSettingsLoadError{Cause: err}
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, &ServerSettingsLoadError{Cause: err}
	}
	if len(document.Content) == 0 || document.Content[0].Tag == "!!null" {
		return nil, &ServerSettingsLoadError{Cause: fmt.Errorf("config.yaml is empty")}
	}
	order := map[string][]string{}
	raw, err := settingsFromYAML(document.Content[0], []any{"body"}, order, map[*yaml.Node]bool{})
	if err != nil {
		return nil, &ServerSettingsLoadError{Cause: err}
	}
	input, err := settingsYAMLDictionary(raw)
	if err != nil {
		return nil, &ServerSettingsLoadError{Cause: err}
	}
	// default インスタンスを出力した辞書へマージし、省略されていた URL・エンコーダー・
	// ポートも起動時には検査する。PUT の「明示指定だけ検査」とはここが異なる。
	model := settingsValidationModels["ServerSettings"]
	defaultsValidator := settingsValidator{model: model}
	defaults := defaultsValidator.value(map[string]any{}, model.Schema, []any{"body"}).(map[string]any)
	merged := mergeStartupSettings(defaults, input)
	if options.Docker {
		prefixDockerSettingsPaths(merged)
	}
	options.Validation.AllowLegacyIPTV = options.AllowLegacyIPTV
	body := &SettingsBody{input: merged, order: order}
	normalized, details, fatal := body.ServerSettings(ctx, options.Validation)
	if fatal != nil {
		return nil, &ServerSettingsLoadError{Cause: fatal}
	}
	if len(details) > 0 {
		for i := range details {
			if len(details[i].Loc) > 0 && details[i].Loc[0] == "body" {
				details[i].Loc = details[i].Loc[1:]
			}
			// FastAPI は from_attributes=True、LoadConfig は通常のモデル検証を使う。
			if details[i].Type == "model_attributes_type" && len(details[i].Loc) == 1 {
				section, _ := details[i].Loc[0].(string)
				if schema := model.Schema.Properties[section]; schema != nil {
					name := strings.TrimPrefix(schema.Ref, "#/$defs/")
					details[i].Type = "model_type"
					details[i].Msg = "Input should be a valid dictionary or instance of " + name
					details[i].Ctx = map[string]any{"class_name": name}
				}
			}
		}
		return nil, &ServerSettingsLoadError{Details: details}
	}
	config := Default()
	// Go 固有の IPTV セクションは本家の五セクションと分離して読む。
	// 未知の項目は本家のモデルと同じく検証対象へ混ぜない。
	var extra struct {
		IPTV IPTVConfig `yaml:"iptv"`
	}
	extra.IPTV = config.IPTV
	if options.AllowLegacyIPTV && document.Content[0].Kind == yaml.MappingNode {
		if err := yaml.Unmarshal(data, &extra); err != nil {
			return nil, &ServerSettingsLoadError{Cause: err}
		}
	}
	config.IPTV = extra.IPTV
	general := normalized["general"].(map[string]any)
	config.General.Backend = general["backend"].(string)
	config.General.AlwaysReceiveTVFromMirakurun = general["always_receive_tv_from_mirakurun"].(bool)
	config.General.EDCBURL = general["edcb_url"].(string)
	config.General.MirakurunURL = general["mirakurun_url"].(string)
	config.General.Encoder = general["encoder"].(string)
	config.General.ProgramUpdateInterval, _ = settingsFloat(general["program_update_interval"], false)
	config.General.Debug = general["debug"].(bool)
	config.General.DebugEncoder = general["debug_encoder"].(bool)
	server := normalized["server"].(map[string]any)
	port, err := server["port"].(json.Number).Int64()
	if err != nil || int64(int(port)) != port {
		return nil, &ServerSettingsLoadError{Cause: fmt.Errorf("server.port exceeds the machine integer range")}
	}
	config.Server.Port = int(port)
	config.Server.CustomHTTPSCertificate = settingsOptionalPath(server["custom_https_certificate"])
	config.Server.CustomHTTPSPrivateKey = settingsOptionalPath(server["custom_https_private_key"])
	tv := normalized["tv"].(map[string]any)
	config.TV.PreferredTerrestrialRegion = settingsOptionalPath(tv["preferred_terrestrial_region"])
	config.TV.MaxAliveTime, _ = settingsFloat(tv["max_alive_time"], false)
	config.TV.DebugModeTSPath = settingsOptionalPath(tv["debug_mode_ts_path"])
	video := normalized["video"].(map[string]any)
	config.Video.RecordedFolders = settingsStringSlice(video["recorded_folders"])
	config.Video.ExcludeScanPaths = settingsStringSlice(video["exclude_scan_paths"])
	config.Video.RecordedFoldersPollingInterval, _ = settingsFloat(video["recorded_folders_polling_interval"], false)
	capture := normalized["capture"].(map[string]any)
	config.Capture.UploadFolders = settingsStringSlice(capture["upload_folders"])
	config.Capture.UploadFoldersPollingInterval, _ = settingsFloat(capture["upload_folders_polling_interval"], false)
	// 計算用の float64 へ変換した整数も、GET では元の検証済みの精度で返す。
	config.startupSettings = deepCopyMap(normalized)
	return config, nil
}

func settingsOptionalPath(value any) *string {
	if value == nil {
		return nil
	}
	text := value.(string)
	return &text
}
func settingsStringSlice(value any) []string {
	items := value.([]any)
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = item.(string)
	}
	return result
}

func mergeStartupSettings(base, override map[string]any) map[string]any {
	for key, value := range override {
		baseMap, baseOK := base[key].(map[string]any)
		nextMap, nextOK := value.(map[string]any)
		if baseOK && nextOK {
			base[key] = mergeStartupSettings(baseMap, nextMap)
		} else {
			base[key] = value
		}
	}
	return base
}

// settingsYAMLDictionary は本家の dict(config_raw) と同じトップレベル変換を行う。
func settingsYAMLDictionary(input any) (map[string]any, error) {
	if mapping, ok := input.(map[string]any); ok {
		return mapping, nil
	}
	if text, ok := input.(string); ok && text == "" {
		return map[string]any{}, nil
	}
	if sequence, ok := input.([]any); ok {
		result := map[string]any{}
		for _, entry := range sequence {
			pair, ok := entry.([]any)
			if text, isString := entry.(string); isString {
				pair = []any{}
				for _, character := range text {
					pair = append(pair, string(character))
				}
				ok = true
			}
			if !ok || len(pair) != 2 {
				return nil, fmt.Errorf("config.yaml cannot be converted to a dictionary")
			}
			key, ok := pair[0].(string)
			if !ok {
				return nil, fmt.Errorf("config.yaml keys must be strings")
			}
			result[key] = pair[1]
		}
		return result, nil
	}
	return nil, fmt.Errorf("config.yaml must contain a dictionary")
}

// settingsFromYAML は数値を float64 経由で丸めず、YAML 1.2 のスカラーとして復元する。
// anchors / merge keys と入力順序も保持し、型変換は後段の共通検証器へ任せる。
func settingsFromYAML(node *yaml.Node, location []any, order map[string][]string, active map[*yaml.Node]bool) (any, error) {
	if active[node] {
		return nil, fmt.Errorf("recursive YAML alias")
	}
	active[node] = true
	defer delete(active, node)
	switch node.Kind {
	case yaml.AliasNode:
		return settingsFromYAML(node.Alias, location, order, active)
	case yaml.MappingNode:
		result := map[string]any{}
		keys := []string{}
		explicit := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" {
				merged, err := settingsFromYAML(node.Content[i+1], location, order, active)
				if err != nil {
					return nil, err
				}
				maps := []any{merged}
				if list, ok := merged.([]any); ok {
					maps = list
				}
				for _, candidate := range maps {
					mapping, ok := candidate.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("YAML merge key must refer to a mapping")
					}
					for name, value := range mapping {
						if _, exists := result[name]; !exists {
							result[name] = value
							keys = append(keys, name)
						}
					}
				}
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" {
				continue
			}
			if key.Tag != "!!str" {
				return nil, fmt.Errorf("YAML setting keys must be strings")
			}
			if explicit[key.Value] {
				return nil, fmt.Errorf("duplicate YAML key %q", key.Value)
			}
			explicit[key.Value] = true
			value, err := settingsFromYAML(node.Content[i+1], settingsChild(location, key.Value), order, active)
			if err != nil {
				return nil, err
			}
			if _, exists := result[key.Value]; !exists {
				keys = append(keys, key.Value)
			}
			result[key.Value] = value
		}
		order[settingsPathKey(location)] = keys
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, len(node.Content))
		for i, item := range node.Content {
			value, err := settingsFromYAML(item, settingsChild(location, i), order, active)
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	case yaml.ScalarNode:
		if node.Tag == "!!int" || node.Tag == "!!float" && node.Style&yaml.TaggedStyle == 0 && settingsYAMLDecimalInteger(node.Value) {
			text := strings.ReplaceAll(node.Value, "_", "")
			base := 10
			unsigned := strings.TrimLeft(text, "+-")
			if strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0b") {
				base = 0
			}
			if number, ok := new(big.Int).SetString(text, base); ok {
				return json.Number(number.String()), nil
			}
			return nil, fmt.Errorf("invalid YAML integer")
		}
		if node.Tag == "!!float" {
			text := strings.ToLower(strings.ReplaceAll(node.Value, "_", ""))
			switch text {
			case ".nan":
				return math.NaN(), nil
			case ".inf", "+.inf":
				return math.Inf(1), nil
			case "-.inf":
				return math.Inf(-1), nil
			}
			number, err := strconv.ParseFloat(text, 64)
			if err == nil || math.IsInf(number, 0) {
				return number, nil
			}
			return nil, err
		}
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		if node.Tag == "!!binary" {
			return settingsYAMLBinary([]byte(value.(string))), nil
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported YAML node")
	}
}

// yaml.v3 は machine int を超えた暗黙の整数を float と分類するため、元の字句で補正する。
func settingsYAMLDecimalInteger(text string) bool {
	text = strings.TrimLeft(text, "+-")
	if text == "" {
		return false
	}
	for _, character := range text {
		if character != '_' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

type settingsYAMLBinary []byte

func settingsText(input any) (string, bool) {
	if value, ok := input.(string); ok {
		return value, true
	}
	if value, ok := input.(settingsYAMLBinary); ok && utf8.Valid(value) {
		return string(value), true
	}
	return "", false
}

// prefixDockerSettingsPaths は本家と同じ順序で変換し、不正な項目に出会えば残りを
// 変更しない。正しい型かどうかは、その後のモデル検証でまとめて返す。
func prefixDockerSettingsPaths(settings map[string]any) {
	video, ok := settings["video"].(map[string]any)
	if !ok {
		return
	}
	if !prefixDockerFolderList(video, "recorded_folders") {
		return
	}
	if exclude, exists := video["exclude_scan_paths"]; exists {
		items, ok := settingsIterable(exclude)
		if !ok {
			return
		}
		result := []any{}
		for _, item := range items {
			if text, ok := item.(string); ok {
				text = strings.TrimFunc(text, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
				if text != "" {
					result = append(result, "/host-rootfs"+text)
				}
			}
		}
		video["exclude_scan_paths"] = result
	}
	capture, ok := settings["capture"].(map[string]any)
	if !ok || !prefixDockerFolderList(capture, "upload_folders") {
		return
	}
	if tv, ok := settings["tv"].(map[string]any); ok {
		if text, ok := tv["debug_mode_ts_path"].(string); ok {
			tv["debug_mode_ts_path"] = "/host-rootfs" + text
		}
	}
}
func prefixDockerFolderList(section map[string]any, name string) bool {
	items, ok := settingsIterable(section[name])
	if !ok {
		return false
	}
	result := []any{}
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return false
		}
		result = append(result, "/host-rootfs"+text)
	}
	section[name] = result
	return true
}
func settingsIterable(value any) ([]any, bool) {
	switch value := value.(type) {
	case []any:
		return value, true
	case string:
		items := []any{}
		for _, character := range value {
			items = append(items, string(character))
		}
		return items, true
	case map[string]any:
		items := []any{}
		for key := range value {
			items = append(items, key)
		}
		return items, true
	default:
		return nil, false
	}
}
