package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.Decode で JPEG を扱う
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	xdraw "golang.org/x/image/draw"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// accountIconSize はアイコン画像のリサイズ後のサイズ (Python 版と同じ 512x512) 。
const accountIconSize = 512

// permittedUsernames は /api/users/me と /api/users/token のパスと重複するため利用できないユーザー名。
var permittedUsernames = map[string]bool{"me": true, "token": true}

// userCreateRequest は schemas.UserCreateRequest と互換のリクエスト。
type userCreateRequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
}

// userUpdateRequest は schemas.UserUpdateRequest と互換のリクエスト。
type userUpdateRequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
}

// userUpdateRequestForAdmin は schemas.UserUpdateRequestForAdmin と互換のリクエスト。
type userUpdateRequestForAdmin struct {
	IsAdmin *bool `json:"is_admin"`
}

// accountLinkCreateRequest は schemas.AccountLinkCreateRequest と互換のリクエスト。
type accountLinkCreateRequest struct {
	TwitterAccountID *int64 `json:"twitter_account_id"`
	BlueskyAccountID *int64 `json:"bluesky_account_id"`
}

// decodeJSONBody はリクエストボディを JSON としてデコードする。
func decodeJSONBody(r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return false
	}
	// 全 consumer で末尾の別 JSON / ごみとモデルの null root を拒否する。
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF || string(raw) == "null" {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

// handleUserCreate は POST /api/users (アカウント作成 API) を処理する。
// 最初に作成されたアカウントのみ、特別に管理者権限 (is_admin: true) が付与される。
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var request userCreateRequest
	if !decodeJSONBody(r, &request) || request.Username == nil || request.Password == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	// 同じユーザー名のアカウントがあったら 422 を返す
	// ユーザー名がそのままログイン ID になるので、同じユーザー名のアカウントがあると重複する
	if _, err := database.GetUserByName(r.Context(), s.db, *request.Username); err == nil {
		s.logger.Warn("[UsersRouter][UserCreateAPI] Specified username is duplicated.", "username", *request.Username)
		writeError(w, http.StatusUnprocessableEntity, "Specified username is duplicated")
		return
	} else if !errors.Is(err, database.ErrUserNotFound) {
		s.logger.Error("failed to check duplicated username", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 利用不可なユーザー名だったら 422 を返す
	if permittedUsernames[strings.ToLower(*request.Username)] {
		s.logger.Warn("[UsersRouter][UserCreateAPI] Specified username is not permitted.", "username", *request.Username)
		writeError(w, http.StatusUnprocessableEntity, "Specified username is not permitted")
		return
	}

	// 他のユーザーアカウントがまだ作成されていないなら、特別に管理者権限を付与する
	userCount, err := database.CountUsers(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to count users", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	isAdmin := userCount == 0

	// パスワードをハッシュ化する (passlib と同じ bcrypt コスト12)
	passwordHash, err := auth.HashPassword(*request.Password)
	if err != nil {
		s.logger.Error("failed to hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// ユーザーを作成する
	user, err := database.CreateUser(r.Context(), s.writeDB, *request.Username, passwordHash, isAdmin)
	if err != nil {
		s.logger.Error("failed to create user", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	response, err := s.buildUserResponse(r.Context(), user)
	if err != nil {
		s.logger.Error("failed to build user response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

// handleUserUpdate は PUT /api/users/me (アカウント情報更新 API) を処理する。
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	var request userUpdateRequest
	if !decodeJSONBody(r, &request) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	// ユーザー名を更新（存在する場合）
	if request.Username != nil {
		newUsername := *request.Username

		// 重複しないように、同じユーザー名のアカウントがあったら 422 を返す
		// 新しいユーザー名が現在のユーザー名と同じなら問題ないので除外
		if newUsername != currentUser.Name {
			if _, err := database.GetUserByName(r.Context(), s.db, newUsername); err == nil {
				s.logger.Warn("[UsersRouter][UserUpdateAPI] Specified username is duplicated.", "username", newUsername)
				writeError(w, http.StatusUnprocessableEntity, "Specified username is duplicated")
				return
			} else if !errors.Is(err, database.ErrUserNotFound) {
				s.logger.Error("failed to check duplicated username", "error", err)
				writeError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
		}

		// 利用不可なユーザー名だったら 422 を返す
		if permittedUsernames[strings.ToLower(newUsername)] {
			s.logger.Warn("[UsersRouter][UserUpdateAPI] Specified username is not permitted.", "username", newUsername)
			writeError(w, http.StatusUnprocessableEntity, "Specified username is not permitted")
			return
		}

		if err := database.UpdateUserName(r.Context(), s.writeDB, currentUser.ID, newUsername); err != nil {
			s.logger.Error("failed to update username", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
	}

	// パスワードを更新（存在する場合）
	if request.Password != nil {
		passwordHash, err := auth.HashPassword(*request.Password)
		if err != nil {
			s.logger.Error("failed to hash password", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		if err := database.UpdateUserPassword(r.Context(), s.writeDB, currentUser.ID, passwordHash); err != nil {
			s.logger.Error("failed to update password", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUserDelete は DELETE /api/users/me (アカウント削除 API) を処理する。
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	// アイコン画像が保存されていれば削除する
	s.removeUserIcon(currentUser.ID)

	// 現在ログイン中のユーザーアカウント（自分自身）を削除する
	if err := database.DeleteUser(r.Context(), s.writeDB, currentUser.ID); err != nil {
		s.logger.Error("failed to delete user", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// ユーザーを削除した結果、管理者アカウントがいなくなってしまった場合
	// ID が一番若いアカウントに管理者権限を付与する
	s.reassignAdminIfNeeded(r)

	w.WriteHeader(http.StatusNoContent)
}

// handleSpecifiedUserUpdate は PUT /api/users/{username} (指定ユーザーアカウント情報更新 API) を処理する。
// /api/users/me と異なり、管理者権限の付与/剥奪のみ可能。管理者のみアクセスできる。
func (s *Server) handleSpecifiedUserUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.getSpecifiedUser(w, r)
	if !ok {
		return
	}

	var request userUpdateRequestForAdmin
	if !decodeJSONBody(r, &request) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	// 管理者権限を剥奪する場合、この処理によってシステム内に管理者が一人もいなくならないかを確認する
	if request.IsAdmin != nil && *request.IsAdmin == false {
		remainingAdmins, err := database.CountAdminsExcluding(r.Context(), s.db, user.ID)
		if err != nil {
			s.logger.Error("failed to count admins", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		if remainingAdmins == 0 {
			s.logger.Warn("[UsersRouter][SpecifiedUserUpdateAPI] Cannot revoke admin permission because there are no more admins.")
			writeError(w, http.StatusUnprocessableEntity, "Cannot revoke admin permission because there are no more admins")
			return
		}
	}

	// 管理者権限を付与/剥奪する (Python 版と同じく、変更がない場合も更新日時が更新される)
	isAdmin := user.IsAdmin
	if request.IsAdmin != nil {
		isAdmin = *request.IsAdmin
	}
	if err := database.UpdateUserIsAdmin(r.Context(), s.writeDB, user.ID, isAdmin); err != nil {
		s.logger.Error("failed to update is_admin", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleSpecifiedUserDelete は DELETE /api/users/{username} (指定ユーザーアカウント削除 API) を処理する。管理者のみアクセスできる。
func (s *Server) handleSpecifiedUserDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.getSpecifiedUser(w, r)
	if !ok {
		return
	}

	// アイコン画像が保存されていれば削除する
	s.removeUserIcon(user.ID)

	// 指定されたユーザーを削除する
	if err := database.DeleteUser(r.Context(), s.writeDB, user.ID); err != nil {
		s.logger.Error("failed to delete user", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// ユーザーを削除した結果、管理者アカウントがいなくなってしまった場合
	s.reassignAdminIfNeeded(r)

	w.WriteHeader(http.StatusNoContent)
}

// handleUserUpdateIcon は PUT /api/users/me/icon (アカウントアイコン画像更新 API) を処理する。
func (s *Server) handleUserUpdateIcon(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	// multipart/form-data を解析する (アップロード上限は 32MB)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}
	file, fileHeader, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}
	defer func() { _ = file.Close() }()

	// MIME タイプが image/jpeg or image/png 以外
	contentType := fileHeader.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/png" {
		s.logger.Warn("[UsersRouter][UserUpdateIconAPI] Please upload JPEG or PNG image.", "content_type", contentType)
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}

	// 画像をデコードする
	sourceImage, _, err := image.Decode(file)
	if err != nil {
		s.logger.Warn("[UsersRouter][UserUpdateIconAPI] Failed to decode the uploaded image.", "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}

	// 正方形の 512x512 PNG にリサイズして保存する
	resizedImage := resizeToSquarePNG(sourceImage, accountIconSize)
	iconDirectory := filepath.Join(s.paths.DataDir, "account-icons")
	if err := os.MkdirAll(iconDirectory, 0o755); err != nil {
		s.logger.Error("failed to create account icons directory", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	iconPath := filepath.Join(iconDirectory, fmt.Sprintf("%02d.png", currentUser.ID))
	outputFile, err := os.Create(iconPath)
	if err != nil {
		s.logger.Error("failed to create icon file", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if err := png.Encode(outputFile, resizedImage); err != nil {
		_ = outputFile.Close()
		s.logger.Error("failed to save icon file", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if err := outputFile.Close(); err != nil {
		s.logger.Error("failed to close icon file", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAccountLinkCreate は POST /api/users/me/account-links (Twitter / Bluesky アカウント紐付け作成 API) を処理する。
func (s *Server) handleAccountLinkCreate(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	var request accountLinkCreateRequest
	if !decodeJSONBody(r, &request) || request.TwitterAccountID == nil || request.BlueskyAccountID == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	// リクエストされた Twitter アカウントがログイン中ユーザーの所有物であることを確認する
	twitterAccount, err := database.GetTwitterAccountByIDAndUserID(r.Context(), s.db, *request.TwitterAccountID, currentUser.ID)
	if errors.Is(err, database.ErrUserNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "Specified Twitter account does not exist")
		return
	}
	if err != nil {
		s.logger.Error("failed to get twitter account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// Bluesky 側も同じユーザーに属するレコードだけを許可する
	blueskyAccount, err := database.GetBlueskyAccountByIDAndUserID(r.Context(), s.db, *request.BlueskyAccountID, currentUser.ID)
	if errors.Is(err, database.ErrUserNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "Specified Bluesky account does not exist")
		return
	}
	if err != nil {
		s.logger.Error("failed to get bluesky account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 紐付けを作成する
	// 紐付けは DB の一意制約で一対一を最終保証するため、競合時は実際の重複側を調べて Python 版と同じエラー文を返す
	link, err := database.CreateAccountLink(r.Context(), s.writeDB, currentUser.ID, twitterAccount.ID, blueskyAccount.ID)
	if err != nil {
		if exists, checkErr := database.AccountLinkExistsByTwitterAccountID(r.Context(), s.db, twitterAccount.ID); checkErr == nil && exists {
			writeError(w, http.StatusUnprocessableEntity, "Specified Twitter account is already linked")
			return
		}
		if exists, checkErr := database.AccountLinkExistsByBlueskyAccountID(r.Context(), s.db, blueskyAccount.ID); checkErr == nil && exists {
			writeError(w, http.StatusUnprocessableEntity, "Specified Bluesky account is already linked")
			return
		}
		s.logger.Error("[UsersRouter][AccountLinkCreateAPI] Failed to create account link due to an unexpected integrity error.", "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Failed to create account link")
		return
	}

	writeJSON(w, http.StatusCreated, accountLinkToResponse(link))
}

// handleAccountLinkDelete は DELETE /api/users/me/account-links/{link_id} (Twitter / Bluesky アカウント紐付け解除 API) を処理する。
func (s *Server) handleAccountLinkDelete(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	linkID, err := strconv.ParseInt(r.PathValue("link_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid account link id")
		return
	}

	// 紐付け解除はログイン中ユーザーのリンクレコードだけに限定する
	deleted, err := database.DeleteAccountLink(r.Context(), s.writeDB, linkID, currentUser.ID)
	if err != nil {
		s.logger.Error("failed to delete account link", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if !deleted {
		writeError(w, http.StatusUnprocessableEntity, "Specified account link does not exist")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// reassignAdminIfNeeded は管理者アカウントがいなくなってしまった場合に、ID が一番若いアカウントへ管理者権限を付与する。
func (s *Server) reassignAdminIfNeeded(r *http.Request) {
	adminCount, err := database.CountAdmins(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to count admins", "error", err)
		return
	}
	if adminCount > 0 {
		return
	}
	oldestUserID, err := database.GetOldestUserID(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to get the oldest user", "error", err)
		return
	}
	if oldestUserID == 0 {
		return
	}
	if err := database.UpdateUserIsAdmin(r.Context(), s.writeDB, oldestUserID, true); err != nil {
		s.logger.Error("failed to reassign admin permission", "error", err)
	}
}

// removeUserIcon はユーザーのアイコン画像が保存されていれば削除する。
func (s *Server) removeUserIcon(userID int64) {
	iconPath := filepath.Join(s.paths.DataDir, "account-icons", fmt.Sprintf("%02d.png", userID))
	if err := os.Remove(iconPath); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("failed to remove user icon", "error", err, "path", iconPath)
	}
}

// resizeToSquarePNG は画像を中央で正方形にクロップし、指定されたサイズにリサイズした image.RGBA を返す。
// Python 版 (Pillow) の Image.crop() + Image.resize() と同等の処理。
func resizeToSquarePNG(sourceImage image.Image, size int) *image.RGBA {
	bounds := sourceImage.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	shortSide := width
	if height < shortSide {
		shortSide = height
	}
	cropX := bounds.Min.X + (width-shortSide)/2
	cropY := bounds.Min.Y + (height-shortSide)/2
	cropRectangle := image.Rect(cropX, cropY, cropX+shortSide, cropY+shortSide)

	destination := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(destination, destination.Bounds(), sourceImage, cropRectangle, xdraw.Over, nil)
	return destination
}
