package metadata

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

func seedRecordedFiles(t *testing.T, store *Store, folder string, count int) []string {
	t.Helper()
	paths := []string{}
	for i := 0; i < count; i++ {
		path := filepath.Join(folder, fmt.Sprintf("recorded-%d.ts", i))
		if err := os.WriteFile(path, []byte("synthetic transport stream"), 0600); err != nil {
			t.Fatal(err)
		}
		old := testFixedNow.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		program := buildTestProgram(path, 3600)
		program.Video.FileCreatedAt = fileCreationTime(stat).Round(time.Microsecond)
		program.Video.FileModifiedAt = stat.ModTime().Round(time.Microsecond)
		program.Video.FileSize = stat.Size()
		program.Video.FileHash = fmt.Sprintf("%032d", i+10)
		if _, err := store.SaveRecordedMetadata(context.Background(), program, 0); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func TestMissingRetentionAndRootGuards(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 6)
	now := testFixedNow
	store.Now = func() time.Time { return now }
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: &fakeAnalyzer{err: fmt.Errorf("unchanged files must not be reanalyzed")}, Logger: testLogger(t)}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var since *string
	if err := store.DB.QueryRow(`SELECT status,missing_since FROM recorded_videos WHERE file_path=?`, paths[0]).Scan(&status, &since); err != nil || status != "Missing" || since == nil {
		t.Fatalf("missing status %s %v", status, err)
	}
	if visible, err := database.CountRecordedPrograms(ctx, store.DB, nil, "", nil); err != nil || visible != 5 {
		t.Fatalf("visible recordings=%d err=%v", visible, err)
	}
	now = now.Add(8 * 24 * time.Hour)
	unreadable := filepath.Join(folder, "unavailable")
	service.Config.RecordedFolders = append(service.Config.RecordedFolders, unreadable)
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos`).Scan(&count); err != nil || count != 6 {
		t.Fatal("incomplete root deleted retained recording")
	}
	service.Config.RecordedFolders = []string{folder}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("expired recording retained: %d %v", count, err)
	}
	// 二割を超える新たな欠落があると、別の保存先も含めて期限削除を見送る。
	for _, path := range paths[1:3] {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos WHERE status='Missing'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("missing recordings not marked")
	}
	// 同じ場所に戻った変更のないファイルは、解析せずに復帰し、保持期限を解除する。
	rows, err := store.FindVideoSummariesByPaths(ctx, []string{paths[3]})
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	snapshot := rows[0]
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET status='Missing',missing_since=? WHERE id=?`, database.FormatDBTime(now), snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessRecordedFile(ctx, paths[3], ProcessOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT status,missing_since FROM recorded_videos WHERE id=?`, snapshot.ID).Scan(&status, &since); err != nil || status != "Recorded" || since != nil {
		t.Fatalf("restored status=%s since=%v err=%v", status, since, err)
	}
}

func TestMissingSnapshotCannotDeleteMovedOrRestoredVideo(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	rows, err := store.ListAllVideoSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := rows[0]
	target := filepath.Join(folder, "moved.ts")
	if err := os.Rename(paths[0], target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET file_path=? WHERE id=?`, target, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.handleMissingVideo(ctx, snapshot, true); err != nil {
		t.Fatal(err)
	}
	var status, path string
	if err := store.DB.QueryRow(`SELECT status,file_path FROM recorded_videos WHERE id=?`, snapshot.ID).Scan(&status, &path); err != nil || status != "Recorded" || path != target {
		t.Fatal("moved row was changed")
	}
	// 期限を過ぎた Missing のスナップショットでも、後から復帰した状態は保持する。
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET file_path=?,status='Missing',missing_since=? WHERE id=?`, paths[0], database.FormatDBTime(testFixedNow.Add(-8*24*time.Hour)), snapshot.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListAllVideoSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot = rows[0]
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET status='Recorded',missing_since=NULL WHERE id=?`, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.handleMissingVideo(ctx, snapshot, true); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT status FROM recorded_videos WHERE id=?`, snapshot.ID).Scan(&status); err != nil || status != "Recorded" {
		t.Fatal("restored row was deleted")
	}
}

func TestNewlyMissingMassLossDefersExpiredDeletion(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 11)
	ctx := context.Background()
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET status='Missing',missing_since=? WHERE file_path=?`, database.FormatDBTime(testFixedNow.Add(-8*24*time.Hour)), paths[0]); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths[1:4] {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{folder}}, Analyzer: &fakeAnalyzer{err: fmt.Errorf("unchanged files must not be analyzed")}, Logger: testLogger(t)}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos`).Scan(&count); err != nil || count != 11 {
		t.Fatal("mass disappearance deleted expired row")
	}
	if _, err := service.RunBatchScan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM recorded_videos`).Scan(&count); err != nil || count != 10 {
		t.Fatal("stable repeat scan did not delete expired row")
	}
	status := BatchScanStatus()
	if status.Running || status.Processed != 7 || status.Total != 11 || !status.Registered {
		t.Fatalf("batch progress %#v", status)
	}
}

