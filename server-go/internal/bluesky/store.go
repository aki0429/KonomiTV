package bluesky

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// ErrAccountNotFound は指定された Bluesky アカウントが存在しない場合のエラー。
var ErrAccountNotFound = errors.New("bluesky account not found")

// Account は bluesky_accounts テーブルのレコード (session_string を含む) 。
//
// database.BlueskyAccount には session_string が含まれないため、
// 本パッケージで独自に定義している (database パッケージは他担当のため編集しない) 。
type Account struct {
	ID      int64
	UserID  int64
	DID     string
	Handle  string
	Name    string
	IconURL string
	// SessionString は暗号化済み (enc: 接頭辞付き) の atproto セッション文字列。
	SessionString string
}

// accountColumns は bluesky_accounts テーブルの SELECT で使うカラム一覧。
const accountColumns = `id, user_id, did, handle, name, icon_url, session_string`

// scanAccount は bluesky_accounts テーブルの 1 行を Account に読み込む。
func scanAccount(scan func(dest ...any) error) (*Account, error) {
	var account Account
	if err := scan(
		&account.ID, &account.UserID, &account.DID, &account.Handle,
		&account.Name, &account.IconURL, &account.SessionString,
	); err != nil {
		return nil, err
	}
	return &account, nil
}

// GetAccountByUserAndHandle は指定されたユーザーが所有する handle のアカウントを取得する。
// Python 版 GetCurrentBlueskyAccount() の filter(user_id=..., handle=...) に相当する。
func GetAccountByUserAndHandle(ctx context.Context, db *sql.DB, userID int64, handle string) (*Account, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM bluesky_accounts WHERE user_id = ? AND handle = ?`, userID, handle)
	account, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bluesky account: %w", err)
	}
	return account, nil
}

// GetAccountByID は指定された ID のアカウントを取得する。
func GetAccountByID(ctx context.Context, db *sql.DB, id int64) (*Account, error) {
	row := db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM bluesky_accounts WHERE id = ?`, id)
	account, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bluesky account: %w", err)
	}
	return account, nil
}

// GetAccountByUserAndDID は指定されたユーザーが所有する DID のアカウントを取得する。
func GetAccountByUserAndDID(ctx context.Context, db *sql.DB, userID int64, did string) (*Account, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM bluesky_accounts WHERE user_id = ? AND did = ?`, userID, did)
	account, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bluesky account: %w", err)
	}
	return account, nil
}

// InsertAccount は新しい Bluesky アカウントを作成し、作成されたアカウントを返す。
func InsertAccount(ctx context.Context, db *sql.DB, account *Account) (*Account, error) {
	now := database.NowForDB()
	result, err := db.ExecContext(ctx, `
		INSERT INTO bluesky_accounts (user_id, did, handle, name, icon_url, session_string, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, account.UserID, account.DID, account.Handle, account.Name, account.IconURL, account.SessionString, now, now)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get the created bluesky account id: %w", err)
	}
	return GetAccountByID(ctx, db, id)
}

// UpdateAccountProfile は handle / name / icon_url / session_string を更新する。
// Python 版の再連携時の save() に相当する。
func UpdateAccountProfile(ctx context.Context, db *sql.DB, account *Account) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE bluesky_accounts SET handle = ?, name = ?, icon_url = ?, session_string = ?, updated_at = ?
		WHERE id = ?
	`, account.Handle, account.Name, account.IconURL, account.SessionString, database.NowForDB(), account.ID); err != nil {
		return fmt.Errorf("failed to update bluesky account: %w", err)
	}
	return nil
}

// UpdateAccountSessionString は暗号化済みセッション文字列だけを更新する。
// Python 版のセッション更新通知 (on_session_change) に相当する。
func UpdateAccountSessionString(ctx context.Context, db *sql.DB, id int64, sessionString string) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE bluesky_accounts SET session_string = ?, updated_at = ? WHERE id = ?
	`, sessionString, database.NowForDB(), id); err != nil {
		return fmt.Errorf("failed to update bluesky session string: %w", err)
	}
	return nil
}

// DeleteAccount は指定された ID の Bluesky アカウントを削除する。
func DeleteAccount(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM bluesky_accounts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete bluesky account: %w", err)
	}
	return nil
}
