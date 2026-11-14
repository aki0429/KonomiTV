package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// RecordingMoves は同じサーバーの監視と手動スキャンが共有する移動保留とリンクの対応表。
// ゼロ値で使用でき、別の DB・サーバーの状態をプロセス全体へ漏らさない。
type RecordingMoves struct {
	mu      sync.Mutex
	pending map[string]pendingRecordingMove
	aliases map[string]string
}

type pendingRecordingMove struct {
	sourceID         int64
	sourcePath, hash string
	size             int64
	modified         time.Time
}

func (s *Service) recordingMoves() *RecordingMoves {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Moves == nil {
		s.Moves = &RecordingMoves{}
	}
	return s.Moves
}

func (s *Service) forgetPendingMove(path string) {
	state := s.recordingMoves()
	state.mu.Lock()
	delete(state.pending, path)
	state.mu.Unlock()
}

// rememberRecordedAlias は録画フォルダ内のリンクから参照される実体も移動元として保持する。
func (s *Service) rememberRecordedAlias(original, canonical string) {
	if original == "" || original == canonical {
		return
	}
	state := s.recordingMoves()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.aliases == nil {
		state.aliases = map[string]string{}
	}
	state.aliases[original] = canonical
}

// isInRecordedFolders は設定の元パス・実体パスと、現在も有効なファイルリンクを確認する。
func (s *Service) isInRecordedFolders(path string) bool {
	for _, folder := range s.Config.RecordedFolders {
		absolute, err := filepath.Abs(folder)
		if err == nil && (recordedWithin(path, absolute) || recordedWithin(path, ResolveRecordedPath(absolute))) {
			return true
		}
	}
	state := s.recordingMoves()
	state.mu.Lock()
	aliases := make([]string, 0)
	for alias, canonical := range state.aliases {
		if canonical == path {
			aliases = append(aliases, alias)
		}
	}
	state.mu.Unlock()
	for _, alias := range aliases {
		for _, folder := range s.Config.RecordedFolders {
			absolute, err := filepath.Abs(folder)
			if err == nil && recordedWithin(alias, absolute) && ResolveRecordedPath(alias) == path {
				return true
			}
		}
	}
	return false
}

// canRelinkRecorded は削除通知より先に届いた追加イベントでも実ファイルから移動可否を決める。
func (s *Service) canRelinkRecorded(row VideoSummary, path string) bool {
	if row.Status == "Missing" {
		return true
	}
	if row.FilePath == path {
		return false
	}
	if !s.isInRecordedFolders(row.FilePath) {
		return true
	}
	_, err := os.Stat(row.FilePath)
	return err != nil
}

// findMoveCandidates は本家同様、作成日時とファイル名を使わず、更新日時の誤差を 2 秒まで認める。
func (s *Store) findMoveCandidates(ctx context.Context, size int64, modified time.Time) ([]VideoSummary, error) {
	// SQLite の日時表現は Python と旧 Go で小数桁数が異なるため、文字列比較を避ける。
	rows, err := s.DB.QueryContext(ctx, `SELECT `+videoSummaryColumns+` FROM recorded_videos WHERE status IN ('Recorded','Missing') AND file_size=? ORDER BY id`, size)
	if err != nil {
		return nil, fmt.Errorf("failed to select move candidates: %w", err)
	}
	defer rows.Close()
	var candidates []VideoSummary
	for rows.Next() {
		row, err := scanVideoSummary(rows.Scan)
		if err != nil {
			return nil, err
		}
		if delta := row.FileModifiedAt.Sub(modified); delta >= -2*time.Second && delta <= 2*time.Second {
			candidates = append(candidates, *row)
		}
	}
	return candidates, rows.Err()
}

// recordingHashMatcher は同じ処理中の候補ごとの 3 MiB の再読込を避ける。
// 末尾がゼロ埋めされた TS は、本家 MetadataAnalyzer.matchesFileHash と同じ有効範囲でも照合する。
type recordingHashMatcher struct {
	path      string
	full      string
	effective string
	checked   bool
}

func (m *recordingHashMatcher) matches(ctx context.Context, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m.full == "" {
		info, err := os.Stat(m.path)
		if err != nil {
			return false, err
		}
		if info.Size() < hashChunkSize*hashNumChunks {
			return false, nil
		}
		m.full, err = calculateFileHash(m.path, nil)
		if err != nil {
			return false, err
		}
	}
	if m.full == hash {
		return true, ctx.Err()
	}
	if !m.checked {
		m.checked = true
		file, err := os.Open(m.path)
		if err != nil {
			return false, err
		}
		var syncByte [1]byte
		_, readErr := file.Read(syncByte[:])
		_ = file.Close()
		if readErr == nil && syncByte[0] == 0x47 {
			if _, end, ok := calculateTSFileDuration(m.path, 1024*1024); ok && end >= hashChunkSize*hashNumChunks {
				m.effective, err = calculateFileHash(m.path, &end)
				if err != nil {
					return false, err
				}
			}
		}
	}
	return m.effective != "" && m.effective == hash, ctx.Err()
}

