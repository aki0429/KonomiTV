package niconico

import (
	"context"
	"database/sql"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// Account は users テーブルのニコニコアカウント連携カラム
// (Python 版 server/app/models/User.py の niconico_* フィールド) 。
//
// database.UpdateNiconicoAccount() は niconico_user_id を更新しないため、
// database パッケージ (他担当) を編集せずに済むよう本パッケージで専用の更新クエリを持つ。
type Account struct {
	// NiconicoUserID はニコニコアカウントのユーザー ID (id_token の sub) 。
	NiconicoUserID *int64
	// NiconicoUserName はニコニコアカウントのユーザー名 (ニックネーム) 。
	NiconicoUserName *string
	// UserPremium はプレミアム会員かどうか。
	UserPremium *bool
	// AccessToken はニコニコ OAuth のアクセストークン。
	AccessToken *string
	// RefreshToken はニコニコ OAuth のリフレッシュトークン。
	RefreshToken *string
}

// SaveAccount は users テーブルのニコニコ連携カラムを更新する。
// 移植元: NiconicoRouter.NiconicoAuthCallbackAPI() の await current_user.save()
// (Tortoise ORM の save() は updated_at も併せて更新する) 。
func SaveAccount(ctx context.Context, db *sql.DB, id int64, account Account) error {
	_, err := db.ExecContext(ctx, `
		UPDATE users SET
			niconico_user_id = ?, niconico_user_name = ?, niconico_user_premium = ?,
			niconico_access_token = ?, niconico_refresh_token = ?,
			updated_at = ?
		WHERE id = ?
	`,
		account.NiconicoUserID, account.NiconicoUserName, account.UserPremium,
		account.AccessToken, account.RefreshToken, database.NowForDB(), id,
	)
	if err != nil {
		return err
	}
	return nil
}

// ClearAccount は users テーブルのニコニコ連携カラムをすべて NULL にする。
// 移植元: NiconicoRouter.NiconicoAccountLogoutAPI() (ニコニコ関連フィールドをすべて None にして save) 。
func ClearAccount(ctx context.Context, db *sql.DB, id int64) error {
	return SaveAccount(ctx, db, id, Account{})
}
