package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CaptureFolder はキャプチャフォルダ (capture_folders テーブル) のレコード。
type CaptureFolder struct {
	ID        int64
	Name      string
	SortOrder int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrCaptureFolderNotFound は指定されたキャプチャフォルダが見つからなかったことを表すエラー。
var ErrCaptureFolderNotFound = errors.New("capture folder not found")

// ListCaptureFolders はログインユーザーのキャプチャフォルダを sort_order 順で取得する。
// 戻り値のマップはフォルダ ID からキャプチャ数への対応表。
func ListCaptureFolders(ctx context.Context, db *sql.DB, userID int64) ([]CaptureFolder, map[int64]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, sort_order, created_at, updated_at
		FROM capture_folders
		WHERE user_id = ?
		ORDER BY sort_order ASC, id ASC
	`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	folders := []CaptureFolder{}
	ids := []int64{}
	for rows.Next() {
		var folder CaptureFolder
		var createdAt, updatedAt string
		if err := rows.Scan(&folder.ID, &folder.Name, &folder.SortOrder, &createdAt, &updatedAt); err != nil {
			return nil, nil, err
		}
		if parsed, err := ParseDBTime(createdAt); err == nil {
			folder.CreatedAt = parsed
		}
		if parsed, err := ParseDBTime(updatedAt); err == nil {
			folder.UpdatedAt = parsed
		}
		folders = append(folders, folder)
		ids = append(ids, folder.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// 各フォルダに紐付けられたキャプチャ数を集計する
	counts := map[int64]int{}
	if len(ids) > 0 {
		countRows, err := db.QueryContext(ctx, `
			SELECT folder_id, COUNT(*)
			FROM capture_bookmarks
			WHERE folder_id IN (`+placeholders(len(ids))+`)
			GROUP BY folder_id
		`, toAnySlice(ids)...)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = countRows.Close() }()
		for countRows.Next() {
			var folderID int64
			var count int
			if err := countRows.Scan(&folderID, &count); err != nil {
				return nil, nil, err
			}
			counts[folderID] = count
		}
		if err := countRows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return folders, counts, nil
}

// GetCaptureFolder はログインユーザーのキャプチャフォルダを取得する (他ユーザーのフォルダは取得できない) 。
func GetCaptureFolder(ctx context.Context, db *sql.DB, folderID int64, userID int64) (*CaptureFolder, error) {
	var folder CaptureFolder
	var createdAt, updatedAt string
	err := db.QueryRowContext(ctx, `
		SELECT id, name, sort_order, created_at, updated_at
		FROM capture_folders
		WHERE id = ? AND user_id = ?
	`, folderID, userID).Scan(&folder.ID, &folder.Name, &folder.SortOrder, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCaptureFolderNotFound
	}
	if err != nil {
		return nil, err
	}
	if parsed, err := ParseDBTime(createdAt); err == nil {
		folder.CreatedAt = parsed
	}
	if parsed, err := ParseDBTime(updatedAt); err == nil {
		folder.UpdatedAt = parsed
	}
	return &folder, nil
}

// CreateCaptureFolder はキャプチャフォルダを作成する。
// sort_order は既存フォルダの最大値 + 1 (既存フォルダがない場合は 0) になる。
func CreateCaptureFolder(ctx context.Context, db *sql.DB, userID int64, name string) (*CaptureFolder, error) {
	var maxSortOrder sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT MAX(sort_order) FROM capture_folders WHERE user_id = ?
	`, userID).Scan(&maxSortOrder); err != nil {
		return nil, err
	}
	sortOrder := 0
	if maxSortOrder.Valid {
		sortOrder = int(maxSortOrder.Int64) + 1
	}

	now := FormatDBTime(time.Now())
	result, err := db.ExecContext(ctx, `
		INSERT INTO capture_folders (name, sort_order, created_at, updated_at, user_id)
		VALUES (?, ?, ?, ?, ?)
	`, name, sortOrder, now, now, userID)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return GetCaptureFolder(ctx, db, id, userID)
}

