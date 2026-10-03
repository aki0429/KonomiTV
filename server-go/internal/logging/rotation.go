package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// ArchiveRetentionDays はアーカイブログの保持日数 (Python 版 SERVER_LOG_ARCHIVE_RETENTION_DAYS) 。
const ArchiveRetentionDays = 30

// RotatingWriter は JST 基準で日次ローテーションを行うログファイルライター。
// ローテーション後のファイルは logs/archives/<ファイル名>.<YYYYMMDD>.log に移動する。
// 移植元: server/app/utils/LogRotation.py の DailyRotatingFileHandler
type RotatingWriter struct {
	mutex    sync.Mutex
	path     string
	file     *os.File
	fileDate string
}

// NewRotatingWriter は日次ローテーション付きのログファイルライターを生成する。
func NewRotatingWriter(path string) (*RotatingWriter, error) {
	writer := &RotatingWriter{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 起動時に古いアーカイブを整理する
	CleanupArchiveLogs(path)
	return writer, nil
}

// Write はログを書き込む (日付が変わっていれば先にローテーションする) 。
func (w *RotatingWriter) Write(data []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	today := time.Now().In(constants.JST).Format("20060102")
	if w.file == nil {
		if err := w.open(today); err != nil {
			return 0, err
		}
	} else if w.fileDate != today {
		// 日付が変わったのでローテーションする
		w.rotate()
		if err := w.open(today); err != nil {
			return 0, err
		}
	}
	return w.file.Write(data)
}

// Close はログファイルを閉じる。
func (w *RotatingWriter) Close() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// open はログファイルを開く。
func (w *RotatingWriter) open(date string) error {
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open the log file: %w", err)
	}
	w.file = file
	w.fileDate = date
	return nil
}

// rotate は現在のログファイルを archives ディレクトリへ移動する。
func (w *RotatingWriter) rotate() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	archivesDir := filepath.Join(filepath.Dir(w.path), "archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		return
	}
	// ローテーション対象日 (前回の書き込み日) をアーカイブ名に使う
	dateKey := w.fileDate
	if dateKey == "" {
		dateKey = time.Now().In(constants.JST).Format("20060102")
	}
	extension := filepath.Ext(w.path)
	stem := w.path[:len(w.path)-len(extension)]
	archivePath := filepath.Join(archivesDir, fmt.Sprintf("%s.%s%s", filepath.Base(stem), dateKey, extension))
	// 既に同名アーカイブがある場合は上書きしない (重複ローテーションを避ける)
	if _, err := os.Stat(archivePath); err == nil {
		_ = os.Remove(w.path)
		return
	}
	// 同一ログファイルに複数プロセスが接続されている場合、Windows では共有違反が発生することがあるため無視する
	_ = os.Rename(w.path, archivePath)
}

// CleanupArchiveLogs は保持期間を超えたアーカイブログを削除する。
// 移植元: server/app/utils/LogRotation.py の CleanupOldArchiveLogs()
func CleanupArchiveLogs(logPath string) {
	archivesDir := filepath.Join(filepath.Dir(logPath), "archives")
	entries, err := os.ReadDir(archivesDir)
	if err != nil {
		return
	}
	extension := filepath.Ext(logPath)
	stem := filepath.Base(logPath[:len(logPath)-len(extension)])
	threshold := time.Now().In(constants.JST).AddDate(0, 0, -ArchiveRetentionDays)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// <stem>.<YYYYMMDD><ext> の命名規則に一致するファイルのみを対象にする
		if len(name) < len(stem)+len(extension)+9 {
			continue
		}
		if name[:len(stem)+1] != stem+"." || name[len(name)-len(extension):] != extension {
			continue
		}
		dateText := name[len(stem)+1 : len(name)-len(extension)]
		archiveDate, err := time.ParseInLocation("20060102", dateText, constants.JST)
		if err != nil {
			continue
		}
		if archiveDate.Before(threshold) {
			_ = os.Remove(filepath.Join(archivesDir, name))
		}
	}
}
