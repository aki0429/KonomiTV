package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// serverSettingsDefaultsJSON は Python 版 ServerSettings のデフォルト値 (model_dump(mode='json')) 。
// tools/generate_server_settings_fixture.py で生成する (Python 版と完全に一致させること) 。
//
//go:embed server_settings_defaults.json
var serverSettingsDefaultsJSON []byte

// serverSettingsFixture は server_settings_defaults.json の構造。
type serverSettingsFixture struct {
	// Defaults は Python 版 ServerSettings のデフォルト値。
	Defaults map[string]any `json:"defaults"`
	// Loaded はリポジトリの config.yaml を Python 版 LoadConfig() で読み込んだ結果 (テストで使う) 。
	Loaded map[string]any `json:"loaded"`
	// LoadedJSON は Loaded を Python 版 (Pydantic) でシリアライズした JSON 文字列 (テストで使う) 。
	LoadedJSON string `json:"loaded_json"`
}

// serverSettingsFixtureData は埋め込んだフィクスチャの内容 (初回アクセス時に読み込む) 。
var serverSettingsFixtureData = func() *serverSettingsFixture {
	decoder := json.NewDecoder(strings.NewReader(string(serverSettingsDefaultsJSON)))
	// 数値は json.Number として保持する (整数の項目を 5.0 のように出力しないため)
	decoder.UseNumber()
	fixture := &serverSettingsFixture{}
	if err := decoder.Decode(fixture); err != nil {
		panic("failed to parse server_settings_defaults.json: " + err.Error())
	}
	return fixture
}()

// DefaultServerSettings は Python 版 ServerSettings のデフォルト値を返す (呼び出し元で変更しても安全なコピー) 。
func DefaultServerSettings() map[string]any {
	return deepCopyMap(serverSettingsFixtureData.Defaults)
}

// ServerSettingsFixtureLoaded はリポジトリの config.yaml を Python 版で読み込んだ結果を返す (テスト用) 。
func ServerSettingsFixtureLoaded() map[string]any {
	return deepCopyMap(serverSettingsFixtureData.Loaded)
}

// deepCopyMap は map を再帰的にコピーする。
func deepCopyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = deepCopyValue(value)
	}
	return result
}

// deepCopyValue は値を再帰的にコピーする。
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = deepCopyValue(item)
		}
		return result
	default:
		return value
	}
}

// mergeServerSettings は config.yaml の読み込み結果をデフォルト設定とマージする。
// 移植元: server/app/config.py の LoadConfig() の MergeConfigWithDefaults()
func mergeServerSettings(base map[string]any, override map[string]any) map[string]any {
	merged := deepCopyMap(base)
	for key, value := range override {
		if baseValue, ok := merged[key]; ok {
			baseMap, baseIsMap := baseValue.(map[string]any)
			valueMap, valueIsMap := value.(map[string]any)
			if baseIsMap && valueIsMap {
				merged[key] = mergeServerSettings(baseMap, valueMap)
				continue
			}
		}
		merged[key] = deepCopyValue(value)
	}
	return merged
}

// LoadServerSettings は config.yaml を読み込み、デフォルト値で補完したサーバー設定を返す。
// 値のバリデーションは行わない (Python 版の LoadConfig(bypass_validation=True) に相当) 。
func LoadServerSettings(yamlPath string) (map[string]any, error) {
	raw := map[string]any{}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config.yaml was not found: %s", yamlPath)
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to load config.yaml: %w", err)
		}
	}
	return mergeServerSettings(DefaultServerSettings(), raw), nil
}

