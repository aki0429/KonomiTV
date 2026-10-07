package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/niconico"
)

// niconicoClientSecretLoader は難読化されたニコニコ OAuth のクライアントシークレットを読み込む。
// テストからダミー値に差し替えられるように変数として定義している。
var niconicoClientSecretLoader = interlacedClientSecret

// thirdpartyAuthURLResponse は schemas.ThirdpartyAuthURL 互換のレスポンス。
type thirdpartyAuthURLResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

// validationErrorItem は FastAPI (Pydantic) のバリデーションエラー項目。
type validationErrorItem struct {
	Type  string `json:"type"`
	Loc   []any  `json:"loc"`
	Msg   string `json:"msg"`
	Input any    `json:"input"`
}

// registerNiconicoRoutes はニコニコ関連 API (3 EP) を mux に登録する。
// ルート登録は registerNiconicoRoutes() で行う (server.go の Handler() から呼び出す) 。
// 移植元: server/app/routers/NiconicoRouter.py
func (s *Server) registerNiconicoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/niconico/auth", s.handleNiconicoAuthURL)
	mux.HandleFunc("GET /api/niconico/callback", s.handleNiconicoAuthCallback)
	mux.HandleFunc("DELETE /api/niconico/logout", s.handleNiconicoLogout)
}

// handleNiconicoAuthURL はニコニコ OAuth 認証 URL 発行 API を処理する。
// 移植元: NiconicoRouter.NiconicoAuthURLAPI()
func (s *Server) handleNiconicoAuthURL(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentUser(w, r); !ok {
		return
	}

	// クライアント (フロントエンド) の URL を Origin ヘッダーから取得
	// Origin ヘッダーがリクエストに含まれていない場合はこの API サーバーの URL を使う
	clientURL := ""
	if origin, ok := r.Header["Origin"]; ok && len(origin) > 0 {
		// Origin ヘッダーが存在する場合はその値を使う (空文字でもフォールバックしない)
		clientURL = origin[0]
	} else {
		clientURL = "https://" + r.Host
	}
	clientURL = strings.TrimRight(clientURL, "/") + "/"

	// リクエストの Authorization ヘッダーで渡されたログイン中ユーザーの JWT アクセストークンを取得
	// このトークンをコールバック先の NiconicoAuthCallbackAPI に渡し、ユーザーアカウントと紐づける
	_, userAccessToken := niconico.ParseAuthorizationSchemeParam(r.Header.Get("Authorization"))

	writeJSON(w, http.StatusOK, thirdpartyAuthURLResponse{
		AuthorizationURL: niconico.BuildAuthorizationURL(r.Host, clientURL, userAccessToken),
	})
}

