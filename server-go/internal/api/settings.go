package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// handleClientSettings はクライアント設定取得 API (GET /api/settings/client) を処理する。
// 現在ログイン中のユーザーアカウントのクライアント設定を取得する。
func (s *Server) handleClientSettings(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	// Python 版は response_model (Pydantic の ClientSettings) を通してから返すため、DB に保存された
	// JSON が空でも既定値で補完された完全な設定が返る。Go 版も同じ形にするため、保存値を
	// 既定値とマージし、Python 版のフィールド定義順・数値形式で出力する。
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(currentUser.ClientSettings), &stored); err != nil {
		// Python 版 (Tortoise の JSONField) は壊れた値・空文字を空の設定として扱うため、同じく空にする
		stored = map[string]any{}
	}
	serialized, err := config.MarshalClientSettings(config.MergeClientSettings(stored))
	if err != nil {
		s.logger.Error("[ClientSettingsAPI] Failed to serialize the client settings.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeRawJSON(w, http.StatusOK, serialized)
}

// handleClientSettingsUpdate はクライアント設定更新 API (PUT /api/settings/client) を処理する。
// 現在ログイン中のユーザーアカウントのクライアント設定を更新する。
func (s *Server) handleClientSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	// ボディを JSON として読み込む
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}
	var requestBody map[string]any
	if err := json.Unmarshal(body, &requestBody); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}

	// Pydantic の ClientSettings と同じく、不足している項目をデフォルト値で補完し、未知の項目は無視する
	clientSettings := config.MergeClientSettings(requestBody)

	// 現在サーバーに保存されているクライアント設定の最終同期時刻よりも古いクライアント設定が送られてきた場合、エラーを返す。
	lastSyncedAt, _ := numberField(clientSettings, "last_synced_at")
	var stored map[string]any
	if err := json.Unmarshal([]byte(currentUser.ClientSettings), &stored); err == nil {
		if storedLastSyncedAt, ok := numberField(config.MergeClientSettings(stored), "last_synced_at"); ok && lastSyncedAt < storedLastSyncedAt {
			s.logger.Error("[ClientSettingsUpdateAPI] Client settings are outdated!",
				"last_synced_at", lastSyncedAt, "stored_last_synced_at", storedLastSyncedAt)
			writeError(w, http.StatusUnprocessableEntity,
				"The client settings are outdated. Please update the client settings from the server.")
			return
		}
	}

	// クライアント設定を保存する (Tortoise の JSONField と同じくコンパクトな JSON として保存する) 。
	serialized, err := config.MarshalClientSettings(clientSettings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if err := database.UpdateUserClientSettings(r.Context(), s.writeDB, currentUser.ID, serialized); err != nil {
		s.logger.Error("[ClientSettingsUpdateAPI] Failed to save the client settings.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleServerSettings はサーバー設定取得 API (GET /api/settings/server) を処理する。
// 現在稼働中の KonomiTV サーバーのサーバー設定を取得する。
func (s *Server) handleServerSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := config.LoadServerSettings(s.paths.ConfigYAMLPath)
	if err != nil {
		s.logger.Error("[ServerSettingsAPI] Failed to load config.yaml.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to load config.yaml")
		return
	}
	// Python 版 (Pydantic) と同じく、フィールドの定義順・浮動小数点数の形式で出力する
	serialized, err := config.MarshalServerSettings(settings)
	if err != nil {
		s.logger.Error("[ServerSettingsAPI] Failed to serialize the server settings.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, serialized)
}

// handleServerSettingsUpdate はサーバー設定更新 API (PUT /api/settings/server) を処理する。
// バリデーションが完了したサーバー設定を config.yaml に保存する。
func (s *Server) handleServerSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid JSON body")
		return
	}

	// デフォルト値で補完してからバリデーションする (クライアントはすべての項目を送信するが、
	// 項目が欠けていても Python 版と同じくデフォルト値で補完される) 。
	merged := mergeSettingsWithDefaults(config.DefaultServerSettings(), settings)
	normalized, detail := config.ValidateServerSettings(merged)
	if detail != "" {
		s.logger.Error("[ServerSettingsUpdateAPI] " + detail)
		writeError(w, http.StatusUnprocessableEntity, detail)
		return
	}

	if err := config.SaveServerSettings(s.paths.ConfigYAMLPath, normalized); err != nil {
		s.logger.Error("[ServerSettingsUpdateAPI] Failed to save config.yaml.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to save config.yaml")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mergeSettingsWithDefaults はデフォルト設定にリクエストの設定をマージする。
func mergeSettingsWithDefaults(base map[string]any, override map[string]any) map[string]any {
	for key, value := range override {
		baseValue, ok := base[key]
		baseMap, baseIsMap := baseValue.(map[string]any)
		overrideMap, overrideIsMap := value.(map[string]any)
		if ok && baseIsMap && overrideIsMap {
			base[key] = mergeSettingsWithDefaults(baseMap, overrideMap)
			continue
		}
		base[key] = value
	}
	return base
}

// numberField は JSON オブジェクトから数値フィールドを取り出す。
func numberField(source map[string]any, key string) (float64, bool) {
	value, ok := source[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
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

// writeRawJSON は JSON 文字列をそのままレスポンスとして書き出す。
func writeRawJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		trimmed = "{}"
	}
	_, _ = io.WriteString(w, trimmed+"\n")
}