// ValidateServerSettings はサーバー設定のバリデーションと正規化を行う。
// 戻り値は正規化された設定とエラーメッセージ (問題がない場合は空文字列) 。
//
// Python 版のカスタムバリデーターのうち、EDCB / Mirakurun への接続確認・ポートの使用状況・
// エンコーダーの対応状況のチェックは Go 版では行わない (サーバー起動時にも検証されないため) 。
func ValidateServerSettings(settings map[string]any) (map[string]any, string) {
	normalized := deepCopyMap(settings)

	general, ok := normalized["general"].(map[string]any)
	if !ok {
		return nil, "general セクションがありません。"
	}
	server, ok := normalized["server"].(map[string]any)
	if !ok {
		return nil, "server セクションがありません。"
	}
	tv, ok := normalized["tv"].(map[string]any)
	if !ok {
		return nil, "tv セクションがありません。"
	}
	iptv, ok := normalized["iptv"].(map[string]any)
	if !ok {
		return nil, "iptv セクションがありません。"
	}

	// backend
	backend, err := requireString(general, "backend")
	if err != nil {
		return nil, err.Error()
	}
	if !containsString(validBackends, backend) {
		return nil, fmt.Sprintf("backend には %s のいずれかを指定してください。", strings.Join(validBackends, "・"))
	}

	// encoder
	encoder, err := requireString(general, "encoder")
	if err != nil {
		return nil, err.Error()
	}
	if !containsString(validEncoders, encoder) {
		return nil, fmt.Sprintf("encoder には %s のいずれかを指定してください。", strings.Join(validEncoders, "・"))
	}

	// program_update_interval (0.1 以上)
	interval, err := requireFloat(general, "program_update_interval")
	if err != nil {
		return nil, err.Error()
	}
	if interval < 0.1 {
		return nil, "program_update_interval には 0.1 以上の数値を指定してください。"
	}
	general["program_update_interval"] = interval

	// edcb_url / mirakurun_url (末尾のスラッシュありに統一する)
	edcbURL, err := requireString(general, "edcb_url")
	if err != nil {
		return nil, err.Error()
	}
	if !strings.HasPrefix(edcbURL, "tcp://") {
		return nil, "edcb_url には tcp:// から始まる URL を指定してください。"
	}
	general["edcb_url"] = strings.TrimRight(edcbURL, "/") + "/"

	mirakurunURL, err := requireString(general, "mirakurun_url")
	if err != nil {
		return nil, err.Error()
	}
	if !strings.HasPrefix(mirakurunURL, "http://") && !strings.HasPrefix(mirakurunURL, "https://") {
		return nil, "mirakurun_url には http:// または https:// から始まる URL を指定してください。"
	}
	general["mirakurun_url"] = strings.TrimRight(mirakurunURL, "/") + "/"

	// port (1024 ~ 65525)
	port, err := requireInt(server, "port")
	if err != nil {
		return nil, err.Error()
	}
	if port < 1024 || port > 65525 {
		return nil, "ポート番号の設定が不正なため、KonomiTV を起動できません。\n設定したポート番号が 1024 ~ 65525 (65535 ではない) の間に収まっているかを確認してください。"
	}
	server["port"] = port

	// max_alive_time (1 以上)
	maxAliveTime, err := requireInt(tv, "max_alive_time")
	if err != nil {
		return nil, err.Error()
	}
	if maxAliveTime < 1 {
		return nil, "max_alive_time には 1 以上の整数を指定してください。"
	}
	tv["max_alive_time"] = maxAliveTime

	// cache_ttl (1 以上)
	cacheTTL, err := requireInt(iptv, "cache_ttl")
	if err != nil {
		return nil, err.Error()
	}
	if cacheTTL < 1 {
		return nil, "cache_ttl には 1 以上の整数を指定してください。"
	}
	iptv["cache_ttl"] = cacheTTL

	// request_timeout (1.0 以上)
	requestTimeout, err := requireFloat(iptv, "request_timeout")
	if err != nil {
		return nil, err.Error()
	}
	if requestTimeout < 1.0 {
		return nil, "request_timeout には 1.0 以上の数値を指定してください。"
	}
	iptv["request_timeout"] = requestTimeout

	// iptv.sources は URL またはローカルファイルの絶対パス
	if _, ok := iptv["sources"].([]any); !ok {
		return nil, "iptv.sources には配列を指定してください。"
	}

	return normalized, ""
}

// requireString は map から文字列を取り出す。
func requireString(source map[string]any, key string) (string, error) {
	value, ok := source[key]
	if !ok {
		return "", fmt.Errorf("%s がありません。", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s には文字列を指定してください。", key)
	}
	return text, nil
}

// requireInt は map から整数を取り出す。
func requireInt(source map[string]any, key string) (int, error) {
	value, ok := source[key]
	if !ok {
		return 0, fmt.Errorf("%s がありません。", key)
	}
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case float64:
		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("%s には整数を指定してください。", key)
		}
		return int(typed), nil
	case json.Number:
		if converted, ok := toInt(typed); ok {
			return converted, nil
		}
		return 0, fmt.Errorf("%s には整数を指定してください。", key)
	default:
		return 0, fmt.Errorf("%s には整数を指定してください。", key)
	}
}

