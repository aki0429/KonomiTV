package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// ***** レスポンススキーマ (server/app/schemas.py 互換) *****

// userResponse は schemas.User と互換のレスポンス。
type userResponse struct {
	ID                  int64                    `json:"id"`
	Name                string                   `json:"name"`
	IsAdmin             bool                     `json:"is_admin"`
	NiconicoUserID      *int64                   `json:"niconico_user_id"`
	NiconicoUserName    *string                  `json:"niconico_user_name"`
	NiconicoUserPremium *bool                    `json:"niconico_user_premium"`
	TwitterAccounts     []twitterAccountResponse `json:"twitter_accounts"`
	BlueskyAccounts     []blueskyAccountResponse `json:"bluesky_accounts"`
	AccountLinks        []accountLinkResponse    `json:"account_links"`
	CreatedAt           string                   `json:"created_at"`
	UpdatedAt           string                   `json:"updated_at"`
}

// twitterAccountResponse は schemas.TwitterAccount と互換のレスポンス。
type twitterAccountResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ScreenName string `json:"screen_name"`
	IconURL    string `json:"icon_url"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// blueskyAccountResponse は schemas.BlueskyAccount と互換のレスポンス。
type blueskyAccountResponse struct {
	ID        int64  `json:"id"`
	DID       string `json:"did"`
	Handle    string `json:"handle"`
	Name      string `json:"name"`
	IconURL   string `json:"icon_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// accountLinkResponse は schemas.AccountLink と互換のレスポンス。
type accountLinkResponse struct {
	ID             int64                  `json:"id"`
	TwitterAccount twitterAccountResponse `json:"twitter_account"`
	BlueskyAccount blueskyAccountResponse `json:"bluesky_account"`
	CreatedAt      string                 `json:"created_at"`
	UpdatedAt      string                 `json:"updated_at"`
}

// userAccessTokenResponse は schemas.UserAccessToken と互換のレスポンス。
type userAccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// ***** 認証ヘルパー *****

// requireCurrentUser はログイン中のユーザーを取得する。未ログインの場合は 401 を返して false を返す。
func (s *Server) requireCurrentUser(w http.ResponseWriter, r *http.Request) (*database.User, bool) {
	user, err := s.auth.AuthenticateRequest(r)
	if err != nil {
		s.writeAuthError(w, err)
		return nil, false
	}
	return user, true
}

// requireCurrentAdminUser はログイン中の管理者ユーザーを取得する。管理者でない場合は 403 を返して false を返す。
func (s *Server) requireCurrentAdminUser(w http.ResponseWriter, r *http.Request) (*database.User, bool) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return nil, false
	}
	if !user.IsAdmin {
		s.logger.Warn("[GetCurrentAdminUser] Don't have permission to access this resource.", "user_id", user.ID)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusForbidden, "Don't have permission to access this resource")
		return nil, false
	}
	return user, true
}

// writeAuthError は認証エラーを 401 レスポンスに変換する。
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	var authError *auth.AuthError
	if errors.As(err, &authError) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, authError.Detail)
		return
	}
	s.logger.Error("authentication failed", "error", err)
	writeError(w, http.StatusInternalServerError, "Internal Server Error")
}

