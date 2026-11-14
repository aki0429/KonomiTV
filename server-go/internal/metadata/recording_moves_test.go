package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordedMoveHashMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/recording_move_hash_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var reference struct {
		Cases []struct {
			Name, Recipe, Hash string
			Matches            bool
			EffectiveEnd       *int64 `json:"effective_end"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	for _, tc := range reference.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.ts")
			switch tc.Recipe {
			case "pattern":
				writeMoveFile(t, path)
			case "short":
				if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				content := make([]byte, 0, 24000*188+4*1024*1024)
				for i := range 24000 {
					packet := tsinfoPCRPacket(0x200, uint64(i)*900, 0)
					if tc.Recipe == "padded-without-pcr" {
						packet[5] = 0
					}
					content = append(content, packet...)
				}
				content = append(content, make([]byte, 4*1024*1024)...)
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := (&recordingHashMatcher{path: path}).matches(context.Background(), tc.Hash)
			if err != nil || got != tc.Matches {
				t.Fatalf("Go=%v upstream=%v err=%v", got, tc.Matches, err)
			}
			if tc.EffectiveEnd != nil {
				_, end, ok := calculateTSFileDuration(path, 1024*1024)
				if !ok || end != *tc.EffectiveEnd {
					t.Fatalf("effective end Go=%d upstream=%d ok=%v", end, *tc.EffectiveEnd, ok)
				}
			}
		})
	}
}

type unexpectedMoveAnalyzer struct{ calls atomic.Int32 }

func (a *unexpectedMoveAnalyzer) Analyze(context.Context, string) (*RecordedProgram, error) {
	a.calls.Add(1)
	return nil, errors.New("unexpected heavy metadata analysis")
}

func writeMoveFile(t *testing.T, path string) {
	t.Helper()
	data := make([]byte, 4*1024*1024)
	for i := range data {
		data[i] = byte(i*17 + i/1024 + 1)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := testFixedNow.Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func saveMoveVideo(t *testing.T, store *Store, path, status string) VideoSummary {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := calculateFileHash(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	program := buildTestProgram(path, 3600)
	program.Video.Status = status
	program.Video.FileSize = info.Size()
	program.Video.FileHash = hash
	program.Video.FileCreatedAt = fileCreationTime(info).Round(time.Microsecond)
	program.Video.FileModifiedAt = info.ModTime().Round(time.Microsecond)
	if _, err := store.SaveRecordedMetadata(context.Background(), program, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := store.FindVideoSummariesByPaths(context.Background(), []string{path})
	if err != nil || len(rows) != 1 {
		t.Fatalf("saved video: %v %v", rows, err)
	}
	return rows[0]
}

func assertMoveIdentity(t *testing.T, store *Store, source VideoSummary, path string) {
	t.Helper()
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("videos: %v %v", rows, err)
	}
	row := rows[0]
	if row.ID != source.ID || row.RecordedProgramID != source.RecordedProgramID || row.FileHash != source.FileHash || row.FilePath != path || row.Status != "Recorded" {
		t.Fatalf("lost source identity: before=%+v after=%+v", source, row)
	}
	var parents int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_programs`).Scan(&parents); err != nil || parents != 1 {
		t.Fatalf("orphan parent: %d %v", parents, err)
	}
}

