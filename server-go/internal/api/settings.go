package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// handleClientSettings はクライアント設定取得 API (GET /api/settings/client) を処理する。
// 現在ログイン中のユーザーアカウントのクライアント設定を取得する。
func (s *Server) handleClientSettings(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	// 保存済みの値も本家の response_model と同じ型検査・既定値補完を通す。
	// 壊れた保存値を黙って既定値へ戻さず、サーバー側のエラーとして返す。
	stored, details := config.ValidateClientSettingsJSON([]byte(currentUser.ClientSettings))
	if len(details) > 0 {
		s.logger.Error("[ClientSettingsAPI] Stored client settings are invalid.")
		writeSettingsInternalError(w)
		return
	}
	serialized, err := config.MarshalClientSettingsResponse(stored)
	if err != nil || !json.Valid([]byte(serialized)) {
		s.logger.Error("[ClientSettingsAPI] Failed to serialize the client settings.", "error", err)
		writeSettingsInternalError(w)
		return
	}
	writeRawJSON(w, http.StatusOK, serialized)
}

// handleClientSettingsUpdate はクライアント設定更新 API (PUT /api/settings/client) を処理する。
// 現在ログイン中のユーザーアカウントのクライアント設定を更新する。
func (s *Server) handleClientSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	// ボディを JSON として読み込む
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "There was an error parsing the body")
		return
	}
	clientSettings, details := config.ValidateClientSettingsBody(body, r.Header.Get("Content-Type"))
	// FastAPI は JSON の構文エラーを依存関係より先に返し、型検査は認証の後で返す。
	if len(details) > 0 && details[0].Type == "body_error" {
		writeError(w, http.StatusBadRequest, details[0].Msg)
		return
	}
	if len(details) > 0 && details[0].Type == "json_invalid" {
		s.writeSettingsValidation(w, details)
		return
	}
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	if len(details) > 0 {
		s.writeSettingsValidation(w, details)
		return
	}

	// 現在サーバーに保存されているクライアント設定の最終同期時刻よりも古いクライアント設定が送られてきた場合、エラーを返す。
	lastSyncedAt, _ := numberField(clientSettings, "last_synced_at")
	stored, storedDetails := config.ValidateClientSettingsJSON([]byte(currentUser.ClientSettings))
	if len(storedDetails) > 0 {
		s.logger.Error("[ClientSettingsUpdateAPI] Stored client settings are invalid.")
		writeSettingsInternalError(w)
		return
	}
	if storedLastSyncedAt, ok := numberField(stored, "last_synced_at"); ok && lastSyncedAt < storedLastSyncedAt {
		s.logger.Error("[ClientSettingsUpdateAPI] Client settings are outdated!",
			"last_synced_at", lastSyncedAt, "stored_last_synced_at", storedLastSyncedAt)
		writeError(w, http.StatusUnprocessableEntity,
			"The client settings are outdated. Please update the client settings from the server.")
		return
	}

	// クライアント設定を保存する (Tortoise の JSONField と同じくコンパクトな JSON として保存する) 。
	serialized, err := config.MarshalClientSettings(clientSettings)
	if err != nil {
		writeSettingsInternalError(w)
		return
	}
	if err := database.UpdateUserClientSettings(r.Context(), s.writeDB, currentUser.ID, serialized); err != nil {
		s.logger.Error("[ClientSettingsUpdateAPI] Failed to save the client settings.", "error", err)
		writeSettingsInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSettingsValidation は全項目の検証エラーを本家と同じ detail 配列で返す。
func (s *Server) writeSettingsValidation(w http.ResponseWriter, details []config.SettingsValidationError) {
	data, err := config.SettingsValidationJSON(details)
	if err != nil {
		s.logger.Error("Failed to encode settings validation errors.", "error", err)
		writeSettingsInternalError(w)
		return
	}
	writeRawJSON(w, http.StatusUnprocessableEntity, string(data))
}

// writeSettingsInternalError は本家の未処理例外・応答モデル検証失敗と同じ 500 応答を返す。
func writeSettingsInternalError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, "Internal Server Error")
}

// handleServerSettings はサーバー設定取得 API (GET /api/settings/server) を処理する。
// 現在稼働中の KonomiTV サーバーのサーバー設定を取得する。
func (s *Server) handleServerSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.config.ServerSettingsSnapshot()
	// Python 版 (Pydantic) と同じく、フィールドの定義順・浮動小数点数の形式で出力する
	serialized, err := config.MarshalServerSettingsResponse(settings)
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "There was an error parsing the body")
		return
	}
	parsed, details := config.ParseSettingsBody(body, r.Header.Get("Content-Type"))
	if len(details) > 0 {
		if details[0].Type == "body_error" {
			writeError(w, http.StatusBadRequest, details[0].Msg)
		} else {
			s.writeSettingsValidation(w, details)
		}
		return
	}
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}
	options := config.ServerSettingsValidationOptions{Logger: s.logger, LibraryPath: s.paths.LibraryPath,
		RequestHeaders: http.Header{"User-Agent": []string{dataBroadcastingUserAgent}},
		EDCBStatus: func(ctx context.Context, url string) string {
			client, err := reservations.NewClientFromURLContext(ctx, url)
			if err != nil {
				return "Unknown"
			}
			return client.ServerStatus()
		},
	}
	if s.serverSettingsValidation != nil {
		options = *s.serverSettingsValidation
	}
	normalized, details, fatal := parsed.ServerSettings(r.Context(), options)
	if fatal != nil {
		s.logger.Error("[ServerSettingsUpdateAPI] Settings validation failed.", "error", fatal)
		writeSettingsInternalError(w)
		return
	}
	if len(details) > 0 {
		s.writeSettingsValidation(w, details)
		return
	}
	docker := false
	if environment := platformEnvironment(); environment != nil {
		docker = *environment == "Linux-Docker"
	}
	s.settingsSaveMu.Lock()
	err = config.SaveValidatedServerSettings(s.paths.ConfigYAMLPath, normalized, docker)
	s.settingsSaveMu.Unlock()
	if err != nil {
		s.logger.Error("[ServerSettingsUpdateAPI] Failed to save config.yaml.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to save config.yaml")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
