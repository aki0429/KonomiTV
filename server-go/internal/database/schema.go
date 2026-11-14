package database

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// currentSchema は本家の空 DB に全マイグレーションを適用して生成したスキーマ。
//
//go:embed schema.sql
var currentSchema string

//go:embed schema_columns.json
var currentSchemaColumns []byte

// EnsureSchema は Python を実行せずに新規 DB を作成し、既存 DB には本家の追加項目だけを補う。
// 既存テーブル・データ・独自項目は削除せず、失敗時には追加処理全体をロールバックする。
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statements := strings.Split(currentSchema, ";")
	// 既存 DB の索引が追加列を参照できるよう、テーブル・列・索引の順に適用する。
	for _, statement := range statements {
		if strings.Contains(statement, "CREATE TABLE") {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("initialize upstream table: %w", err)
			}
		}
	}
	for _, column := range []struct{ table, name, definition string }{
		{"recorded_videos", "thumbnail_info", "JSON"},
		{"recorded_videos", "segment_map", "JSON NOT NULL DEFAULT '[]'"},
		{"recorded_videos", "has_video_stream_changes", "INT NOT NULL DEFAULT 0"},
		{"recorded_videos", "missing_since", "TIMESTAMP"},
		{"twitter_accounts", "cookie_browser_info", "JSON"},
	} {
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info("`+column.table+`")`)
		if err != nil {
			return fmt.Errorf("inspect upstream columns: %w", err)
		}
		found := false
		for rows.Next() {
			var id, notNull, primaryKey int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read upstream column: %w", err)
			}
			found = found || name == column.name
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("inspect upstream columns: %w", err)
		}
		if !found {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE "`+column.table+`" ADD COLUMN "`+column.name+`" `+column.definition); err != nil {
				return fmt.Errorf("add upstream column %s.%s: %w", column.table, column.name, err)
			}
		}
	}
	// SQLite は二重引用符の未知列を文字列リテラルとして受理する場合があるため、
	// 索引作成の成否ではなく、本家の全列が存在することを明示的に確認する。
	var required map[string][]string
	if err := json.Unmarshal(currentSchemaColumns, &required); err != nil {
		return fmt.Errorf("decode embedded schema columns: %w", err)
	}
	for table, columns := range required {
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		found := map[string]bool{}
		for rows.Next() {
			var id, notNull, primaryKey int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				return fmt.Errorf("inspect column in %s: %w", table, err)
			}
			found[name] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		for _, column := range columns {
			if !found[column] {
				return fmt.Errorf("incompatible database: missing column %s.%s", table, column)
			}
		}
	}
	for _, statement := range statements {
		if strings.Contains(statement, "CREATE INDEX") || strings.Contains(statement, "CREATE UNIQUE INDEX") {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("initialize upstream index: %w", err)
			}
		}
	}
	// 本家 migration 12 と同じく、既に指定されている revision は維持する。
	if _, err := tx.ExecContext(ctx, `UPDATE recorded_videos
		SET thumbnail_info = json_set(thumbnail_info, '$.representative.revision', 1)
		WHERE thumbnail_info IS NOT NULL AND json_type(thumbnail_info, '$.representative.revision') IS NULL`); err != nil {
		return fmt.Errorf("initialize thumbnail revisions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}
