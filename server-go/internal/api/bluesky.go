package api

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/bluesky"
)

// blueskyAuthRequest は schemas.BlueskyAuthRequest と互換のリクエスト。
type blueskyAuthRequest struct {
	Handle      *string `json:"handle"`
	AppPassword *string `json:"app_password"`
}

// blueskyManagerFactory は Server に紐づく Bluesky Manager を生成する。
// テストで差し替えられるように変数として定義している。
var blueskyManagerFactory = newBlueskyManager

// blueskyManagerState はプロセス全体で共有する Bluesky Manager を保持する。
// Python 版 BlueskyAPI がクラス変数でアカウント単位の共有クライアントを保持するのと同じ役割。
var blueskyManagerState struct {
	sync.Mutex
	manager *bluesky.Manager
}

// newBlueskyManager は Server の DB と jwt_secret.dat から Bluesky Manager を生成する。
func newBlueskyManager(s *Server) (*bluesky.Manager, error) {
	// セッション文字列の暗号化鍵は jwt_secret.dat から導出する (Python 版 constants.py と同じ)
	secret, err := auth.LoadOrCreateSecret(filepath.Join(s.paths.DataDir, "jwt_secret.dat"))
	if err != nil {
		return nil, err
	}
	return bluesky.NewManager(bluesky.ManagerOptions{
		DB:      s.db,
		WriteDB: s.writeDB,
		Fernet:  bluesky.NewFernetWithJWTSecret(secret),
		Logf: func(format string, values ...any) {
			s.logger.Info("bluesky: "+format, values...)
		},
	}), nil
}

// blueskyManager はプロセス全体で共有する Bluesky Manager を返す。
func (s *Server) blueskyManager() (*bluesky.Manager, error) {
	blueskyManagerState.Lock()
	defer blueskyManagerState.Unlock()
	if blueskyManagerState.manager == nil {
		manager, err := blueskyManagerFactory(s)
		if err != nil {
			return nil, err
		}
		blueskyManagerState.manager = manager
	}
	return blueskyManagerState.manager, nil
}

// getCurrentBlueskyAccount は現在ログイン中のユーザーに紐づく Bluesky アカウントを取得する。
// Python 版 GetCurrentBlueskyAccount() の移植。
func (s *Server) getCurrentBlueskyAccount(w http.ResponseWriter, r *http.Request) (*bluesky.Account, bool) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return nil, false
	}

	// handle は変更可能だが、API パスでは UI 上の識別子として使う
	// 実際の更新・削除操作ではログイン中ユーザーに紐づくレコードだけに絞り込む
	handle := bluesky.NormalizeHandle(r.PathValue("handle"))
	account, err := bluesky.GetAccountByUserAndHandle(r.Context(), s.db, currentUser.ID, handle)
	if errors.Is(err, bluesky.ErrAccountNotFound) {
		s.logger.Error("[BlueskyRouter][GetCurrentBlueskyAccount] BlueskyAccount associated with handle does not exist.", "handle", handle)
		writeError(w, http.StatusUnprocessableEntity, "BlueskyAccount associated with handle does not exist")
		return nil, false
	}
	if err != nil {
		s.logger.Error("failed to get bluesky account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, false
	}
	return account, true
}

// registerBlueskyRoutes は Bluesky 関連 API (9 EP) を mux に登録する。
//
// Python 版 BlueskyRouter.py の全エンドポイントに対応する。
func (s *Server) registerBlueskyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/bluesky/auth", s.handleBlueskyAuth)
	mux.HandleFunc("DELETE /api/bluesky/accounts/{handle}", s.handleBlueskyAccountDelete)
	mux.HandleFunc("POST /api/bluesky/accounts/{handle}/posts", s.handleBlueskyPost)
	// リポスト / いいねの実行・取り消しは post_id (AT URI) にスラッシュが含まれるため、
	// パス全体を {rest...} で受け取ってから末尾の操作名で振り分ける
	mux.HandleFunc("PUT /api/bluesky/accounts/{handle}/posts/{rest...}", s.handleBlueskyPostAction)
	mux.HandleFunc("DELETE /api/bluesky/accounts/{handle}/posts/{rest...}", s.handleBlueskyPostAction)
	mux.HandleFunc("GET /api/bluesky/accounts/{handle}/timeline", s.handleBlueskyTimeline)
	mux.HandleFunc("GET /api/bluesky/accounts/{handle}/search", s.handleBlueskySearch)
}

