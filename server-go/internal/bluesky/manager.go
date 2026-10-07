package bluesky

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// ManagerOptions は Manager の生成に必要な依存関係。
type ManagerOptions struct {
	// DB は読み取り用のデータベース接続。
	DB *sql.DB
	// WriteDB は書き込み用のデータベース接続 (省略時は DB を使う) 。
	WriteDB *sql.DB
	// Fernet はセッション文字列の暗号化に使う Fernet (省略時は暗号化しない) 。
	Fernet *Fernet
	// HTTPClient は XRPC に使う HTTP クライアント (テストで差し替え可能) 。
	HTTPClient *http.Client
	// Logf はログ出力に使う関数。
	Logf func(format string, values ...any)
}

// Manager は Bluesky アカウントごとの共有 API クライアント (Service) を管理する。
//
// Python 版 BlueskyAPI がアカウント ID ごとにシングルトンインスタンスを保持し、
// SDK クライアントとセッション更新ロックを共有するのと同じ役割を持つ。
type Manager struct {
	db         *sql.DB
	writeDB    *sql.DB
	fernet     *Fernet
	httpClient *http.Client
	logf       func(format string, values ...any)

	// services はアカウント ID ごとの共有 Service 。
	services map[int64]*Service
}

// NewManager は Manager を生成する。
func NewManager(options ManagerOptions) *Manager {
	writeDB := options.WriteDB
	if writeDB == nil {
		writeDB = options.DB
	}
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		db:         options.DB,
		writeDB:    writeDB,
		fernet:     options.Fernet,
		httpClient: options.HTTPClient,
		logf:       logf,
		services:   map[int64]*Service{},
	}
}

// Authenticate は App Password で Bluesky にログインし、保存用の Account を返す。
// App Password は保存せず、ログイン成功後にセッション文字列だけを暗号化して保持する。
// Python 版 BlueskyAPI.authenticate() の移植。
func (m *Manager) Authenticate(ctx context.Context, handle string, appPassword string) (*Account, error) {
	normalizedHandle := NormalizeHandle(handle)
	client := NewClient(ClientOptions{HTTPClient: m.httpClient, Logf: m.logf})
	defer client.Close()

	profile, err := client.Login(ctx, normalizedHandle, appPassword)
	if err != nil {
		return nil, err
	}
	sessionString, err := client.ExportSessionString()
	if err != nil {
		return nil, err
	}

	// profile.handle は DID 解決後の正規 handle なので、ユーザー入力ではなく API 応答値を保存する
	account := &Account{
		DID:           profile.DID,
		Handle:        profile.Handle,
		Name:          profile.Name(),
		IconURL:       profile.AvatarURL(),
		SessionString: "",
	}
	encrypted, err := m.encryptSessionString(sessionString)
	if err != nil {
		return nil, err
	}
	account.SessionString = encrypted
	return account, nil
}

// encryptSessionString は Fernet が設定されていればセッション文字列を暗号化する。
func (m *Manager) encryptSessionString(plainText string) (string, error) {
	if m.fernet == nil {
		return plainText, nil
	}
	return EncryptSessionString(m.fernet, plainText)
}

// decryptSessionString は Fernet が設定されていればセッション文字列を復号する。
func (m *Manager) decryptSessionString(sessionString string) (string, error) {
	if m.fernet == nil {
		return sessionString, nil
	}
	return DecryptSessionString(m.fernet, sessionString)
}

// Service は指定された Bluesky アカウント用の共有 API クライアントを返す。
// Python 版 BlueskyAPI.__new__() と同じく、アカウント ID 単位でインスタンスを再利用する。
func (m *Manager) Service(account *Account) *Service {
	instance := m.services[account.ID]
	if instance == nil {
		instance = &Service{
			manager: m,
			client:  NewClient(ClientOptions{HTTPClient: m.httpClient, Logf: m.logf}),
		}
		m.services[account.ID] = instance
	}
	// DB から取得した新しいアカウント情報へ差し替え、セッション更新時の保存先を最新状態にする
	instance.account = account
	return instance
}

// RemoveInstance は指定された Bluesky アカウント ID の共有 API クライアントを破棄する。
// Python 版 BlueskyAPI.removeInstance() の移植。
func (m *Manager) RemoveInstance(accountID int64) {
	instance := m.services[accountID]
	if instance == nil {
		return
	}
	delete(m.services, accountID)
	instance.client.Close()
}

// removeAllInstancesForTest はテスト用にすべての共有クライアントを破棄する。
func (m *Manager) removeAllInstancesForTest() {
	for accountID := range m.services {
		m.RemoveInstance(accountID)
	}
}

// ErrSessionExpired はセッションの再連携が必要な状態を表す。
var ErrSessionExpired = errors.New("bluesky session expired")