func TestRecordingSymlinkNormalizationPreservesID(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(folder, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	aliasPath := filepath.Join(alias, filepath.Base(paths[0]))
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET file_path=? WHERE file_path=?`, aliasPath, paths[0]); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := rows[0].ID
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{alias}}, Analyzer: &fakeAnalyzer{err: fmt.Errorf("unchanged alias was reanalyzed")}, Logger: testLogger(t)}
	if _, err := service.RunBatchScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].FilePath != paths[0] || rows[0].Status != "Recorded" {
		t.Fatalf("alias changed identity %#v %v", rows, err)
	}
}

func TestLegacyGoTimestampPrecisionIsNormalizedWithoutAnalysis(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	modified := testFixedNow.Add(-time.Hour).Add(123456700 * time.Nanosecond)
	if err := os.Chtimes(paths[0], modified, modified); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	created := fileCreationTime(stat)
	actualModified := stat.ModTime()
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET file_created_at=?,file_modified_at=? WHERE file_path=?`, database.FormatDBTime(created.Truncate(time.Microsecond)), database.FormatDBTime(actualModified.Truncate(time.Microsecond)), paths[0]); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Analyzer: &fakeAnalyzer{err: fmt.Errorf("precision-only change must not reanalyze content")}, Logger: testLogger(t)}
	if err := service.ProcessRecordedFile(context.Background(), paths[0], ProcessOptions{}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Status != "Recorded" || !rows[0].FileCreatedAt.Equal(created.Round(time.Microsecond)) || !rows[0].FileModifiedAt.Equal(actualModified.Round(time.Microsecond)) {
		t.Fatalf("precision repair %#v %v", rows, err)
	}
}

func TestSameContentAnalysisRefreshesChangedFileTimes(t *testing.T) {
	store := openTestDB(t)
	folder := t.TempDir()
	paths := seedRecordedFiles(t, store, folder, 1)
	ctx := context.Background()
	old := testFixedNow.Add(-48 * time.Hour)
	if _, err := store.writeDB().Exec(`UPDATE recorded_videos SET file_created_at=?,file_modified_at=?`, database.FormatDBTime(old), database.FormatDBTime(old)); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAllVideoSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	program := buildTestProgram(paths[0], 3600)
	program.Title = "再解析で上書きしない"
	program.Video.FileHash = rows[0].FileHash
	program.Video.FileSize = rows[0].FileSize
	service := &Service{Store: store, Analyzer: &fakeAnalyzer{program: program}, Logger: testLogger(t)}
	if err := service.ProcessRecordedFile(ctx, paths[0], ProcessOptions{MetadataOnly: true}); err != nil {
		t.Fatal(err)
	}
	service.Analyzer = &fakeAnalyzer{err: fmt.Errorf("same unchanged recording was analyzed repeatedly")}
	if err := service.ProcessRecordedFile(ctx, paths[0], ProcessOptions{MetadataOnly: true}); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := store.DB.QueryRow(`SELECT title FROM recorded_programs`).Scan(&title); err != nil || title != "テスト番組" {
		t.Fatal("same-content refresh changed program metadata")
	}
}