// handleBlueskyAuth は POST /api/bluesky/auth (Bluesky 認証 API) を処理する。
func (s *Server) handleBlueskyAuth(w http.ResponseWriter, r *http.Request) {
	currentUser, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}

	var request blueskyAuthRequest
	if !decodeJSONBody(r, &request) || request.Handle == nil || request.AppPassword == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 認証処理では App Password を使ってセッションを作成し、保存用のアカウント情報へ変換する
	account, err := manager.Authenticate(r.Context(), *request.Handle, *request.AppPassword)
	if err != nil {
		s.logger.Error("[BlueskyRouter][BlueskyAuthAPI] Failed to login to Bluesky.", "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Failed to login to Bluesky")
		return
	}
	account.UserID = currentUser.ID

	// 同じ DID のアカウントが既にある場合は、セッションとプロフィール情報を更新する
	existingAccount, err := bluesky.GetAccountByUserAndDID(r.Context(), s.db, currentUser.ID, account.DID)
	if err != nil && !errors.Is(err, bluesky.ErrAccountNotFound) {
		s.logger.Error("failed to get bluesky account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if existingAccount != nil {
		account.ID = existingAccount.ID
		s.updateBlueskyAccount(w, r, manager, account, "Updated existing Bluesky account")
		return
	}

	if _, err := bluesky.InsertAccount(r.Context(), s.writeDB, account); err != nil {
		// 同じ DID の連携が同時に走った場合、事前の存在確認だけでは一意制約違反を避けられない
		// 競合相手が作成したレコードを更新して、連打や複数タブでも連携済み状態に収束させる
		conflicting, getErr := bluesky.GetAccountByUserAndDID(r.Context(), s.db, currentUser.ID, account.DID)
		if getErr != nil {
			s.logger.Error("[BlueskyRouter][BlueskyAuthAPI] Failed to save Bluesky account due to an unexpected integrity error.", "error", err)
			writeError(w, http.StatusUnprocessableEntity, "Failed to link Bluesky account")
			return
		}
		account.ID = conflicting.ID
		s.updateBlueskyAccount(w, r, manager, account, "Updated existing Bluesky account after conflict")
		return
	}
	s.logger.Info("[BlueskyRouter][BlueskyAuthAPI] Created new Bluesky account.", "handle", account.Handle)
	w.WriteHeader(http.StatusNoContent)
}