// requireFloat は map から浮動小数点数を取り出す。
func requireFloat(source map[string]any, key string) (float64, error) {
	value, ok := source[key]
	if !ok {
		return 0, fmt.Errorf("%s がありません。", key)
	}
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case json.Number:
		converted, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s には数値を指定してください。", key)
		}
		return converted, nil
	default:
		return 0, fmt.Errorf("%s には数値を指定してください。", key)
	}
}

// containsString は配列に指定された文字列が含まれるかどうかを返す。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// SaveServerSettings はサーバー設定を config.yaml に書き込む。
//
// Python 版の SaveConfig() は ruamel.yaml でコメントやフォーマットを保持して書き込むが、
// Go 版では行単位で値を置き換えることでコメントを保持する。
// ファイル全体を書き換えるのではなく、該当するキーの行のみを置き換える。
func SaveServerSettings(yamlPath string, settings map[string]any) error {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	// セクションごとに、セクション内のキーを更新する
	// ファイルに存在しないセクション・キーは末尾に追加する
	sections := []string{}
	for _, section := range []string{"general", "server", "tv", "video", "capture", "iptv"} {
		if _, ok := settings[section]; ok {
			sections = append(sections, section)
		}
	}
	for section := range settings {
		if !containsString(sections, section) {
			sections = append(sections, section)
		}
	}

	updated := lines
	for _, section := range sections {
		sectionSettings, ok := settings[section].(map[string]any)
		if !ok {
			continue
		}
		updated = updateSection(updated, section, sectionSettings)
	}

	// 一時ファイルに書き出してから置き換える (書き込みに失敗しても元のファイルを壊さないため)
	temporaryPath := yamlPath + ".tmp"
	if err := os.WriteFile(temporaryPath, []byte(strings.Join(updated, "\n")), 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, yamlPath); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}

// updateSection は指定されたセクションのキーを更新する。
func updateSection(lines []string, section string, settings map[string]any) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	// 出力順を安定させる (YAML の定義順に近い並びにする)
	sort.Strings(keys)

	// セクションの開始行と終了行を探す
	start := -1
	end := len(lines)
	for index, line := range lines {
		if isSectionHeader(line, section) {
			start = index
			continue
		}
		if start >= 0 && isSectionHeader(line, "") == false && isTopLevelLine(line) {
			end = index
			break
		}
	}
	if start < 0 {
		// セクション自体が存在しない場合は末尾に追加する
		result := append([]string{}, lines...)
		// 末尾の空行を取り除いてから追加する
		for len(result) > 0 && strings.TrimSpace(result[len(result)-1]) == "" {
			result = result[:len(result)-1]
		}
		result = append(result, "", section+":")
		for _, key := range keys {
			result = append(result, formatSetting(key, settings[key], "    ")...)
		}
		result = append(result, "")
		return result
	}

	// セクション内のキーを更新する
	result := make([]string, 0, len(lines))
	result = append(result, lines[:start+1]...)
	remaining := map[string]any{}
	for _, key := range keys {
		remaining[key] = settings[key]
	}
	index := start + 1
	for index < end {
		line := lines[index]
		key, ok := keyOfSetting(line)
		if ok {
			if value, exists := remaining[key]; exists {
				// 既存のキーの行を置き換える (値の行数は変わる可能性があるため、古い値を読み飛ばす)
				result = append(result, formatSetting(key, value, "    ")...)
				delete(remaining, key)
				index++
				// 複数行の値 (フローシーケンスなど) を読み飛ばす
				for index < end && isContinuationLine(lines[index]) {
					index++
				}
				continue
			}
		}
		result = append(result, line)
		index++
	}
	// セクションに存在しなかったキーをセクションの末尾に追加する
	if len(remaining) > 0 {
		added := []string{}
		for _, key := range keys {
			if value, exists := remaining[key]; exists {
				added = append(added, formatSetting(key, value, "    ")...)
			}
		}
		// セクション末尾の空行の前に挿入する
		insertAt := len(result)
		for insertAt > start+1 && strings.TrimSpace(result[insertAt-1]) == "" {
			insertAt--
		}
		head := append([]string{}, result[:insertAt]...)
		tail := append([]string{}, result[insertAt:]...)
		result = append(append(head, added...), tail...)
	}
	result = append(result, lines[end:]...)
	return result
}