func TestRecordedMoveRenamePreservesProgramAndDerivedData(t *testing.T) {
	for _, status := range []string{"Recorded", "Missing"} {
		t.Run(status, func(t *testing.T) {
			store := openTestDB(t)
			folder := t.TempDir()
			old, path := filepath.Join(folder, "old.ts"), filepath.Join(folder, "renamed.ts")
			writeMoveFile(t, old)
			source := saveMoveVideo(t, store, old, status)
			// 実際に参照する親 ID と、ユーザー編集・解析済みデータが維持されることを検査する。
			if _, err := store.DB.Exec(`CREATE TABLE watched_history (program_id INTEGER REFERENCES recorded_programs(id) ON DELETE CASCADE, position REAL); INSERT INTO watched_history VALUES (?,123.5); UPDATE recorded_programs SET title='edited title',series_title='edited series' WHERE id=?; UPDATE recorded_videos SET cm_sections='[{"start_time":1,"end_time":2}]',segment_map='[{"marker":"preserve"}]',thumbnail_info='{"marker":"preserve"}',missing_since='2026-10-06 00:00:00+09:00' WHERE id=?`, source.RecordedProgramID, source.RecordedProgramID, source.ID); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(old, path); err != nil {
				t.Fatal(err)
			}
			analyzer := &unexpectedMoveAnalyzer{}
			service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: analyzer, Logger: testLogger(t)}
			if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{MetadataOnly: true}); err != nil {
				t.Fatal(err)
			}
			assertMoveIdentity(t, store, source, path)
			var title, series, cm, segments, thumbnail string
			var position float64
			var cleared bool
			if err := store.DB.QueryRow(`SELECT p.title,p.series_title,v.cm_sections,v.segment_map,v.thumbnail_info,v.missing_since IS NULL,h.position FROM recorded_videos v JOIN recorded_programs p ON p.id=v.recorded_program_id JOIN watched_history h ON h.program_id=p.id WHERE v.id=?`, source.ID).Scan(&title, &series, &cm, &segments, &thumbnail, &cleared, &position); err != nil {
				t.Fatal(err)
			}
			if title != "edited title" || series != "edited series" || position != 123.5 || !cleared || segments != `[{"marker":"preserve"}]` || thumbnail != `{"marker":"preserve"}` || cm != `[{"start_time":1,"end_time":2}]` || analyzer.calls.Load() != 0 {
				t.Fatalf("modified preserved data: %q %q %v %v %q %q %q calls=%d", title, series, position, cleared, segments, thumbnail, cm, analyzer.calls.Load())
			}
		})
	}
}

func TestRecordedMoveHoldsCompleteCopyAndRemovesPartialRow(t *testing.T) {
	for _, partial := range []string{"", "Recording", "Recorded"} {
		t.Run("partial="+partial, func(t *testing.T) {
			store := openTestDB(t)
			folder := t.TempDir()
			old, path := filepath.Join(folder, "original.ts"), filepath.Join(folder, "copy.ts")
			writeMoveFile(t, old)
			source := saveMoveVideo(t, store, old, "Recorded")
			writeMoveFile(t, path)
			if partial != "" {
				target := saveMoveVideo(t, store, path, partial)
				if _, err := store.DB.Exec(`UPDATE recorded_videos SET file_size=1,file_hash='partial-copy' WHERE id=?`, target.ID); err != nil {
					t.Fatal(err)
				}
			}
			analyzer := &unexpectedMoveAnalyzer{}
			moves := &RecordingMoves{}
			service := &Service{Store: store, Moves: moves, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: analyzer, Logger: testLogger(t)}
			for range 2 {
				if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{MetadataOnly: true}); err != nil {
					t.Fatal(err)
				}
				assertMoveIdentity(t, store, source, old)
			}
			moves.mu.Lock()
			pending := moves.pending[path]
			moves.mu.Unlock()
			if pending.sourceID != source.ID || analyzer.calls.Load() != 0 {
				t.Fatalf("copy was analyzed: %+v calls=%d", pending, analyzer.calls.Load())
			}
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			// API が新しい Service / Store を作っても、同じサーバーの保留を継続できる。
			other := &Service{Store: &Store{DB: store.DB, Now: store.Now}, Moves: moves, Config: service.Config, Analyzer: analyzer, Logger: testLogger(t)}
			other.retryPendingMoves(context.Background())
			assertMoveIdentity(t, store, source, path)
			moves.mu.Lock()
			count := len(moves.pending)
			moves.mu.Unlock()
			if count != 0 || analyzer.calls.Load() != 0 {
				t.Fatalf("pending=%d analysis=%d", count, analyzer.calls.Load())
			}
		})
	}
}