// UpdateCaptureFolder はキャプチャフォルダの名前・表示順序を更新する。
// name または sortOrder に nil を渡したフィールドは更新しない。
func UpdateCaptureFolder(
	ctx context.Context,
	db *sql.DB,
	folderID int64,
	userID int64,
	name *string,
	sortOrder *int,
) (*CaptureFolder, error) {
	if _, err := GetCaptureFolder(ctx, db, folderID, userID); err != nil {
		return nil, err
	}
	if name != nil {
		if _, err := db.ExecContext(ctx, `UPDATE capture_folders SET name = ? WHERE id = ?`, *name, folderID); err != nil {
			return nil, err
		}
	}
	if sortOrder != nil {
		if _, err := db.ExecContext(ctx, `UPDATE capture_folders SET sort_order = ? WHERE id = ?`, *sortOrder, folderID); err != nil {
			return nil, err
		}
	}
	// Tortoise の auto_now と同じく、更新時に updated_at を更新する
	if _, err := db.ExecContext(ctx, `UPDATE capture_folders SET updated_at = ? WHERE id = ?`, FormatDBTime(time.Now()), folderID); err != nil {
		return nil, err
	}
	return GetCaptureFolder(ctx, db, folderID, userID)
}

// DeleteCaptureFolder はキャプチャフォルダを削除する。
// 紐付けられた CaptureBookmark のレコードも削除する (外部キーの CASCADE に依存しない) 。
func DeleteCaptureFolder(ctx context.Context, db *sql.DB, folderID int64, userID int64) error {
	if _, err := GetCaptureFolder(ctx, db, folderID, userID); err != nil {
		return err
	}
	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `DELETE FROM capture_bookmarks WHERE folder_id = ?`, folderID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM capture_folders WHERE id = ?`, folderID); err != nil {
		return err
	}
	return transaction.Commit()
}

// ListCaptureBookmarks はフォルダに紐付けられたキャプチャのファイル名を取得する (追加日時順) 。
func ListCaptureBookmarks(ctx context.Context, db *sql.DB, folderID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT filename FROM capture_bookmarks WHERE folder_id = ? ORDER BY id ASC
	`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	filenames := []string{}
	for rows.Next() {
		var filename string
		if err := rows.Scan(&filename); err != nil {
			return nil, err
		}
		filenames = append(filenames, filename)
	}
	return filenames, rows.Err()
}

// CountCaptureBookmarks はフォルダに紐付けられたキャプチャの件数を取得する。
func CountCaptureBookmarks(ctx context.Context, db *sql.DB, folderID int64) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capture_bookmarks WHERE folder_id = ?`, folderID).Scan(&count)
	return count, err
}

// AddCaptureBookmark はフォルダにキャプチャを紐付ける。
// 既に同じファイル名が紐付けられている場合は何もしない (unique_together 制約の事前チェック) 。
func AddCaptureBookmark(ctx context.Context, db *sql.DB, folderID int64, filename string) error {
	var existing int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM capture_bookmarks WHERE folder_id = ? AND filename = ?
	`, folderID, filename).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO capture_bookmarks (filename, created_at, folder_id) VALUES (?, ?, ?)
	`, filename, FormatDBTime(time.Now()), folderID)
	return err
}

// RemoveCaptureBookmarks は指定されたファイル名の紐付けを削除する。
func RemoveCaptureBookmarks(ctx context.Context, db *sql.DB, folderID int64, filenames []string) error {
	if len(filenames) == 0 {
		return nil
	}
	arguments := []any{folderID}
	for _, filename := range filenames {
		arguments = append(arguments, filename)
	}
	_, err := db.ExecContext(ctx, `
		DELETE FROM capture_bookmarks
		WHERE folder_id = ? AND filename IN (`+placeholders(len(filenames))+`)
	`, arguments...)
	return err
}

// placeholders は SQL のプレースホルダー ("?, ?, ?") を生成する。
func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	result := "?"
	for index := 1; index < count; index++ {
		result += ", ?"
	}
	return result
}

// toAnySlice は int64 のスライスを any のスライスに変換する。
func toAnySlice(values []int64) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