// isTopLevelLine はトップレベルの行 (インデントなし) かどうかを返す。
func isTopLevelLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	return line == strings.TrimLeft(line, " \t")
}

// isSectionHeader は指定されたセクションの見出し行かどうかを返す (section が空文字列の場合はトップレベルの行かどうか) 。
func isSectionHeader(line string, section string) bool {
	if section == "" {
		return isTopLevelLine(line) && !strings.HasPrefix(strings.TrimSpace(line), "#")
	}
	trimmed := strings.TrimRight(line, " \t")
	if trimmed != section+":" {
		return false
	}
	return line == trimmed
}

// keyOfSetting は設定行からキーを取り出す (設定行でない場合は ok に false を返す) 。
func keyOfSetting(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", false
	}
	// トップレベルの行はキーではない
	if line == trimmed {
		return "", false
	}
	index := strings.Index(trimmed, ":")
	if index <= 0 {
		return "", false
	}
	key := trimmed[:index]
	// キーとして使える文字だけか確認する
	for _, character := range key {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '_' && character != '-' {
			return "", false
		}
	}
	return key, true
}

// isContinuationLine は値の続きの行かどうかを返す。
func isContinuationLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return false
	}
	// フローシーケンスの要素や閉じ括弧は値の続き
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "'") || trimmed == "]" || trimmed == "}" {
		return true
	}
	return false
}

// formatSetting は設定を YAML の行として整形する。
func formatSetting(key string, value any, indent string) []string {
	switch typed := value.(type) {
	case nil:
		return []string{indent + key + ": null"}
	case bool:
		return []string{indent + key + ": " + strconv.FormatBool(typed)}
	case int:
		return []string{indent + key + ": " + strconv.Itoa(typed)}
	case int64:
		return []string{indent + key + ": " + strconv.FormatInt(typed, 10)}
	case json.Number:
		// フィクスチャ由来の数値は元の表記 (5.0 / 3600) をそのまま使う
		text := typed.String()
		if !strings.ContainsAny(text, ".eE") {
			// 整数の項目はそのまま出力する
			return []string{indent + key + ": " + text}
		}
		return []string{indent + key + ": " + text}
	case float64:
		// Python 版 (ruamel) は 5.0 のような浮動小数点数を小数部付きで出力するため、合わせる
		text := strconv.FormatFloat(typed, 'f', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return []string{indent + key + ": " + text}
	case string:
		return []string{indent + key + ": '" + strings.ReplaceAll(typed, "'", "''") + "'"}
	case []any:
		if len(typed) == 0 {
			return []string{indent + key + ": []"}
		}
		lines := []string{indent + key + ": ["}
		for _, item := range typed {
			itemText, ok := item.(string)
			if !ok {
				itemText = fmt.Sprintf("%v", item)
			}
			lines = append(lines, indent+"    '"+strings.ReplaceAll(itemText, "'", "''")+"',")
		}
		lines = append(lines, indent+"]")
		return lines
	default:
		return []string{indent + key + ": " + fmt.Sprintf("%v", typed)}
	}
}

// ServerSettingsPath は config.yaml のパスから一時ファイルのパスを返す (テスト用) 。
func ServerSettingsPath(directory string) string {
	return filepath.Join(directory, "config.yaml")
}

// ClientSettingsSampleJSON はクライアント設定のシリアライズの検証用フィクスチャ (テストで使う) 。
//
//go:embed client_settings_sample.json
var ClientSettingsSampleJSON []byte

// clientSettingsDefaultsJSON は Python 版 ClientSettings のデフォルト値 (model_dump(mode='json')) 。
// tools/generate_server_settings_fixture.py で生成する。
//
//go:embed client_settings_defaults.json
var clientSettingsDefaultsJSON []byte

// clientSettingsDefaults は埋め込んだクライアント設定のデフォルト値。
var clientSettingsDefaults = func() map[string]any {
	decoder := json.NewDecoder(strings.NewReader(string(clientSettingsDefaultsJSON)))
	// 数値は json.Number として保持する (整数の項目を 50.0 のように出力しないため)
	decoder.UseNumber()
	defaults := map[string]any{}
	if err := decoder.Decode(&defaults); err != nil {
		panic("failed to parse client_settings_defaults.json: " + err.Error())
	}
	return defaults
}()

// MergeClientSettings はクライアント設定を Python 版 ClientSettings (Pydantic) と同じ形に整える。
// 不足している項目はデフォルト値で補完し、モデルに存在しない項目は無視する。
// 辞書や配列の項目は Pydantic と同じくそのままの値を使う (デフォルト値とのマージは行わない) 。
func MergeClientSettings(source map[string]any) map[string]any {
	merged := make(map[string]any, len(clientSettingsDefaults))
	for key, defaultValue := range clientSettingsDefaults {
		value, ok := source[key]
		if !ok {
			merged[key] = deepCopyValue(defaultValue)
			continue
		}
		// Pydantic と同じく、整数の項目 (PositiveInt) は整数に、浮動小数点数の項目は
		// 浮動小数点数に変換する (JSON の数値は常に float64 として読み込まれるため) 。
		merged[key] = convertClientSettingValue(value, defaultValue)
	}
	return merged
}

// convertClientSettingValue はクライアント設定の値をデフォルト値の型に合わせて変換する。
func convertClientSettingValue(value any, defaultValue any) any {
	number, ok := defaultValue.(json.Number)
	if !ok {
		// 数値以外の項目はそのまま使う (Pydantic と同じくデフォルト値とのマージは行わない)
		return deepCopyValue(value)
	}
	// 整数の項目
	if !strings.ContainsAny(number.String(), ".eE") {
		if converted, ok := toInt(value); ok {
			return converted
		}
		return deepCopyValue(value)
	}
	// 浮動小数点数の項目
	if converted, ok := toFloat64(value); ok {
		return converted
	}
	return deepCopyValue(value)
}

// toFloat64 は値を浮動小数点数に変換する。
func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		converted, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return converted, true
	default:
		return 0, false
	}
}