func TestRecordedMoveRelinksOverPartialTarget(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	old, path := filepath.Join(folder, "old.ts"), filepath.Join(folder, "new.ts")
	writeMoveFile(t, old)
	source := saveMoveVideo(t, store, old, "Recorded")
	writeMoveFile(t, path)
	target := saveMoveVideo(t, store, path, "Recorded")
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET file_size=2,file_hash='partial' WHERE id=?`, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: &unexpectedMoveAnalyzer{}, Logger: testLogger(t)}
	if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{}); err != nil {
		t.Fatal(err)
	}
	assertMoveIdentity(t, store, source, path)
	// 途中の行を参照していた古い CM 検出は、完成後の元レコードへ保存してはいけない。
	if err := store.updateVideoCMSections(context.Background(), VideoRow{ID: target.ID, FilePath: path, FileHash: source.FileHash}, []CMSection{{StartTime: 1, EndTime: 2}}); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := store.DB.QueryRow(`SELECT cm_sections IS NULL FROM recorded_videos WHERE id=?`, source.ID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("stale CM analysis overwrote source: %v %v", isNull, err)
	}
}

func TestRecordedMoveCandidateBoundsAndContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delta  time.Duration
		change string
		match  bool
	}{{"minus two seconds", -2 * time.Second, "", true}, {"plus two seconds", 2 * time.Second, "", true}, {"outside window", 2*time.Second + time.Microsecond, "", false}, {"different hash", 0, "hash", false}, {"different size", 0, "size", false}} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestDB(t)
			oldRoot, newRoot := t.TempDir(), t.TempDir()
			old, path := filepath.Join(oldRoot, "old.ts"), filepath.Join(newRoot, "new.ts")
			writeMoveFile(t, old)
			source := saveMoveVideo(t, store, old, "Recorded")
			writeMoveFile(t, path)
			if err := os.Chtimes(path, testFixedNow, source.FileModifiedAt.Add(tc.delta)); err != nil {
				t.Fatal(err)
			}
			if tc.change == "hash" {
				if _, err := store.DB.Exec(`UPDATE recorded_videos SET file_hash='different' WHERE id=?`, source.ID); err != nil {
					t.Fatal(err)
				}
			} else if tc.change == "size" {
				if err := os.Truncate(path, 3*1024*1024); err != nil {
					t.Fatal(err)
				}
			}
			analyzer := &unexpectedMoveAnalyzer{}
			service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{newRoot}}, Analyzer: analyzer, Logger: testLogger(t)}
			if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{MetadataOnly: true}); err != nil {
				t.Fatal(err)
			}
			if tc.match {
				// 接続したままの旧ドライブが設定対象外なら、元ファイルがあっても付け替える。
				assertMoveIdentity(t, store, source, path)
				if analyzer.calls.Load() != 0 {
					t.Fatal("performed heavy analysis for a move")
				}
			} else if analyzer.calls.Load() != 1 {
				t.Fatal("a different recording was treated as a move")
			}
		})
	}
}

func TestRecordedMoveRejectsStaleSource(t *testing.T) {
	for _, change := range []string{"path", "hash", "status", "size", "delete", "revived"} {
		t.Run(change, func(t *testing.T) {
			store := openTestDB(t)
			folder := t.TempDir()
			old, path := filepath.Join(folder, "old.ts"), filepath.Join(folder, "new.ts")
			writeMoveFile(t, old)
			source := saveMoveVideo(t, store, old, "Recorded")
			if err := os.Rename(old, path); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "path":
				_, err = store.DB.Exec(`UPDATE recorded_videos SET file_path='other' WHERE id=?`, source.ID)
			case "hash":
				_, err = store.DB.Exec(`UPDATE recorded_videos SET file_hash='other' WHERE id=?`, source.ID)
			case "status":
				_, err = store.DB.Exec(`UPDATE recorded_videos SET status='Recording' WHERE id=?`, source.ID)
			case "size":
				_, err = store.DB.Exec(`UPDATE recorded_videos SET file_size=1 WHERE id=?`, source.ID)
			case "delete":
				_, err = store.DB.Exec(`DELETE FROM recorded_videos WHERE id=?`, source.ID)
			case "revived":
				writeMoveFile(t, old)
			}
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}}
			if committed, err := service.commitRecordedMove(context.Background(), source, path, testFixedNow, testFixedNow, true); err != nil || committed {
				t.Fatalf("stale source committed: %v %v", committed, err)
			}
			if change != "revived" {
				if _, err := store.SaveRecordedMetadata(context.Background(), buildTestProgram(old, 3600), source.RecordedProgramID, source); !errors.Is(err, ErrRecordingChanged) {
					t.Fatalf("stale metadata save was accepted: %v", err)
				}
			}
		})
	}
}

func TestRecordedMoveConcurrentTargetsPreserveSourceOnce(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	folder := t.TempDir()
	old := filepath.Join(folder, "old.ts")
	writeMoveFile(t, old)
	source := saveMoveVideo(t, store, old, "Recorded")
	paths := []string{filepath.Join(folder, "a.ts"), filepath.Join(folder, "b.ts")}
	for _, path := range paths {
		writeMoveFile(t, path)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	analyzer := &unexpectedMoveAnalyzer{}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: analyzer, Logger: testLogger(t)}
	var workers sync.WaitGroup
	errors := make(chan error, len(paths))
	for _, path := range paths {
		workers.Go(func() {
			errors <- service.ProcessRecordedFile(context.Background(), path, ProcessOptions{MetadataOnly: true})
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != source.ID || rows[0].RecordedProgramID != source.RecordedProgramID || analyzer.calls.Load() != 0 {
		t.Fatalf("concurrent targets: %+v err=%v calls=%d", rows, err, analyzer.calls.Load())
	}
}

func TestRecordedMovePendingTargetDeletionAndIsolation(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	old, path := filepath.Join(folder, "old.ts"), filepath.Join(folder, "copy.ts")
	writeMoveFile(t, old)
	writeMoveFile(t, path)
	saveMoveVideo(t, store, old, "Recorded")
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Logger: testLogger(t)}
	if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{}); err != nil {
		t.Fatal(err)
	}
	other := &Service{Store: openTestDB(t)}
	if service.recordingMoves() == other.recordingMoves() {
		t.Fatal("move state leaked into another database")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	service.retryPendingMoves(context.Background())
	state := service.recordingMoves()
	state.mu.Lock()
	count := len(state.pending)
	state.mu.Unlock()
	if count != 0 {
		t.Fatal("deleted copy is still pending")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&recordingHashMatcher{path: old}).matches(ctx, "hash"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestRecordedMoveMonitorRetriesSharedPending(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	folder := t.TempDir()
	old, path := filepath.Join(folder, "original.ts"), filepath.Join(folder, "target.ts")
	writeMoveFile(t, old)
	writeMoveFile(t, path)
	source := saveMoveVideo(t, store, old, "Recorded")
	moves := &RecordingMoves{}
	analyzer := &unexpectedMoveAnalyzer{}
	manual := &Service{Store: store, Moves: moves, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: analyzer, Logger: testLogger(t)}
	if err := manual.ProcessRecordedFile(context.Background(), path, ProcessOptions{}); err != nil {
		t.Fatal(err)
	}
	monitor := &Service{Store: store, Moves: moves, Config: manual.Config, Analyzer: analyzer, Logger: testLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); monitor.RunRecordingMonitor(ctx, 20*time.Millisecond) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("monitor did not stop")
		}
	}()
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := store.FindVideoSummariesByPaths(ctx, []string{path})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].ID == source.ID && rows[0].Status == "Recorded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("monitor did not relink: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertMoveIdentity(t, store, source, path)
	if analyzer.calls.Load() != 0 {
		t.Fatal("monitor performed heavy analysis for pending move")
	}
}

type interruptedMoveAnalyzer struct {
	started chan struct{}
	resume  chan struct{}
	program *RecordedProgram
	err     error
}

func (a *interruptedMoveAnalyzer) Analyze(context.Context, string) (*RecordedProgram, error) {
	close(a.started)
	<-a.resume
	return a.program, a.err
}

func TestRecordedMoveDuringOldPathAnalysis(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fails], func(t *testing.T) {
			store := openTestDB(t)
			store.DB.SetMaxOpenConns(1)
			folder := t.TempDir()
			old, path := filepath.Join(folder, "old.ts"), filepath.Join(folder, "new.ts")
			writeMoveFile(t, old)
			source := saveMoveVideo(t, store, old, "Recorded")
			analyzer := &interruptedMoveAnalyzer{started: make(chan struct{}), resume: make(chan struct{}), program: buildTestProgram(old, 3600)}
			if fails {
				analyzer.err = errors.New("old path disappeared")
			}
			service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: analyzer, Logger: testLogger(t)}
			done := make(chan error, 1)
			go func() {
				done <- service.ProcessRecordedFile(context.Background(), old, ProcessOptions{ForceUpdate: true, MetadataOnly: true})
			}()
			<-analyzer.started
			if err := os.Rename(old, path); err != nil {
				t.Fatal(err)
			}
			if err := service.ProcessRecordedFile(context.Background(), path, ProcessOptions{MetadataOnly: true}); err != nil {
				t.Fatal(err)
			}
			close(analyzer.resume)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertMoveIdentity(t, store, source, path)
		})
	}
}