// handleNiconicoAuthCallback はニコニコ OAuth コールバック API を処理する。
// 移植元: NiconicoRouter.NiconicoAuthCallbackAPI()
func (s *Server) handleNiconicoAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// client と user_access_token は必須のクエリパラメータ (FastAPI は欠落時に 422 を返す)
	missing := make([]string, 0, 2)
	if _, ok := query["client"]; !ok {
		missing = append(missing, "client")
	}
	if _, ok := query["user_access_token"]; !ok {
		missing = append(missing, "user_access_token")
	}
	if len(missing) > 0 {
		writeMissingQueryParamError(w, missing)
		return
	}

	client := lastQueryValue(query["client"])
	userAccessToken := lastQueryValue(query["user_access_token"])
	_, codePresent := query["code"]
	code := lastQueryValue(query["code"])
	errorValues, errorPresent := query["error"]
	errorText := lastQueryValue(errorValues)

	// スマホ・タブレット向けのリダイレクト先 URL を生成
	redirectURL := strings.TrimRight(client, "/") + "/settings/jikkyo"

	// "error" パラメーターがセットされている
	// OAuth 認証がユーザーによって拒否されたことを示しているので、401 エラーにする
	if errorPresent {
		s.logger.Warn("[NiconicoAuthCallbackAPI] Authorization was denied.", "error", errorText)
		writeOAuthCallbackResponse(w, http.StatusUnauthorized,
			"Authorization was denied ("+errorText+")", redirectURL)
		return
	}

	// なぜか code がない
	if !codePresent {
		s.logger.Error("[NiconicoAuthCallbackAPI] Authorization code does not exist.")
		writeOAuthCallbackResponse(w, http.StatusInternalServerError,
			"Authorization code does not exist", redirectURL)
		return
	}

	// JWT アクセストークンに基づくユーザーアカウントを取得
	currentUser, err := s.auth.AuthenticateToken(r.Context(), userAccessToken)
	if err != nil {
		// Python 版はここで HTTPException の存在しない属性 (.message) を参照して AttributeError になり、
		// Starlette の ServerErrorMiddleware が 500 Internal Server Error (text/plain) を返す。
		// Go 版では他 API と同じ JSON 形式の 500 を返す (ステータスコードは Python 版と同一) 。
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to authenticate user.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// クライアントシークレットは難読化された状態で同梱されている
	clientSecret, err := niconicoClientSecretLoader(s.paths.StaticDir)
	if err != nil {
		// Python 版は Interlaced() の例外が捕捉されず 500 になる
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to load niconico client secret.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 認証コードを使い、ニコニコ OAuth のアクセストークンとリフレッシュトークンを取得
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", niconicoOAuthClientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", niconico.CallbackURL)

	request, err := http.NewRequestWithContext(
		r.Context(), http.MethodPost, niconicoTokenURL, strings.NewReader(form.Encode()),
	)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to create access token request.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := newNiconicoHTTPClient().Do(request)
	if err != nil {
		// 接続エラー (サーバーメンテナンスやタイムアウトなど)
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to get access token. (Connection Timeout)")
		writeOAuthCallbackResponse(w, http.StatusInternalServerError,
			"Failed to get access token. (Connection Timeout)", redirectURL)
		return
	}
	body, err := readAndCloseBody(response.Body)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to get access token. (Connection Timeout)")
		writeOAuthCallbackResponse(w, http.StatusInternalServerError,
			"Failed to get access token. (Connection Timeout)", redirectURL)
		return
	}

	// ステータスコードが 200 以外
	if response.StatusCode != http.StatusOK {
		detail := fmt.Sprintf("Failed to get access token. (HTTP Error %d)", response.StatusCode)
		s.logger.Error("[NiconicoAuthCallbackAPI] " + detail)
		writeOAuthCallbackResponse(w, http.StatusInternalServerError, detail, redirectURL)
		return
	}

	// レスポンスが JSON でない場合は Python 版も JSONDecodeError で 500 になる
	var tokenResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to parse access token response.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// ニコニコアカウントのユーザー ID を id_token の JWT から取得
	userID, err := niconico.ParseIDTokenSubject(tokenResponse.IDToken)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to parse id_token.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// ニコニコアカウントのユーザー情報を取得
	userRequest, err := http.NewRequestWithContext(
		r.Context(), http.MethodGet, fmt.Sprintf(niconicoUserAPIURLPattern, userID), nil,
	)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to create user request.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	// X-Frontend-Id がないと INVALID_PARAMETER になる
	userRequest.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	userRequest.Header.Set("X-Frontend-Id", "6")

	userResponse, err := newNiconicoHTTPClient().Do(userRequest)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to get user information. (Connection Timeout)")
		writeOAuthCallbackResponse(w, http.StatusInternalServerError,
			"Failed to get user information (Connection Timeout)", redirectURL)
		return
	}
	userBody, err := readAndCloseBody(userResponse.Body)
	if err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to get user information. (Connection Timeout)")
		writeOAuthCallbackResponse(w, http.StatusInternalServerError,
			"Failed to get user information (Connection Timeout)", redirectURL)
		return
	}

	// ステータスコードが 200 以外
	if userResponse.StatusCode != http.StatusOK {
		detail := fmt.Sprintf("Failed to get user information (HTTP Error %d)", userResponse.StatusCode)
		s.logger.Error("[NiconicoAuthCallbackAPI] " + detail)
		writeOAuthCallbackResponse(w, http.StatusInternalServerError, detail, redirectURL)
		return
	}

	// Python 版はレスポンスに data / user / nickname / isPremium が含まれていないと KeyError で 500 になる
	var envelope struct {
		Data *struct {
			User *struct {
				Nickname  *string `json:"nickname"`
				IsPremium *bool   `json:"isPremium"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(userBody, &envelope); err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to parse user information.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if envelope.Data == nil || envelope.Data.User == nil ||
		envelope.Data.User.Nickname == nil || envelope.Data.User.IsPremium == nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to get user information. (KeyError)")
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 取得したアクセストークン・リフレッシュトークン・ユーザー情報をデータベースに保存する
	accessToken := tokenResponse.AccessToken
	refreshToken := tokenResponse.RefreshToken
	userName := *envelope.Data.User.Nickname
	userPremium := *envelope.Data.User.IsPremium
	if err := niconico.SaveAccount(r.Context(), s.writeDB, currentUser.ID, niconico.Account{
		NiconicoUserID:   &userID,
		NiconicoUserName: &userName,
		UserPremium:      &userPremium,
		AccessToken:      &accessToken,
		RefreshToken:     &refreshToken,
	}); err != nil {
		s.logger.Error("[NiconicoAuthCallbackAPI] Failed to save niconico account.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// OAuth 連携が正常に完了したことを伝える
	writeOAuthCallbackResponse(w, http.StatusOK, "Success", redirectURL)
}

// handleNiconicoLogout はニコニコアカウント連携解除 API を処理する。
// 移植元: NiconicoRouter.NiconicoAccountLogoutAPI()
func (s *Server) handleNiconicoLogout(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	// ニコニコ関連のフィールドをすべて None (null) にすることで連携解除とする
	if err := niconico.ClearAccount(r.Context(), s.writeDB, currentUser.ID); err != nil {
		s.logger.Error("[NiconicoAccountLogoutAPI] Failed to clear niconico account.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeOAuthCallbackResponse は OAuth 連携コールバック用の HTML レスポンスを書き出す。
// 移植元: server/app/utils/OAuthCallbackResponse.py (HTMLResponse の Content-Type は
// "text/html; charset=utf-8"、ボディは UTF-8 の HTML) 。
func writeOAuthCallbackResponse(w http.ResponseWriter, statusCode int, detail string, redirectTo string) {
	body := niconico.RenderOAuthCallbackResponse(statusCode, detail, redirectTo)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	_, _ = io.WriteString(w, body)
}

// writeMissingQueryParamError は FastAPI が必須クエリパラメータ欠落時に返す 422 レスポンスを再現する。
func writeMissingQueryParamError(w http.ResponseWriter, names []string) {
	items := make([]validationErrorItem, 0, len(names))
	for _, name := range names {
		items = append(items, validationErrorItem{
			Type:  "missing",
			Loc:   []any{"query", name},
			Msg:   "Field required",
			Input: nil,
		})
	}
	writeJSON(w, http.StatusUnprocessableEntity, struct {
		Detail []validationErrorItem `json:"detail"`
	}{Detail: items})
}

// lastQueryValue はクエリパラメータの値を 1 つ返す。
// FastAPI (Starlette の QueryParams) は同名パラメータが複数ある場合に最後の値を採用するため、
// それに合わせて末尾の値を返す (値が空の配列の場合は空文字を返す) 。
func lastQueryValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// readAndCloseBody はレスポンスボディをすべて読み込んで閉じる。
func readAndCloseBody(body io.ReadCloser) ([]byte, error) {
	defer func() { _ = body.Close() }()
	return io.ReadAll(body)
}