// toInt は値を整数に変換する。
func toInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		converted, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(converted), true
	default:
		return 0, false
	}
}

// clientSettingsKeyOrder はクライアント設定のキーの並び (Python 版 ClientSettings のフィールドの定義順) 。
var clientSettingsKeyOrder = scanKeyOrder(clientSettingsDefaultsJSON)

// serverSettingsKeyOrder はサーバー設定のキーの並び (Python 版 ServerSettings のフィールドの定義順) 。
// フィクスチャは {"defaults": {...}, "loaded": {...}} の形なので、defaults の中身を走査する。
var serverSettingsKeyOrder = scanKeyOrder(extractJSONField(serverSettingsDefaultsJSON, "defaults"))

// extractJSONField は JSON オブジェクトから指定されたフィールドの生の JSON を取り出す。
func extractJSONField(rawJSON []byte, field string) []byte {
	decoder := json.NewDecoder(strings.NewReader(string(rawJSON)))
	if _, err := decoder.Token(); err != nil {
		panic("failed to parse the fixture: " + err.Error())
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			panic("failed to parse the fixture: " + err.Error())
		}
		key, ok := token.(string)
		if !ok {
			panic("failed to parse the fixture: unexpected token")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			panic("failed to parse the fixture: " + err.Error())
		}
		if key == field {
			return value
		}
	}
	panic("the fixture does not have the field: " + field)
}

// scanKeyOrder は JSON オブジェクトのキーの並びをパスごとに取り出す。
// パスは JSON Pointer 風 (ルートは ""、ネストしたオブジェクトは "general" や "iptv" など) 。
func scanKeyOrder(rawJSON []byte) map[string][]string {
	order := map[string][]string{}
	var walk func(decoder *json.Decoder, path string) error
	walk = func(decoder *json.Decoder, path string) error {
		// オブジェクトの開始
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
			return fmt.Errorf("unexpected token: %v", token)
		}
		keys := []string{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("unexpected token: %v", token)
			}
			keys = append(keys, key)
			// 値の先頭を覗いて、オブジェクトなら再帰する
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			if len(value) > 0 && value[0] == '{' {
				child := key
				if path != "" {
					child = path + "." + key
				}
				if err := walk(json.NewDecoder(strings.NewReader(string(value))), child); err != nil {
					return err
				}
			}
		}
		// オブジェクトの終了
		if _, err := decoder.Token(); err != nil {
			return err
		}
		order[path] = keys
		return nil
	}
	if err := walk(json.NewDecoder(strings.NewReader(string(rawJSON))), ""); err != nil {
		panic("failed to scan the key order: " + err.Error())
	}
	return order
}

