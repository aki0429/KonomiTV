// Package database は SQLite データベースへのアクセスを提供する。
//
// KonomiTV のデータベースは Python 版 (Tortoise ORM) が作成・管理しているため、
// スキーマとデータ形式は server/app/models/ 以下と完全に互換でなければならない。
// 日時は Python の datetime.isoformat(" ") 形式 (例: "2025-09-22 15:47:00.123456+09:00") で
// 保存されている点に注意すること (tortoise/backends/sqlite/executor.py を参照) 。
package database

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // SQLite ドライバー (純 Go 実装)
)

// OpenReadOnly は SQLite データベースを読み取り専用で開く。
// 移行初期段階では Go 側から一切書き込みを行わないため、誤ってデータを破壊しないよう
// mode=ro と query_only の両方で読み取り専用を強制している。
func OpenReadOnly(path string) (*sql.DB, error) {
	// Windows のドライブレター (J:\...) を含むパスでも正しく解釈されるよう、
	// スラッシュに正規化した上で file: URI として渡す
	dsn := fmt.Sprintf(
		"file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(true)",
		filepath.ToSlash(path),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database (%s): %w", path, err)
	}

	// 読み取り専用アクセスなので複数接続を許容するが、SQLite への同時アクセス数は控えめにする
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	// 接続確認
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to connect to database (%s): %w", path, err)
	}
	return db, nil
}
