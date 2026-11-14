package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

type migrationReferenceRow struct {
	ID         int64           `json:"id"`
	Status     string          `json:"status"`
	Path       string          `json:"file_path"`
	Hash       string          `json:"file_hash"`
	Container  string          `json:"container_format"`
	Duration   any             `json:"duration"`
	FrameRate  any             `json:"video_frame_rate"`
	KeyFrames  json.RawMessage `json:"key_frames"`
	SegmentMap json.RawMessage `json:"segment_map"`
}

type migrationReferenceCounts struct {
	Migrated int `json:"migrated"`
	Repaired int `json:"repaired"`
	Skipped  int `json:"skipped"`
}

type migrationReferenceCase struct {
	Name            string `json:"name"`
	Input, Expected []migrationReferenceRow
	Counts          migrationReferenceCounts `json:"counts"`
}

func migrationReference(t *testing.T) []migrationReferenceCase {
	t.Helper()
	data, err := os.ReadFile("testdata/keyframe_migration_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Commit string                   `json:"upstream_commit"`
		Cases  []migrationReferenceCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Commit != "687b904bb0a2cbb329596fd7b0368c09afe19a6c" || len(fixture.Cases) != 33 {
		t.Fatalf("unexpected reference %s cases=%d", fixture.Commit, len(fixture.Cases))
	}
	return fixture.Cases
}

func assertMigrationJSON(t *testing.T, actual, expected string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal([]byte(actual), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache=%s, want=%s", actual, expected)
	}
}

// 本家のクラスと移行メソッドを AST から実行した 192 行の期待値に照合する。
func TestKeyFrameMigrationReference(t *testing.T) {
	for _, test := range migrationReference(t) {
		t.Run(test.Name, func(t *testing.T) {
			counts := migrationReferenceCounts{}
			for index, input := range test.Input {
				frames, cache := string(input.KeyFrames), string(input.SegmentMap)
				if input.Status == "Recorded" {
					row := keyFrameMigrationRow{ID: input.ID, Path: input.Path, Hash: input.Hash, Container: input.Container, Duration: input.Duration, FrameRate: input.FrameRate,
						KeyFrames: sql.NullString{String: frames, Valid: true}, SegmentMap: sql.NullString{String: cache, Valid: true}}
					change, err := planKeyFrameMigration(row)
					if err != nil {
						counts.Skipped++
					} else {
						if change.ClearFrames {
							frames = "[]"
							counts.Migrated++
						} else if change.SegmentMap != nil {
							counts.Repaired++
						}
						if change.SegmentMap != nil {
							cache = *change.SegmentMap
						}
					}
				}
				assertMigrationJSON(t, frames, string(test.Expected[index].KeyFrames))
				assertMigrationJSON(t, cache, string(test.Expected[index].SegmentMap))
			}
			if counts != test.Counts {
				t.Fatalf("counts=%+v, want=%+v", counts, test.Counts)
			}
		})
	}
}

func TestKeyFrameMigrationPagesAndIdempotence(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	test := migrationReference(t)[32]
	ctx := context.Background()
	for _, input := range test.Input {
		program := buildTestProgram(input.Path, 60)
		program.Video.Status = input.Status
		if _, err := store.SaveRecordedMetadata(ctx, program, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB.Exec(`UPDATE recorded_videos SET duration=?,video_frame_rate=?,key_frames=?,segment_map=? WHERE file_path=?`, input.Duration, input.FrameRate, string(input.KeyFrames), string(input.SegmentMap), input.Path); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Store: store, Logger: testLogger(t)}
	if err := service.migrateKeyFramesToSegmentMap(ctx); err != nil {
		t.Fatal(err)
	}
	for _, expected := range test.Expected {
		var frames, cache, status string
		if err := store.DB.QueryRow(`SELECT key_frames,segment_map,status FROM recorded_videos WHERE file_path=?`, expected.Path).Scan(&frames, &cache, &status); err != nil {
			t.Fatal(err)
		}
		if status != expected.Status {
			t.Fatalf("status changed: %s", status)
		}
		assertMigrationJSON(t, frames, string(expected.KeyFrames))
		assertMigrationJSON(t, cache, string(expected.SegmentMap))
	}
	var before, after int
	if err := store.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := service.migrateKeyFramesToSegmentMap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("repeated migration wrote %d rows", after-before)
	}
}

func migrationSeed(t *testing.T, store *Store, path string) keyFrameMigrationRow {
	t.Helper()
	ctx := context.Background()
	if _, err := store.SaveRecordedMetadata(ctx, buildTestProgram(path, 60), 0); err != nil {
		t.Fatal(err)
	}
	frames := `[{"dts":9000000,"offset":188},{"dts":9540000,"offset":188000},{"sentinel":true}]`
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET key_frames=?,duration=60,video_frame_rate=30 WHERE file_path=?`, frames, path); err != nil {
		t.Fatal(err)
	}
	rows, err := store.keyFrameMigrationBatch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Path == path {
			return row
		}
	}
	t.Fatal("migration row missing")
	return keyFrameMigrationRow{}
}

func TestKeyFrameMigrationDoesNotOverwriteChangedRows(t *testing.T) {
	for _, column := range []string{"segment_map", "key_frames", "status", "file_path", "file_hash", "video_frame_rate", "duration", "container_format"} {
		t.Run(column, func(t *testing.T) {
			store := openTestDB(t)
			store.DB.SetMaxOpenConns(1)
			ctx := context.Background()
			row := migrationSeed(t, store, "/fixture/source.ts")
			change, err := planKeyFrameMigration(row)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			switch column {
			case "segment_map":
				value = videostream.MarshalSegmentMap([]videostream.SegmentMapEntry{{SequenceIndex: 5, SourceFilePosition: 987654, SourceStartDTS: 12000000}})
			case "key_frames":
				value = `[{"dts":1,"offset":2}]`
			case "status":
				value = "Recording"
			case "file_path":
				value = "/fixture/moved.ts"
			case "file_hash":
				value = "new-content"
			case "video_frame_rate":
				value = 60
			case "duration":
				value = 120
			case "container_format":
				value = "MPEG-4"
			}
			if _, err := store.DB.Exec(`UPDATE recorded_videos SET `+column+`=? WHERE id=?`, value, row.ID); err != nil {
				t.Fatal(err)
			}
			var beforeFrames, beforeMap string
			if err := store.DB.QueryRow(`SELECT key_frames,segment_map FROM recorded_videos WHERE id=?`, row.ID).Scan(&beforeFrames, &beforeMap); err != nil {
				t.Fatal(err)
			}
			if saved, err := store.saveKeyFrameMigration(ctx, row, change); err != nil || saved {
				t.Fatalf("stale snapshot saved=%v err=%v", saved, err)
			}
			var frames, cache string
			if err := store.DB.QueryRow(`SELECT key_frames,segment_map FROM recorded_videos WHERE id=?`, row.ID).Scan(&frames, &cache); err != nil {
				t.Fatal(err)
			}
			if frames != beforeFrames || cache != beforeMap {
				t.Fatal("concurrent metadata/cache was replaced")
			}
			if column == "segment_map" {
				// 新しい再生位置を読み直した次の移行では、それを保ったまま旧配列だけ除く。
				service := &Service{Store: store, Logger: testLogger(t)}
				if err := service.migrateKeyFramesToSegmentMap(ctx); err != nil {
					t.Fatal(err)
				}
				if err := store.DB.QueryRow(`SELECT key_frames,segment_map FROM recorded_videos WHERE id=?`, row.ID).Scan(&frames, &cache); err != nil {
					t.Fatal(err)
				}
				if frames != "[]" || cache != beforeMap {
					t.Fatal("fresh playback cache was not retained")
				}
			}
		})
	}
}

func TestKeyFrameMigrationKeepsInvalidRowsAndContinues(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	ctx := context.Background()
	badJSON := migrationSeed(t, store, "/fixture/bad-json.ts")
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET key_frames='invalid-json' WHERE id=?`, badJSON.ID); err != nil {
		t.Fatal(err)
	}
	badFPS := migrationSeed(t, store, "/fixture/bad-fps.ts")
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET video_frame_rate='broken' WHERE id=?`, badFPS.ID); err != nil {
		t.Fatal(err)
	}
	good := migrationSeed(t, store, "/fixture/good.ts")
	service := &Service{Store: store, Logger: testLogger(t)}
	if err := service.migrateKeyFramesToSegmentMap(ctx); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id   int64
		want string
	}{{badJSON.ID, "invalid-json"}, {badFPS.ID, badFPS.KeyFrames.String}, {good.ID, "[]"}} {
		var raw string
		if err := store.DB.QueryRow(`SELECT key_frames FROM recorded_videos WHERE id=?`, test.id).Scan(&raw); err != nil || raw != test.want {
			t.Fatalf("id=%d frames=%q err=%v", test.id, raw, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := service.migrateKeyFramesToSegmentMap(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestBatchScanMigratesLegacyKeyFrames(t *testing.T) {
	store := openTestDB(t)
	store.DB.SetMaxOpenConns(1)
	root := t.TempDir()
	paths := seedRecordedFiles(t, store, root, 1)
	frames := `[{"dts":9000000,"offset":188},{"dts":9540000,"offset":188000},{"dts":9600000,"offset":200000}]`
	if _, err := store.DB.Exec(`UPDATE recorded_videos SET key_frames=?,video_frame_rate=30`, frames); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Config: ServiceConfig{RecordedFolders: []string{root}}, Analyzer: &fakeAnalyzer{err: errors.New("unchanged file must not be analyzed")}, Logger: testLogger(t)}
	if _, err := service.RunBatchScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var old, cache, status string
	if err := store.DB.QueryRow(`SELECT key_frames,segment_map,status FROM recorded_videos WHERE file_path=?`, paths[0]).Scan(&old, &cache, &status); err != nil {
		t.Fatal(err)
	}
	if old != "[]" || cache == "[]" || status != "Recorded" {
		t.Fatalf("legacy cache not migrated: %s %s %s", old, cache, status)
	}
}
