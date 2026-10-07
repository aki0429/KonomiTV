package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// jstZone は KonomiTV のタイムゾーン (JST) 。
var jstZone = constants.JST

// PersistCookiesFunc は Cookie を暗号化して DB に保存するフック。
//
// Python 版では TwitterGraphQLAPI 内で TwitterAccount モデルを直接更新するが、
// Go 版では DB アクセスを internal/api 側に集約するため、この関数を internal/api が設定する。
// 未設定の場合 (テストなど) は何もしない。
var PersistCookiesFunc func(ctx context.Context, account *Account, cookiesTxt string) error

// PersistCookies は Cookie を DB に保存する。
func PersistCookies(ctx context.Context, account *Account, cookiesTxt string) error {
	// 未保存 (Temporary) アカウントは DB 上の行を持たないためスキップする
	if account.ID == 0 {
		return nil
	}
	// アカウント単位のコールバック (internal/api が設定する) を優先する
	if account.PersistCookies != nil {
		return account.PersistCookies(ctx, cookiesTxt)
	}
	if PersistCookiesFunc == nil {
		return nil
	}
	return PersistCookiesFunc(ctx, account, cookiesTxt)
}

// ----------------------------------------------------------------------------
// Python の dict / list アクセスを再現するヘルパー
// ----------------------------------------------------------------------------

// getObject は value.(map[string]any) を (値, 有無) で返す。
func getObject(object map[string]any, key string) (map[string]any, bool) {
	if object == nil {
		return nil, false
	}
	value, exists := object[key]
	if !exists || value == nil {
		return nil, false
	}
	result, ok := value.(map[string]any)
	return result, ok
}

// getListDefault は object[key] を []any として返す (存在しない / 型が違う場合は空スライス) 。
func getListDefault(object map[string]any, key string) []any {
	value, exists := object[key]
	if !exists || value == nil {
		return []any{}
	}
	result, ok := value.([]any)
	if !ok {
		return []any{}
	}
	return result
}

// pyString は Python の str() 相当の文字列化を緩く行う (None → "None") 。
func pyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	}
	return fmt.Sprintf("%v", value)
}

// hasStringPrefix は strings.HasPrefix の薄いラッパー。
func hasStringPrefix(value string, prefix string) bool {
	return strings.HasPrefix(value, prefix)
}

// replaceAll は strings.ReplaceAll の薄いラッパー。
func replaceAll(value string, old string, new string) string {
	return strings.ReplaceAll(value, old, new)
}

// requireInt は JSON 由来の値を int として取り出す (取り出せない場合はエラー) 。
func requireInt(value any) (int, error) {
	converted, ok := pyInt(value)
	if !ok {
		return 0, fmt.Errorf("value is not an integer: %v", value)
	}
	return converted, nil
}

// pyInt は JSON 由来の値を int として取り出す。
func pyInt(value any) (int, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return int(typed), true
		}
		return 0, false
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	}
	return 0, false
}

// requireBool は JSON 由来の値を bool として取り出す (取り出せない場合はエラー) 。
func requireBool(value any) (bool, error) {
	typed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("value is not a boolean: %v", value)
	}
	return typed, nil
}