// buildUserResponse は User と関連アカウントを schemas.User 互換のレスポンスに変換する。
func (s *Server) buildUserResponse(ctx context.Context, user *database.User) (*userResponse, error) {
	twitterAccounts, err := database.ListTwitterAccounts(ctx, s.db, user.ID)
	if err != nil {
		return nil, err
	}
	blueskyAccounts, err := database.ListBlueskyAccounts(ctx, s.db, user.ID)
	if err != nil {
		return nil, err
	}
	accountLinks, err := database.ListAccountLinks(ctx, s.db, user.ID)
	if err != nil {
		return nil, err
	}

	response := &userResponse{
		ID:                  user.ID,
		Name:                user.Name,
		IsAdmin:             user.IsAdmin,
		NiconicoUserID:      user.NiconicoUserID,
		NiconicoUserName:    user.NiconicoUserName,
		NiconicoUserPremium: user.NiconicoUserPremium,
		TwitterAccounts:     make([]twitterAccountResponse, 0, len(twitterAccounts)),
		BlueskyAccounts:     make([]blueskyAccountResponse, 0, len(blueskyAccounts)),
		AccountLinks:        make([]accountLinkResponse, 0, len(accountLinks)),
		CreatedAt:           database.FormatJSONTime(user.CreatedAt),
		UpdatedAt:           database.FormatJSONTime(user.UpdatedAt),
	}
	for _, account := range twitterAccounts {
		response.TwitterAccounts = append(response.TwitterAccounts, twitterAccountResponse{
			ID:         account.ID,
			Name:       account.Name,
			ScreenName: account.ScreenName,
			IconURL:    account.IconURL,
			CreatedAt:  database.FormatJSONTime(account.CreatedAt),
			UpdatedAt:  database.FormatJSONTime(account.UpdatedAt),
		})
	}
	for _, account := range blueskyAccounts {
		response.BlueskyAccounts = append(response.BlueskyAccounts, blueskyAccountResponse{
			ID:        account.ID,
			DID:       account.DID,
			Handle:    account.Handle,
			Name:      account.Name,
			IconURL:   account.IconURL,
			CreatedAt: database.FormatJSONTime(account.CreatedAt),
			UpdatedAt: database.FormatJSONTime(account.UpdatedAt),
		})
	}
	for _, link := range accountLinks {
		response.AccountLinks = append(response.AccountLinks, accountLinkResponse{
			ID: link.ID,
			TwitterAccount: twitterAccountResponse{
				ID:         link.TwitterAccount.ID,
				Name:       link.TwitterAccount.Name,
				ScreenName: link.TwitterAccount.ScreenName,
				IconURL:    link.TwitterAccount.IconURL,
				CreatedAt:  database.FormatJSONTime(link.TwitterAccount.CreatedAt),
				UpdatedAt:  database.FormatJSONTime(link.TwitterAccount.UpdatedAt),
			},
			BlueskyAccount: blueskyAccountResponse{
				ID:        link.BlueskyAccount.ID,
				DID:       link.BlueskyAccount.DID,
				Handle:    link.BlueskyAccount.Handle,
				Name:      link.BlueskyAccount.Name,
				IconURL:   link.BlueskyAccount.IconURL,
				CreatedAt: database.FormatJSONTime(link.BlueskyAccount.CreatedAt),
				UpdatedAt: database.FormatJSONTime(link.BlueskyAccount.UpdatedAt),
			},
			CreatedAt: database.FormatJSONTime(link.CreatedAt),
			UpdatedAt: database.FormatJSONTime(link.UpdatedAt),
		})
	}
	return response, nil
}

// ***** ハンドラー *****

