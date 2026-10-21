// keyframe-cache は旧キャッシュ移行の保守ツール。既存 DB のみ開き、初期化しない。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
	_ "modernc.org/sqlite"
)

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("keyframe-cache", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("database", "", "既存 SQLite DB のパス")
	backupDir := flags.String("backup-dir", "", "移行バックアップ先 (省略時 DB 横)")
	migrate := flags.Bool("migrate", false, "旧キャッシュを移行する")
	restore := flags.String("restore", "", "復元するバックアップ JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || (*migrate == (*restore != "")) || flags.NArg() != 0 {
		return errors.New("specify -database and exactly one of -migrate or -restore")
	}
	absolute, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("database must be an existing regular file")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(absolute)+"?mode=rw")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	store := &metadata.Store{DB: db, WriteDB: db}
	if *restore != "" {
		if err := store.RestoreKeyFrameCache(ctx, *restore); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Cache migration restored.")
		return err
	}
	result, err := store.MigrateKeyFrameCaches(ctx, *backupDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Migrated: %d, repaired: %d, skipped: %d\n", result.Migrated, result.Repaired, result.Skipped)
	for _, issue := range result.Issues {
		fmt.Fprintf(output, "Video %d: %s\n", issue.VideoID, issue.Reason)
	}
	if result.Skipped > 0 {
		return errors.New("some legacy caches were preserved without migration; see diagnostics")
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
