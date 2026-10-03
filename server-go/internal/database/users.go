package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User は users テーブルのレコード (server/app/models/User.py 互換) 。
type User struct {
	ID                   int64
	Name                 string
	Password             string // bcrypt ハッシュ
	IsAdmin              bool
	ClientSettings       string // JSON 文字列
	NiconicoUserID       *int64
	NiconicoUserName     *string
	NiconicoUserPremium  *bool
	NiconicoAccessToken  *string
	NiconicoRefreshToken *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// TwitterAccount は twitter_accounts テーブルのレコード。
type TwitterAccount struct {
	ID         int64
	UserID     int64
	Name       string
	ScreenName string
	IconURL    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// BlueskyAccount は bluesky_accounts テーブルのレコード。
type BlueskyAccount struct {
	ID        int64
	UserID    int64
	DID       string
	Handle    string
	Name      string
	IconURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AccountLink は account_links テーブルのレコード (紐付け先のアカウントを展開済み) 。
type AccountLink struct {
	ID             int64
	TwitterAccount TwitterAccount
	BlueskyAccount BlueskyAccount
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ErrUserNotFound は指定されたユーザーが存在しない場合のエラー。
var ErrUserNotFound = errors.New("user not found")

// userColumns は users テーブルの SELECT で使うカラム一覧。
const userColumns = `
	id, name, password, is_admin, client_settings,
	niconico_user_id, niconico_user_name, niconico_user_premium,
	niconico_access_token, niconico_refresh_token, created_at, updated_at
`

// scanUser は users テーブルの 1 行を User に読み込む。
func scanUser(scan func(dest ...any) error) (*User, error) {
	var (
		user                 User
		clientSettings       sql.NullString
		niconicoUserID       sql.NullInt64
		niconicoUserName     sql.NullString
		niconicoUserPremium  sql.NullInt64
		niconicoAccessToken  sql.NullString
		niconicoRefreshToken sql.NullString
		createdAt            sql.NullString
		updatedAt            sql.NullString
		isAdmin              int64
	)
	err := scan(
		&user.ID, &user.Name, &user.Password, &isAdmin, &clientSettings,
		&niconicoUserID, &niconicoUserName, &niconicoUserPremium,
		&niconicoAccessToken, &niconicoRefreshToken, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	user.IsAdmin = isAdmin != 0
	user.ClientSettings = clientSettings.String
	if niconicoUserID.Valid {
		user.NiconicoUserID = &niconicoUserID.Int64
	}
	if niconicoUserName.Valid {
		user.NiconicoUserName = &niconicoUserName.String
	}
	if niconicoUserPremium.Valid {
		value := niconicoUserPremium.Int64 != 0
		user.NiconicoUserPremium = &value
	}
	if niconicoAccessToken.Valid {
		user.NiconicoAccessToken = &niconicoAccessToken.String
	}
	if niconicoRefreshToken.Valid {
		user.NiconicoRefreshToken = &niconicoRefreshToken.String
	}
	if user.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if user.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, fmt.Errorf("failed to parse updated_at: %w", err)
	}
	return &user, nil
}

// GetUserByID はユーザー ID からユーザーを取得する。存在しない場合は ErrUserNotFound を返す。
func GetUserByID(ctx context.Context, db *sql.DB, id int64) (*User, error) {
	row := db.QueryRowContext(ctx, `SELECT`+userColumns+`FROM users WHERE id = ?`, id)
	user, err := scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by id (%d): %w", id, err)
	}
	return user, nil
}

// GetUserByName はユーザー名からユーザーを取得する。存在しない場合は ErrUserNotFound を返す。
func GetUserByName(ctx context.Context, db *sql.DB, name string) (*User, error) {
	row := db.QueryRowContext(ctx, `SELECT`+userColumns+`FROM users WHERE name = ?`, name)
	user, err := scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by name (%q): %w", name, err)
	}
	return user, nil
}

// ListUsers はすべてのユーザーを取得する (ID 順) 。
func ListUsers(ctx context.Context, db *sql.DB) ([]User, error) {
	rows, err := db.QueryContext(ctx, `SELECT`+userColumns+`FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user row: %w", err)
		}
		users = append(users, *user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate user rows: %w", err)
	}
	return users, nil
}

// CountUsers はユーザー数を返す。
func CountUsers(ctx context.Context, db *sql.DB) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count users: %w", err)
	}
	return count, nil
}

// ListTwitterAccounts は指定されたユーザーに紐づく Twitter アカウントを取得する。
func ListTwitterAccounts(ctx context.Context, db *sql.DB, userID int64) ([]TwitterAccount, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, name, screen_name, icon_url, created_at, updated_at
		FROM twitter_accounts
		WHERE user_id = ?
		ORDER BY id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list twitter accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accounts := make([]TwitterAccount, 0)
	for rows.Next() {
		var (
			account              TwitterAccount
			createdAt, updatedAt sql.NullString
		)
		if err := rows.Scan(&account.ID, &account.UserID, &account.Name, &account.ScreenName, &account.IconURL, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan twitter account row: %w", err)
		}
		var parseErr error
		if account.CreatedAt, parseErr = ParseDBTime(createdAt.String); parseErr != nil {
			return nil, parseErr
		}
		if account.UpdatedAt, parseErr = ParseDBTime(updatedAt.String); parseErr != nil {
			return nil, parseErr
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// ListBlueskyAccounts は指定されたユーザーに紐づく Bluesky アカウントを取得する。
func ListBlueskyAccounts(ctx context.Context, db *sql.DB, userID int64) ([]BlueskyAccount, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, did, handle, name, icon_url, created_at, updated_at
		FROM bluesky_accounts
		WHERE user_id = ?
		ORDER BY id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list bluesky accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accounts := make([]BlueskyAccount, 0)
	for rows.Next() {
		var (
			account              BlueskyAccount
			createdAt, updatedAt sql.NullString
		)
		if err := rows.Scan(&account.ID, &account.UserID, &account.DID, &account.Handle, &account.Name, &account.IconURL, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan bluesky account row: %w", err)
		}
		var parseErr error
		if account.CreatedAt, parseErr = ParseDBTime(createdAt.String); parseErr != nil {
			return nil, parseErr
		}
		if account.UpdatedAt, parseErr = ParseDBTime(updatedAt.String); parseErr != nil {
			return nil, parseErr
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// ListAccountLinks は指定されたユーザーの Twitter / Bluesky アカウント紐付けを取得する。
func ListAccountLinks(ctx context.Context, db *sql.DB, userID int64) ([]AccountLink, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			account_links.id, account_links.created_at, account_links.updated_at,
			twitter_accounts.id, twitter_accounts.user_id, twitter_accounts.name, twitter_accounts.screen_name, twitter_accounts.icon_url,
			twitter_accounts.created_at, twitter_accounts.updated_at,
			bluesky_accounts.id, bluesky_accounts.user_id, bluesky_accounts.did, bluesky_accounts.handle, bluesky_accounts.name, bluesky_accounts.icon_url,
			bluesky_accounts.created_at, bluesky_accounts.updated_at
		FROM account_links
		JOIN twitter_accounts ON twitter_accounts.id = account_links.twitter_account_id
		JOIN bluesky_accounts ON bluesky_accounts.id = account_links.bluesky_account_id
		WHERE account_links.user_id = ?
		ORDER BY account_links.id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list account links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	links := make([]AccountLink, 0)
	for rows.Next() {
		var (
			link                               AccountLink
			linkCreatedAt, linkUpdatedAt       sql.NullString
			twitterCreatedAt, twitterUpdatedAt sql.NullString
			blueskyCreatedAt, blueskyUpdatedAt sql.NullString
		)
		if err := rows.Scan(
			&link.ID, &linkCreatedAt, &linkUpdatedAt,
			&link.TwitterAccount.ID, &link.TwitterAccount.UserID, &link.TwitterAccount.Name, &link.TwitterAccount.ScreenName, &link.TwitterAccount.IconURL,
			&twitterCreatedAt, &twitterUpdatedAt,
			&link.BlueskyAccount.ID, &link.BlueskyAccount.UserID, &link.BlueskyAccount.DID, &link.BlueskyAccount.Handle, &link.BlueskyAccount.Name, &link.BlueskyAccount.IconURL,
			&blueskyCreatedAt, &blueskyUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan account link row: %w", err)
		}
		for _, target := range []struct {
			value *time.Time
			text  sql.NullString
		}{
			{&link.CreatedAt, linkCreatedAt},
			{&link.UpdatedAt, linkUpdatedAt},
			{&link.TwitterAccount.CreatedAt, twitterCreatedAt},
			{&link.TwitterAccount.UpdatedAt, twitterUpdatedAt},
			{&link.BlueskyAccount.CreatedAt, blueskyCreatedAt},
			{&link.BlueskyAccount.UpdatedAt, blueskyUpdatedAt},
		} {
			parsed, parseErr := ParseDBTime(target.text.String)
			if parseErr != nil {
				return nil, parseErr
			}
			*target.value = parsed
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// GetAccountLinkByID は指定された紐付け ID の Twitter / Bluesky アカウント紐付けを取得する。
func GetAccountLinkByID(ctx context.Context, db *sql.DB, linkID int64) (*AccountLink, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			account_links.id, account_links.created_at, account_links.updated_at,
			twitter_accounts.id, twitter_accounts.user_id, twitter_accounts.name, twitter_accounts.screen_name, twitter_accounts.icon_url,
			twitter_accounts.created_at, twitter_accounts.updated_at,
			bluesky_accounts.id, bluesky_accounts.user_id, bluesky_accounts.did, bluesky_accounts.handle, bluesky_accounts.name, bluesky_accounts.icon_url,
			bluesky_accounts.created_at, bluesky_accounts.updated_at
		FROM account_links
		JOIN twitter_accounts ON twitter_accounts.id = account_links.twitter_account_id
		JOIN bluesky_accounts ON bluesky_accounts.id = account_links.bluesky_account_id
		WHERE account_links.id = ?
	`, linkID)
	if err != nil {
		return nil, fmt.Errorf("failed to get account link: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, ErrUserNotFound
	}
	var (
		link                               AccountLink
		linkCreatedAt, linkUpdatedAt       sql.NullString
		twitterCreatedAt, twitterUpdatedAt sql.NullString
		blueskyCreatedAt, blueskyUpdatedAt sql.NullString
	)
	if err := rows.Scan(
		&link.ID, &linkCreatedAt, &linkUpdatedAt,
		&link.TwitterAccount.ID, &link.TwitterAccount.UserID, &link.TwitterAccount.Name, &link.TwitterAccount.ScreenName, &link.TwitterAccount.IconURL,
		&twitterCreatedAt, &twitterUpdatedAt,
		&link.BlueskyAccount.ID, &link.BlueskyAccount.UserID, &link.BlueskyAccount.DID, &link.BlueskyAccount.Handle, &link.BlueskyAccount.Name, &link.BlueskyAccount.IconURL,
		&blueskyCreatedAt, &blueskyUpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("failed to scan account link row: %w", err)
	}
	for _, target := range []struct {
		value *time.Time
		text  sql.NullString
	}{
		{&link.CreatedAt, linkCreatedAt},
		{&link.UpdatedAt, linkUpdatedAt},
		{&link.TwitterAccount.CreatedAt, twitterCreatedAt},
		{&link.TwitterAccount.UpdatedAt, twitterUpdatedAt},
		{&link.BlueskyAccount.CreatedAt, blueskyCreatedAt},
		{&link.BlueskyAccount.UpdatedAt, blueskyUpdatedAt},
	} {
		parsed, parseErr := ParseDBTime(target.text.String)
		if parseErr != nil {
			return nil, parseErr
		}
		*target.value = parsed
	}
	return &link, nil
}

// GetTwitterAccountByIDAndUserID は指定されたユーザーが所有する Twitter アカウントを取得する。
func GetTwitterAccountByIDAndUserID(ctx context.Context, db *sql.DB, id int64, userID int64) (*TwitterAccount, error) {
	var (
		account              TwitterAccount
		createdAt, updatedAt sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, user_id, name, screen_name, icon_url, created_at, updated_at
		FROM twitter_accounts WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&account.ID, &account.UserID, &account.Name, &account.ScreenName, &account.IconURL, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get twitter account: %w", err)
	}
	if account.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, err
	}
	if account.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, err
	}
	return &account, nil
}

// GetBlueskyAccountByIDAndUserID は指定されたユーザーが所有する Bluesky アカウントを取得する。
func GetBlueskyAccountByIDAndUserID(ctx context.Context, db *sql.DB, id int64, userID int64) (*BlueskyAccount, error) {
	var (
		account              BlueskyAccount
		createdAt, updatedAt sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, user_id, did, handle, name, icon_url, created_at, updated_at
		FROM bluesky_accounts WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&account.ID, &account.UserID, &account.DID, &account.Handle, &account.Name, &account.IconURL, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get bluesky account: %w", err)
	}
	if account.CreatedAt, err = ParseDBTime(createdAt.String); err != nil {
		return nil, err
	}
	if account.UpdatedAt, err = ParseDBTime(updatedAt.String); err != nil {
		return nil, err
	}
	return &account, nil
}

// CreateUser は新しいユーザーを作成し、作成されたユーザーを返す。
func CreateUser(ctx context.Context, db *sql.DB, name string, passwordHash string, isAdmin bool) (*User, error) {
	now := NowForDB()
	result, err := db.ExecContext(ctx, `
		INSERT INTO users (name, password, is_admin, client_settings, created_at, updated_at)
		VALUES (?, ?, ?, '{}', ?, ?)
	`, name, passwordHash, isAdmin, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get the created user id: %w", err)
	}
	return GetUserByID(ctx, db, id)
}

// UpdateUserName はユーザー名を更新する。
func UpdateUserName(ctx context.Context, db *sql.DB, id int64, name string) error {
	if _, err := db.ExecContext(ctx, `UPDATE users SET name = ?, updated_at = ? WHERE id = ?`, name, NowForDB(), id); err != nil {
		return fmt.Errorf("failed to update user name: %w", err)
	}
	return nil
}

// UpdateUserPassword はパスワードハッシュを更新する。
func UpdateUserPassword(ctx context.Context, db *sql.DB, id int64, passwordHash string) error {
	if _, err := db.ExecContext(ctx, `UPDATE users SET password = ?, updated_at = ? WHERE id = ?`, passwordHash, NowForDB(), id); err != nil {
		return fmt.Errorf("failed to update user password: %w", err)
	}
	return nil
}

// UpdateUserIsAdmin は管理者権限を付与/剥奪する。
func UpdateUserIsAdmin(ctx context.Context, db *sql.DB, id int64, isAdmin bool) error {
	if _, err := db.ExecContext(ctx, `UPDATE users SET is_admin = ?, updated_at = ? WHERE id = ?`, isAdmin, NowForDB(), id); err != nil {
		return fmt.Errorf("failed to update user is_admin: %w", err)
	}
	return nil
}

// DeleteUser はユーザーを削除する (関連レコードは外部キーの CASCADE で削除される) 。
func DeleteUser(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	return nil
}

// CountAdmins は管理者ユーザーの数を返す。
func CountAdmins(ctx context.Context, db *sql.DB) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count admins: %w", err)
	}
	return count, nil
}