// updateBlueskyAccount は既存の Bluesky アカウントを更新し、共有クライアントを破棄する。
func (s *Server) updateBlueskyAccount(w http.ResponseWriter, r *http.Request, manager *bluesky.Manager, account *bluesky.Account, message string) {
	if err := bluesky.UpdateAccountProfile(r.Context(), s.writeDB, account); err != nil {
		s.logger.Error("failed to update bluesky account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	// 再連携前のセッション情報を保持している共有クライアントを破棄し、次回 API 呼び出し時に作り直す
	manager.RemoveInstance(account.ID)
	s.logger.Info("[BlueskyRouter][BlueskyAuthAPI] "+message+".", "id", account.ID, "handle", account.Handle)
	w.WriteHeader(http.StatusNoContent)
}

// handleBlueskyAccountDelete は DELETE /api/bluesky/accounts/{handle} (Bluesky アカウント連携解除 API) を処理する。
func (s *Server) handleBlueskyAccountDelete(w http.ResponseWriter, r *http.Request) {
	account, ok := s.getCurrentBlueskyAccount(w, r)
	if !ok {
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// 連携解除後に認証済みクライアントが残らないよう、DB レコードを削除する前に HTTP 接続を閉じる
	manager.RemoveInstance(account.ID)
	// BlueskyAccount に紐づく AccountLink は外部キーの cascade で削除される
	if err := bluesky.DeleteAccount(r.Context(), s.writeDB, account.ID); err != nil {
		s.logger.Error("failed to delete bluesky account", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBlueskyPost は POST /api/bluesky/accounts/{handle}/posts (Bluesky 投稿送信 API) を処理する。
func (s *Server) handleBlueskyPost(w http.ResponseWriter, r *http.Request) {
	account, ok := s.getCurrentBlueskyAccount(w, r)
	if !ok {
		return
	}

	// multipart/form-data を解析する (アップロード上限は 32MB)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}

	text := r.FormValue("post")
	replyRootURI := optionalFormValue(r, "reply_root_uri")
	replyRootCID := optionalFormValue(r, "reply_root_cid")
	replyParentURI := optionalFormValue(r, "reply_parent_uri")
	replyParentCID := optionalFormValue(r, "reply_parent_cid")
	replyFields := []*string{replyRootURI, replyRootCID, replyParentURI, replyParentCID}

	var replyTo *bluesky.ReplyReference
	// Bluesky の StrongRef はルートと親ポストの uri / cid が揃って初めて意味を持つ
	// 一部欠けた状態はクライアント側状態の破損や手動リクエストのミスなので、単独投稿へ丸めず明示的に拒否する
	if replyRootURI != nil || replyRootCID != nil || replyParentURI != nil || replyParentCID != nil {
		for _, field := range replyFields {
			if field == nil {
				writeError(w, http.StatusUnprocessableEntity,
					"reply_root_uri / reply_root_cid / reply_parent_uri / reply_parent_cid must all be specified together.")
				return
			}
		}
		replyTo = &bluesky.ReplyReference{
			RootURI:   *replyRootURI,
			RootCID:   *replyRootCID,
			ParentURI: *replyParentURI,
			ParentCID: *replyParentCID,
		}
	}

	images, ok := s.readBlueskyImages(w, r)
	if !ok {
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	result, err := manager.Service(account).CreatePost(r.Context(), text, images, replyTo)
	if err != nil {
		s.logger.Error("failed to create bluesky post", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// readBlueskyImages は multipart フォームから添付画像を読み出す。
func (s *Server) readBlueskyImages(w http.ResponseWriter, r *http.Request) ([]bluesky.ImageInput, bool) {
	if r.MultipartForm == nil {
		return nil, true
	}
	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		return nil, true
	}
	images := make([]bluesky.ImageInput, 0, len(files))
	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
			return nil, false
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
			return nil, false
		}
		images = append(images, bluesky.ImageInput{Data: data, ContentType: fileHeader.Header.Get("Content-Type")})
	}
	return images, true
}

// handleBlueskyPostAction は PUT / DELETE .../posts/{rest...} (リポスト・いいねの実行/取り消し API) を処理する。
//
// Python 版は /posts/{post_id:path}/repost のように path パラメーターの後ろに固定セグメントを置いているが、
// Go の ServeMux は {name...} ワイルドカードをパターン末尾にしか置けないため、
// パス全体を {rest...} で受け取り、末尾の操作名で振り分ける。
func (s *Server) handleBlueskyPostAction(w http.ResponseWriter, r *http.Request) {
	account, ok := s.getCurrentBlueskyAccount(w, r)
	if !ok {
		return
	}

	rest := r.PathValue("rest")
	action := ""
	postID := ""
	if index := strings.LastIndexByte(rest, '/'); index >= 0 {
		action = rest[index+1:]
		postID = rest[:index]
	}
	if postID == "" || (action != "repost" && action != "like") {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request path")
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	service := manager.Service(account)

	var result any
	switch {
	case r.Method == http.MethodPut && action == "repost":
		result, err = service.CreateRepost(r.Context(), postID)
	case r.Method == http.MethodDelete && action == "repost":
		result, err = service.DeleteRepost(r.Context(), postID)
	case r.Method == http.MethodPut && action == "like":
		result, err = service.FavoritePost(r.Context(), postID)
	default:
		result, err = service.UnfavoritePost(r.Context(), postID)
	}
	if err != nil {
		s.logger.Error("failed to process bluesky post action", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleBlueskyTimeline は GET /api/bluesky/accounts/{handle}/timeline (Bluesky ホームタイムライン取得 API) を処理する。
func (s *Server) handleBlueskyTimeline(w http.ResponseWriter, r *http.Request) {
	account, ok := s.getCurrentBlueskyAccount(w, r)
	if !ok {
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	result, err := manager.Service(account).HomeLatestTimeline(r.Context(), optionalQueryValue(r, "cursor_id"))
	if err != nil {
		s.logger.Error("failed to fetch bluesky timeline", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleBlueskySearch は GET /api/bluesky/accounts/{handle}/search (Bluesky 投稿検索 API) を処理する。
func (s *Server) handleBlueskySearch(w http.ResponseWriter, r *http.Request) {
	account, ok := s.getCurrentBlueskyAccount(w, r)
	if !ok {
		return
	}

	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusUnprocessableEntity, "query is required")
		return
	}

	manager, err := s.blueskyManager()
	if err != nil {
		s.logger.Error("failed to initialize bluesky manager", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	result, err := manager.Service(account).SearchTimeline(r.Context(), query, optionalQueryValue(r, "cursor_id"))
	if err != nil {
		s.logger.Error("failed to search bluesky posts", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// optionalFormValue は空文字列または未指定のフォーム値を nil として返す。
func optionalFormValue(r *http.Request, name string) *string {
	value := strings.TrimSpace(r.FormValue(name))
	if value == "" {
		return nil
	}
	return &value
}

// optionalQueryValue は空文字列または未指定のクエリパラメーターを nil として返す。
func optionalQueryValue(r *http.Request, name string) *string {
	value := r.URL.Query().Get(name)
	if value == "" {
		return nil
	}
	return &value
}
