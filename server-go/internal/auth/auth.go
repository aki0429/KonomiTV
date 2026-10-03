// Package auth は KonomiTV の認証 (JWT アクセストークン・匿名 ID・パスワード検証) を提供する。
// server/app/routers/UsersRouter.py と互換のトークン形式・Cookie を扱う。
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

const (
	// tokenIssuer は JWT の発行者 (iss) 。
	tokenIssuer = "KonomiTV Server"
	// accessTokenType は JWT の種類 (typ) 。
	accessTokenType = "AccessToken"
	// accessTokenValidDuration はアクセストークンの有効期限 (180日間) 。
	accessTokenValidDuration = 180 * 24 * time.Hour
	// anonymousIDCookieName は未ログインユーザーを識別するための匿名 ID の Cookie 名。
	anonymousIDCookieName = "KonomiTV-AnonymousID"
	// accessTokenCookieName はアクセストークンを保持する Cookie 名 (クライアントは通常 Authorization ヘッダーを使う) 。
	accessTokenCookieName = "KonomiTV-AccessToken"
	// anonymousIDCookieMaxAge は匿名 ID の Cookie の有効期間 (秒) 。アクセスの度に延長される。
	anonymousIDCookieMaxAge = 60 * 60 * 24 * 30 * 6
)

// anonymousIDPattern は匿名 ID として受け付ける UUID v4 の形式。
// クライアントから送られてきた任意の値をそのまま識別子にしないための対策。
var anonymousIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// AuthError は認証エラー (HTTP 401) を表す。Detail は API レスポンスの detail にそのまま使う。
type AuthError struct {
	Detail string
}

func (e *AuthError) Error() string {
	return e.Detail
}

// accessTokenClaims は JWT アクセストークンのペイロード。
type accessTokenClaims struct {
	// Type はトークンの種類 (typ) 。常に 'AccessToken'。
	Type string `json:"typ"`
	jwt.RegisteredClaims
}

// Manager は認証処理のインスタンス。
type Manager struct {
	// jwtSecret は JWT の署名/検証に使うシークレットキー (server/data/jwt_secret.dat の内容) 。
	jwtSecret []byte
	// db はユーザー情報の取得に使うデータベース。
	db *sql.DB
	// debug は開発環境かどうか (Cookie の SameSite 属性に影響する) 。
	debug bool
	// logger はロガー。
	logger *slog.Logger
}

// New は jwt_secret.dat からシークレットキーを読み込み (存在しない場合は Python 版と同様に生成し) 、Manager を生成する。
func New(paths constants.Paths, db *sql.DB, cfg *config.Config, logger *slog.Logger) (*Manager, error) {
	secret, err := LoadOrCreateSecret(filepath.Join(paths.DataDir, "jwt_secret.dat"))
	if err != nil {
		return nil, err
	}
	return NewWithSecret(secret, db, cfg.General.Debug, logger), nil
}

// NewWithSecret は指定されたシークレットキーで Manager を生成する (テスト用) 。
func NewWithSecret(secret string, db *sql.DB, debug bool, logger *slog.Logger) *Manager {
	return &Manager{
		jwtSecret: []byte(secret),
		db:        db,
		debug:     debug,
		logger:    logger,
	}
}

// LoadOrCreateSecret は jwt_secret.dat を読み込む。存在しない場合は Python 版と同じく
// 32 バイトの乱数の16進数表現 (64文字) を生成して保存する。
func LoadOrCreateSecret(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		secret := strings.TrimSpace(string(data))
		if secret == "" {
			return "", fmt.Errorf("jwt_secret.dat is empty: %s", path)
		}
		return secret, nil
	}

	// 32 バイトの乱数を生成し、16進数文字列 (64文字) として保存する
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate jwt secret: %w", err)
	}
	secret := hex.EncodeToString(randomBytes)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", fmt.Errorf("failed to write jwt_secret.dat: %w", err)
	}
	return secret, nil
}

// GenerateAccessToken はユーザー ID を含む JWT アクセストークンを生成する (有効期限は 180 日間) 。
func (m *Manager) GenerateAccessToken(userID int64) (string, error) {
	now := time.Now().In(constants.JST)
	claims := accessTokenClaims{
		Type: accessTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenValidDuration)),
			ID:        uuid.NewString(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.jwtSecret)
}