// CountAdminsExcluding は指定されたユーザーを除く管理者ユーザーの数を返す。
func CountAdminsExcluding(ctx context.Context, db *sql.DB, excludedID int64) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND id != ?`, excludedID).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count admins: %w", err)
	}
	return count, nil
}

// GetOldestUserID は ID が一番若いユーザーの ID を返す (存在しない場合は 0) 。
func GetOldestUserID(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get the oldest user: %w", err)
	}
	return id, nil
}

// DeleteTemporaryTwitterAccounts は連携キャンセル時に残置された仮の Twitter アカウントを削除する。
func DeleteTemporaryTwitterAccounts(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM twitter_accounts WHERE icon_url = 'Temporary'`); err != nil {
		return fmt.Errorf("failed to delete temporary twitter accounts: %w", err)
	}
	return nil
}

// CreateAccountLink は Twitter / Bluesky アカウントの紐付けを作成し、作成された紐付けを返す。
func CreateAccountLink(ctx context.Context, db *sql.DB, userID int64, twitterAccountID int64, blueskyAccountID int64) (*AccountLink, error) {
	now := NowForDB()
	result, err := db.ExecContext(ctx, `
		INSERT INTO account_links (user_id, twitter_account_id, bluesky_account_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, userID, twitterAccountID, blueskyAccountID, now, now)
	if err != nil {
		return nil, err
	}
	linkID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get the created account link id: %w", err)
	}
	return GetAccountLinkByID(ctx, db, linkID)
}

// DeleteAccountLink は指定されたユーザーの紐付けを削除する。削除できた場合は true を返す。
func DeleteAccountLink(ctx context.Context, db *sql.DB, linkID int64, userID int64) (bool, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM account_links WHERE id = ? AND user_id = ?`, linkID, userID)
	if err != nil {
		return false, fmt.Errorf("failed to delete account link: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check the deleted account link: %w", err)
	}
	return affected > 0, nil
}

// AccountLinkExistsByTwitterAccountID は指定された Twitter アカウントの紐付けが存在するかを返す。
func AccountLinkExistsByTwitterAccountID(ctx context.Context, db *sql.DB, twitterAccountID int64) (bool, error) {
	return existsAccountLink(ctx, db, `SELECT COUNT(*) FROM account_links WHERE twitter_account_id = ?`, twitterAccountID)
}

// AccountLinkExistsByBlueskyAccountID は指定された Bluesky アカウントの紐付けが存在するかを返す。
func AccountLinkExistsByBlueskyAccountID(ctx context.Context, db *sql.DB, blueskyAccountID int64) (bool, error) {
	return existsAccountLink(ctx, db, `SELECT COUNT(*) FROM account_links WHERE bluesky_account_id = ?`, blueskyAccountID)
}

func existsAccountLink(ctx context.Context, db *sql.DB, query string, id int64) (bool, error) {
	var count int64
	if err := db.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
		return false, fmt.Errorf("failed to check account link: %w", err)
	}
	return count > 0, nil
}