// reconcileRecordedMove は呼び出し側が移動先のパスロックを保持している間に実行する。
// 移動元のロックを追加取得すると、相互の改名でデッドロックするので DB の再読込で保護する。
func (s *Service) reconcileRecordedMove(ctx context.Context, path string, info os.FileInfo, created, modified time.Time) (bool, error) {
	// Windows は FileInfo のファイル ID を遅延取得するので、改名や置換の前に確定させる。
	_ = os.SameFile(info, info)
	state := s.recordingMoves()
	state.mu.Lock()
	pending, wasPending := state.pending[path]
	delete(state.pending, path)
	state.mu.Unlock()
	matcher := recordingHashMatcher{path: path}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		candidates, err := s.Store.findMoveCandidates(ctx, info.Size(), modified)
		if err != nil {
			return false, err
		}
		var relinkable, holdable []VideoSummary
		for _, row := range candidates {
			if s.canRelinkRecorded(row, path) {
				relinkable = append(relinkable, row)
			} else if row.FilePath != path {
				holdable = append(holdable, row)
			}
		}
		reselect := false
		for _, group := range []struct {
			rows   []VideoSummary
			relink bool
		}{{relinkable, true}, {holdable, false}} {
			for _, row := range group.rows {
				matched := !group.relink && wasPending && pending.sourceID == row.ID && pending.sourcePath == row.FilePath && pending.hash == row.FileHash && pending.size == info.Size() && pending.modified.Equal(modified)
				if !matched {
					matched, err = matcher.matches(ctx, row.FileHash)
					if err != nil {
						return false, err
					}
				}
				if !matched {
					continue
				}
				// コピーが照合中に再開された場合、古い内容の照合結果を確定しない。
				current, err := os.Stat(path)
				if err != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
					return false, fmt.Errorf("recording changed during move matching")
				}
				changed, err := s.commitRecordedMove(ctx, row, path, created, modified, group.relink)
				if err != nil {
					return false, err
				}
				if !changed {
					reselect = true
					break
				}
				s.forgetRecordingFile(path)
				if group.relink {
					s.logger().Info("Moved recording record.", "file_path", path, "original_path", row.FilePath, "video_id", row.ID)
					requestBackgroundAnalysis()
				} else {
					state.mu.Lock()
					if state.pending == nil {
						state.pending = map[string]pendingRecordingMove{}
					}
					state.pending[path] = pendingRecordingMove{sourceID: row.ID, sourcePath: row.FilePath, hash: row.FileHash, size: info.Size(), modified: modified}
					state.mu.Unlock()
					if !wasPending {
						s.logger().Info("Waiting for original recording to be removed.", "file_path", path, "original_path", row.FilePath)
					}
				}
				return true, nil
			}
			if reselect {
				break
			}
		}
		if !reselect {
			return false, nil
		}
	}
}

// commitRecordedMove は候補の再読込・コピー途中の行削除・付け替えを一つのトランザクションにする。
func (s *Service) commitRecordedMove(ctx context.Context, source VideoSummary, path string, created, modified time.Time, relink bool) (bool, error) {
	tx, err := s.Store.writeDB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// SQLite の書き込み権を先に確保し、検査後の候補更新や削除を防ぐ。
	if _, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET id=id WHERE 0`); err != nil {
		return false, err
	}
	current, err := scanVideoSummary(tx.QueryRowContext(ctx, `SELECT `+videoSummaryColumns+` FROM recorded_videos WHERE id=?`, source.ID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameRecordedSnapshot(*current, source) || current.FileHash != source.FileHash || s.canRelinkRecorded(*current, path) != relink {
		return false, nil
	}
	target, err := scanVideoSummary(tx.QueryRowContext(ctx, `SELECT `+videoSummaryColumns+` FROM recorded_videos WHERE file_path=? ORDER BY id LIMIT 1`, path).Scan)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if target != nil && target.ID != source.ID && (relink || target.Status == "Recording" || target.Status == "Recorded") {
		// コピー途中にできた親子だけを削除し、移動元の履歴が参照する親 ID は保持する。
		if target.Status != "Recording" && target.Status != "Recorded" && target.Status != "Missing" {
			return false, fmt.Errorf("move target status changed")
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM recorded_videos WHERE id=?`, target.ID); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM recorded_programs WHERE id=? AND NOT EXISTS(SELECT 1 FROM recorded_videos WHERE recorded_program_id=?)`, target.RecordedProgramID, target.RecordedProgramID); err != nil {
			return false, err
		}
	}
	if relink {
		if _, err = tx.ExecContext(ctx, `UPDATE recorded_videos SET status='Recorded',file_path=?,file_created_at=?,file_modified_at=?,missing_since=NULL,updated_at=? WHERE id=?`, path, database.FormatDBTime(created), database.FormatDBTime(modified), database.FormatDBTime(s.now()), source.ID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// retryPendingMoves は削除通知の届かない NAS やフォルダ移動でも元ファイルの消失を検出する。
func (s *Service) retryPendingMoves(ctx context.Context) {
	state := s.recordingMoves()
	state.mu.Lock()
	items := make(map[string]pendingRecordingMove, len(state.pending))
	for path, item := range state.pending {
		items[path] = item
	}
	state.mu.Unlock()
	for path, item := range items {
		if ctx.Err() != nil {
			return
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			s.forgetPendingMove(path)
			continue
		}
		if _, err := os.Stat(item.sourcePath); err == nil && s.isInRecordedFolders(item.sourcePath) {
			continue
		}
		if err := s.ProcessRecordedFile(ctx, path, ProcessOptions{MetadataOnly: true}); err != nil && ctx.Err() == nil {
			s.logger().Error("Failed to retry pending recording move.", "file_path", path, "error", err)
		}
	}
}