// AuthenticateToken はアクセストークンを検証し、紐づくユーザーを返す。
// トークンが不正な場合は *AuthError を返す (Detail は Python 版と同じ文言) 。
func (m *Manager) AuthenticateToken(ctx context.Context, token string) (*database.User, error) {
	parsed, err := jwt.ParseWithClaims(
		token,
		&accessTokenClaims{},
		func(_ *jwt.Token) (any, error) { return m.jwtSecret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(tokenIssuer),
	)
	if err != nil {
		m.logger.Warn("[GetCurrentUser] Access token is invalid", slog.Any("error", err))
		return nil, &AuthError{Detail: "Access token is invalid"}
	}
	claims, ok := parsed.Claims.(*accessTokenClaims)
	if !ok || !parsed.Valid {
		m.logger.Warn("[GetCurrentUser] Access token is invalid")
		return nil, &AuthError{Detail: "Access token is invalid"}
	}

	// typ が AccessToken でない (JWT トークンが不正)
	if claims.Type != accessTokenType {
		m.logger.Warn("[GetCurrentUser] Access token type is invalid.")
		return nil, &AuthError{Detail: "Access token type is invalid"}
	}

	// Subject が JWT ペイロードに含まれていない (JWT トークンが不正)
	if claims.Subject == "" {
		m.logger.Warn("[GetCurrentUser] Access token data is invalid.")
		return nil, &AuthError{Detail: "Access token data is invalid"}
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		m.logger.Warn("[GetCurrentUser] Access token data is invalid.", slog.String("sub", claims.Subject))
		return nil, &AuthError{Detail: "Access token data is invalid"}
	}

	// トークンに刻まれたユーザー ID に紐づくユーザー情報を取得する
	user, err := database.GetUserByID(ctx, m.db, userID)
	if errors.Is(err, database.ErrUserNotFound) {
		m.logger.Warn("[GetCurrentUser] User associated with access token does not exist.", slog.Int64("user_id", userID))
		return nil, &AuthError{Detail: "User associated with access token does not exist"}
	}
	if err != nil {
		return nil, err
	}
	return user, nil
}

// AuthenticateRequest はリクエストの Authorization ヘッダー (Bearer) から現在のユーザーを取得する。
// Python 版の Depends(OAuth2PasswordBearer(...)) 相当で、Cookie は参照しない点に注意。
// ヘッダーがない場合は 401 'Not authenticated' を返す。
func (m *Manager) AuthenticateRequest(r *http.Request) (*database.User, error) {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		if spaceIndex := strings.Index(authorization, " "); spaceIndex > 0 {
			scheme := strings.ToLower(authorization[:spaceIndex])
			parameter := strings.TrimSpace(authorization[spaceIndex+1:])
			if scheme == "bearer" && parameter != "" {
				return m.AuthenticateToken(r.Context(), parameter)
			}
		}
	}
	return nil, &AuthError{Detail: "Not authenticated"}
}

// GetOptionalCurrentUser はリクエストに付与されたアクセストークンからログイン中のユーザーを取得する。
// 認証が必須ではない API で使う。トークンがない・不正な場合は nil を返し、エラーにはしない。
func (m *Manager) GetOptionalCurrentUser(r *http.Request) *database.User {
	accessToken := extractAccessToken(r)
	if accessToken == "" {
		return nil
	}
	user, err := m.AuthenticateToken(r.Context(), accessToken)
	if err != nil {
		return nil
	}
	return user
}

// extractAccessToken は Authorization ヘッダー (Bearer) 、なければ Cookie からアクセストークンを取り出す。
func extractAccessToken(r *http.Request) string {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		// FastAPI の get_authorization_scheme_param() 相当 (最初の空白で scheme と param に分割する)
		if spaceIndex := strings.Index(authorization, " "); spaceIndex > 0 {
			scheme := strings.ToLower(authorization[:spaceIndex])
			parameter := strings.TrimSpace(authorization[spaceIndex+1:])
			if scheme == "bearer" && parameter != "" {
				return parameter
			}
		}
		// Bearer 以外のスキームでは Cookie を参照する
	}
	if cookie, err := r.Cookie(accessTokenCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// ResolveUserKey は API の呼び出し元を識別するキーを返す。
//   - ログイン中のユーザーがいる場合は 'user:{ユーザー ID}'
//   - 未ログインの場合は Cookie の匿名 ID を使い 'anon:{匿名 ID}'
//
// 匿名 ID の Cookie がない (または不正な) 場合は新規に発行し、アクセスの度に有効期限を6ヶ月に延長する。
func (m *Manager) ResolveUserKey(w http.ResponseWriter, r *http.Request) string {
	if currentUser := m.GetOptionalCurrentUser(r); currentUser != nil {
		return fmt.Sprintf("user:%d", currentUser.ID)
	}

	anonymousID := ""
	if cookie, err := r.Cookie(anonymousIDCookieName); err == nil {
		anonymousID = strings.ToLower(cookie.Value)
	}
	if !anonymousIDPattern.MatchString(anonymousID) {
		anonymousID = uuid.NewString()
	}

	sameSite := http.SameSiteLaxMode
	if m.debug {
		// 開発環境ではクライアント (:7001) と API (:7000) が別オリジンになるため SameSite=None にする
		sameSite = http.SameSiteNoneMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     anonymousIDCookieName,
		Value:    anonymousID,
		Path:     "/",
		MaxAge:   anonymousIDCookieMaxAge,
		Expires:  time.Now().Add(anonymousIDCookieMaxAge * time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: sameSite,
	})
	return "anon:" + anonymousID
}

// VerifyPassword は平文パスワードと bcrypt ハッシュ (passlib 互換) を照合する。
func VerifyPassword(password string, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// HashPassword はパスワードを bcrypt (コスト12、passlib のデフォルトと同等) でハッシュ化する。
func HashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}