// MarshalClientSettings はクライアント設定を JSON 文字列に変換する。
// Tortoise の JSONField (json.dumps(separators=(",", ":"))) と同じくコンパクトな JSON を出力し、
// キーの並びは Python 版 ClientSettings のフィールドの定義順にする。
func MarshalClientSettings(settings map[string]any) (string, error) {
	return encodePythonJSON(settings, clientSettingsKeyOrder, "")
}

// MarshalServerSettings はサーバー設定を JSON 文字列に変換する。
// Pydantic (model_dump_json()) と同じく、キーの並びはフィールドの定義順、浮動小数点数は 5.0 の形式で出力する。
func MarshalServerSettings(settings map[string]any) (string, error) {
	return encodePythonJSON(settings, serverSettingsKeyOrder, "")
}

// encodePythonJSON は Python の json.dumps (ensure_ascii=False, separators=(",", ":")) と
// 同じ形式の JSON 文字列を出力する。
// keyOrder に含まれるオブジェクトはその並びで、含まれないオブジェクトはキーの昇順で出力する。
func encodePythonJSON(value any, keyOrder map[string][]string, path string) (string, error) {
	var builder strings.Builder
	if err := writePythonJSON(&builder, value, keyOrder, path); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// writePythonJSON は encodePythonJSON の実装。
func writePythonJSON(builder *strings.Builder, value any, keyOrder map[string][]string, path string) error {
	switch typed := value.(type) {
	case nil:
		builder.WriteString("null")
	case bool:
		if typed {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	case string:
		builder.WriteString(encodeJSONString(typed))
	case int:
		builder.WriteString(strconv.Itoa(typed))
	case int64:
		builder.WriteString(strconv.FormatInt(typed, 10))
	case float64:
		builder.WriteString(formatPythonFloat(typed))
	case json.Number:
		builder.WriteString(typed.String())
	case []any:
		builder.WriteString("[")
		for index, item := range typed {
			if index > 0 {
				builder.WriteString(",")
			}
			if err := writePythonJSON(builder, item, keyOrder, path); err != nil {
				return err
			}
		}
		builder.WriteString("]")
	case map[string]any:
		keys := keyOrder[path]
		written := map[string]bool{}
		builder.WriteString("{")
		first := true
		for _, key := range keys {
			item, ok := typed[key]
			if !ok {
				continue
			}
			if !first {
				builder.WriteString(",")
			}
			first = false
			builder.WriteString(encodeJSONString(key))
			builder.WriteString(":")
			child := key
			if path != "" {
				child = path + "." + key
			}
			if err := writePythonJSON(builder, item, keyOrder, child); err != nil {
				return err
			}
			written[key] = true
		}
		// keyOrder にないキーはキーの昇順で出力する
		remaining := make([]string, 0, len(typed))
		for key := range typed {
			if !written[key] {
				remaining = append(remaining, key)
			}
		}
		sort.Strings(remaining)
		for _, key := range remaining {
			if !first {
				builder.WriteString(",")
			}
			first = false
			builder.WriteString(encodeJSONString(key))
			builder.WriteString(":")
			child := key
			if path != "" {
				child = path + "." + key
			}
			if err := writePythonJSON(builder, typed[key], keyOrder, child); err != nil {
				return err
			}
		}
		builder.WriteString("}")
	default:
		return fmt.Errorf("unsupported type: %T", value)
	}
	return nil
}

// encodeJSONString は Python の json.dumps (ensure_ascii=False) と同じ文字列リテラルを出力する。
func encodeJSONString(value string) string {
	var builder strings.Builder
	builder.WriteString(`"`)
	for _, character := range value {
		switch character {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		default:
			if character < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, character))
			} else {
				builder.WriteRune(character)
			}
		}
	}
	builder.WriteString(`"`)
	return builder.String()
}

// formatPythonFloat は Python の repr(float) と同じ形式の文字列を返す。
// Python は指数が -4 以上 16 未満のときは固定小数点数 (5.0 や 1700000000.0) 、
// それ以外は指数表記 (1e+16) で出力する。
func formatPythonFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	// 指数表記から 10 の指数を取り出す (例: "1.7e+09" → 9)
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	exponent := 0
	if index := strings.IndexAny(scientific, "eE"); index >= 0 {
		parsed, err := strconv.Atoi(scientific[index+1:])
		if err == nil {
			exponent = parsed
		}
	}
	if exponent >= -4 && exponent < 16 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	return scientific
}