// handleUserAccessToken は POST /api/users/token (アクセストークン発行 API、OAuth2 準拠) を処理する。
// server/app/routers/UsersRouter.py の UserAccessTokenAPI 相当。
func (s *Server) handleUserAccessToken(w http.ResponseWriter, r *http.Request) {
	// OAuth2PasswordRequestForm 相当のフォームデータを解析する
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid form data")
		return
	}
	if r.MultipartForm == nil && r.Header.Get("Content-Type") != "" {
		_ = r.ParseMultipartForm(32 << 20)
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if username == "" || password == "" {
		writeError(w, http.StatusUnprocessableEntity, "Invalid form data")
		return
	}

	// ユーザーを取得する
	user, err := database.GetUserByName(r.Context(), s.db, username)
	if errors.Is(err, database.ErrUserNotFound) {
		s.logger.Warn("[UsersRouter][UserAccessTokenAPI] Incorrect username.", "username", username)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "Incorrect username")
		return
	}
	if err != nil {
		s.logger.Error("failed to get user", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// パスワードを検証する
	if !auth.VerifyPassword(password, user.Password) {
		s.logger.Warn("[UsersRouter][UserAccessTokenAPI] Incorrect password.", "username", username)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "Incorrect password")
		return
	}

	// JWT アクセストークンを生成して返す
	accessToken, err := s.auth.GenerateAccessToken(user.ID)
	if err != nil {
		s.logger.Error("failed to generate access token", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, userAccessTokenResponse{
		AccessToken: accessToken,
		TokenType:   "bearer",
	})
}

// handleUsers は GET /api/users (アカウント一覧 API) を処理する。管理者のみアクセスできる。
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}

	users, err := database.ListUsers(r.Context(), s.db)
	if err != nil {
		s.logger.Error("failed to list users", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	response := make([]userResponse, 0, len(users))
	for i := range users {
		user, err := s.buildUserResponse(r.Context(), &users[i])
		if err != nil {
			s.logger.Error("failed to build user response", "error", err)
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		response = append(response, *user)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleUserMe は GET /api/users/me (アカウント情報 API) を処理する。
func (s *Server) handleUserMe(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	response, err := s.buildUserResponse(r.Context(), currentUser)
	if err != nil {
		s.logger.Error("failed to build user response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// getSpecifiedUser は指定されたユーザー名のユーザーを取得する (管理者のみ) 。
// Python 版の GetSpecifiedUser() 相当で、存在しない場合は 422 を返す。
func (s *Server) getSpecifiedUser(w http.ResponseWriter, r *http.Request) (*database.User, bool) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return nil, false
	}
	username := r.PathValue("username")
	user, err := database.GetUserByName(r.Context(), s.db, username)
	if errors.Is(err, database.ErrUserNotFound) {
		s.logger.Error("[GetSpecifiedUser] Specified user was not found.", "username", username)
		writeError(w, http.StatusUnprocessableEntity, "Specified user was not found")
		return nil, false
	}
	if err != nil {
		s.logger.Error("failed to get user", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, false
	}
	return user, true
}

// handleSpecifiedUser は GET /api/users/{username} (指定ユーザーアカウント情報 API) を処理する。管理者のみアクセスできる。
func (s *Server) handleSpecifiedUser(w http.ResponseWriter, r *http.Request) {
	user, ok := s.getSpecifiedUser(w, r)
	if !ok {
		return
	}
	response, err := s.buildUserResponse(r.Context(), user)
	if err != nil {
		s.logger.Error("failed to build user response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleUserMeIcon は GET /api/users/me/icon (アカウントアイコン画像 API) を処理する。
func (s *Server) handleUserMeIcon(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	s.serveUserIcon(w, r, currentUser.ID)
}

// handleSpecifiedUserIcon は GET /api/users/{username}/icon (指定ユーザーのアカウントアイコン画像 API) を処理する。管理者のみアクセスできる。
func (s *Server) handleSpecifiedUserIcon(w http.ResponseWriter, r *http.Request) {
	user, ok := s.getSpecifiedUser(w, r)
	if !ok {
		return
	}
	s.serveUserIcon(w, r, user.ID)
}

// serveUserIcon は指定されたユーザー ID のアイコン画像を返す。
// アイコンが保存されていない場合はデフォルトのアイコン画像を返す。
func (s *Server) serveUserIcon(w http.ResponseWriter, r *http.Request, userID int64) {
	// ブラウザにキャッシュさせないようにヘッダーを設定
	w.Header().Set("Cache-Control", "no-store")

	iconPath := filepath.Join(s.paths.DataDir, "account-icons", fmt.Sprintf("%02d.png", userID))
	if info, err := os.Stat(iconPath); err == nil && !info.IsDir() {
		http.ServeFile(w, r, iconPath)
		return
	}

	// デフォルトのアイコン画像を返す
	defaultIconPath := filepath.Join(s.paths.StaticDir, "account-icons", "default.png")
	if _, err := os.Stat(defaultIconPath); err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	http.ServeFile(w, r, defaultIconPath)
}
