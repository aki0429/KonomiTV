package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
)

func thumbnailUpdateFixture(t *testing.T) (*Server, int64, int64, string) {
	t.Helper()
	c, s, folder, _ := newCatalogTest(t)
	s.logger = slog.New(slog.NewTextHandler(thumbnailTestErrors{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	file := filepath.Join(folder, "source.jpg")
	if err := os.WriteFile(file, catalogJPEG(t, catalogMetadataFixture(), 60), 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := c.register(context.Background(), file, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	program := insertTestRecordedProgramWithChannel(t, s.db, nil, "different title", time.Date(2026, 11, 14, 10, 0, 0, 0, constants.JST))
	if _, err := s.writeDB.Exec(`UPDATE recorded_programs SET network_id=4,service_id=101,event_id=123 WHERE id=?`, program); err != nil {
		t.Fatal(err)
	}
	insertTestRecordedVideoWithFile(t, s.db, program, filepath.Join(folder, "recording.ts"), "capture-replacement", "Recorded")
	info, _ := json.Marshal(metadata.BuildThumbnailInfo(metadata.CalculateThumbnailLayout(3600)))
	if _, err := s.writeDB.Exec(`UPDATE recorded_videos SET thumbnail_info=? WHERE recorded_program_id=?`, string(info), program); err != nil {
		t.Fatal(err)
	}
	s.thumbnailRenderer.RunCommand = func(ctx context.Context, name string, args, env []string) error {
		return os.WriteFile(args[len(args)-1], []byte("converted capture"), 0600)
	}
	return s, program, capture, file
}

type thumbnailTestErrors struct{ t *testing.T }

func (w thumbnailTestErrors) Write(data []byte) (int, error) {
	w.t.Logf("thumbnail error: %s", data)
	return len(data), nil
}

func TestVideoThumbnailUpdateAvailableCopyAndSharedRevision(t *testing.T) {
	s, program, capture, first := thumbnailUpdateFixture(t)
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	second, third := first+".copy.jpg", first+".other.jpg"
	for _, file := range []string{second, third} {
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.captureCatalog.register(context.Background(), file, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	// 読めない先頭と、未走査の差し替え済みファイルを飛ばして三つ目を使う。
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	copyProgram := insertTestRecordedProgramWithChannel(t, s.db, nil, "same content", time.Now())
	insertTestRecordedVideoWithFile(t, s.db, copyProgram, first+".ts", "capture-replacement", "Recorded")
	if _, err := s.writeDB.Exec(`UPDATE recorded_videos SET thumbnail_info=(SELECT thumbnail_info FROM recorded_videos WHERE recorded_program_id=?) WHERE recorded_program_id=?`, program, copyProgram); err != nil {
		t.Fatal(err)
	}
	otherProgram := insertTestRecordedProgramWithChannel(t, s.db, nil, "other content", time.Now())
	insertTestRecordedVideoWithFile(t, s.db, otherProgram, first+".other.ts", "other-hash", "Recorded")
	handler := s.Handler()
	const requests = 8
	var workers sync.WaitGroup
	for range requests {
		workers.Go(func() {
			response := doJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/videos/%d/thumbnail", program), fmt.Sprintf(`{"capture_id":"%d"}`, capture), "", "application/json")
			if response.Code != 200 {
				t.Errorf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	workers.Wait()
	for _, id := range []int64{program, copyProgram} {
		var raw string
		if err := s.db.QueryRow(`SELECT thumbnail_info FROM recorded_videos WHERE recorded_program_id=?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var info metadata.ThumbnailInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			t.Fatal(err)
		}
		if info.Representative.Revision != requests+1 || info.Tile.TotalTiles != 360 {
			t.Fatalf("info=%+v", info)
		}
	}
	var raw string
	if err := s.db.QueryRow(`SELECT thumbnail_info FROM recorded_videos WHERE recorded_program_id=?`, otherProgram).Scan(&raw); err != nil || raw != `{"version": 1}` {
		t.Fatalf("unrelated info changed: %s %v", raw, err)
	}
	response := doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/videos/%d/thumbnail", program), "", "", "")
	if response.Code != 200 || response.Body.String() != "converted capture" {
		t.Fatalf("image=%d %q", response.Code, response.Body.String())
	}
}

func TestVideoThumbnailUpdateRejectsUnrelatedAndUnavailable(t *testing.T) {
	for _, test := range []struct {
		name         string
		edit         func(*testing.T, *Server, int64, int64, string)
		body, detail string
		status       int
	}{
		{"no thumbnails", func(t *testing.T, s *Server, p, c int64, f string) {
			s.writeDB.Exec(`UPDATE recorded_videos SET thumbnail_info=NULL`)
		}, "", "The thumbnails of the recorded program have not been generated yet", 422},
		{"missing capture", nil, `{"capture_id":9999}`, "Specified capture_id was not found", 422},
		{"large capture", nil, `{"capture_id":99999999999999999999999999999999}`, "Specified capture_id was not found", 422},
		{"network", func(t *testing.T, s *Server, p, c int64, f string) {
			s.writeDB.Exec(`UPDATE recorded_programs SET network_id=5`)
		}, "", "The capture does not belong to the recorded program", 422},
		{"service", func(t *testing.T, s *Server, p, c int64, f string) {
			s.writeDB.Exec(`UPDATE recorded_programs SET service_id=102`)
		}, "", "The capture does not belong to the recorded program", 422},
		{"event", func(t *testing.T, s *Server, p, c int64, f string) {
			s.writeDB.Exec(`UPDATE recorded_programs SET event_id=124`)
		}, "", "The capture does not belong to the recorded program", 422},
		{"one microsecond", func(t *testing.T, s *Server, p, c int64, f string) {
			s.writeDB.Exec(`UPDATE recorded_programs SET start_time='2026-11-14 10:00:00.000001+09:00'`)
		}, "", "The capture does not belong to the recorded program", 422},
		{"missing file", func(t *testing.T, s *Server, p, c int64, f string) {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}, "", "The capture file was not found or is not readable", 422},
		{"replaced file", func(t *testing.T, s *Server, p, c int64, f string) {
			if err := os.WriteFile(f, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "", "The capture file was not found or is not readable", 422},
		{"invalid image", func(t *testing.T, s *Server, p, c int64, f string) {
			data := []byte("invalid image")
			if err := os.WriteFile(f, data, 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(data)
			s.writeDB.Exec(`UPDATE captures SET file_hash=? WHERE id=?`, hash[:], c)
		}, "", "The capture file is not a valid image", 422},
		{"encoder failure", func(t *testing.T, s *Server, p, c int64, f string) {
			s.thumbnailRenderer.RunCommand = func(context.Context, string, []string, []string) error { return fmt.Errorf("encoder failed") }
		}, "", "Failed to save the thumbnail", 500},
		{"missing body field", nil, `{}`, `"type":"missing"`, 422},
		{"zero", nil, `{"capture_id":0}`, `"type":"greater_than"`, 422},
		{"fraction", nil, `{"capture_id":1.5}`, `"type":"int_from_float"`, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, p, c, file := thumbnailUpdateFixture(t)
			output := filepath.Join(s.paths.ThumbnailsDir, "capture-replacement.webp")
			if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(output, []byte("previous image"), 0600); err != nil {
				t.Fatal(err)
			}
			if test.edit != nil {
				test.edit(t, s, p, c, file)
			}
			body := test.body
			if body == "" {
				body = fmt.Sprintf(`{"capture_id":%d}`, c)
			}
			response := doJSONRequest(t, s.Handler(), http.MethodPut, fmt.Sprintf("/api/videos/%d/thumbnail", p), body, "", "application/json")
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.detail) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			data, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(data, []byte("previous image")) {
				t.Fatalf("previous thumbnail changed: %q %v", data, err)
			}
		})
	}
}

func TestVideoThumbnailUpdateWithRealFFmpeg(t *testing.T) {
	ffmpeg := os.Getenv("KONOMITV_TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("KONOMITV_TEST_FFMPEG is not set")
	}
	s, p, c, _ := thumbnailUpdateFixture(t)
	s.thumbnailRenderer.FFmpegPath = ffmpeg
	s.thumbnailRenderer.LibraryPath = filepath.Dir(ffmpeg)
	s.thumbnailRenderer.RunCommand = nil
	response := doJSONRequest(t, s.Handler(), http.MethodPut, fmt.Sprintf("/api/videos/%d/thumbnail", p), fmt.Sprintf(`{"capture_id":%d}`, c), "", "application/json")
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var info metadata.ThumbnailInfo
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil || info.Representative.Revision != 2 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	// 実変換でできた画像のデコードと寸法は metadata 側の実 FFmpeg 試験でも検証する。
	data, err := os.ReadFile(filepath.Join(s.paths.ThumbnailsDir, "capture-replacement.webp"))
	if err != nil || len(data) < 12 || string(data[8:12]) != "WEBP" {
		t.Fatalf("invalid WebP: %v", err)
	}
}

// 登録済み画像のデコード結果が、外部変換に渡す PNG に保たれることを確認する。
func TestVideoThumbnailUpdateEncoderReceivesImage(t *testing.T) {
	s, p, c, _ := thumbnailUpdateFixture(t)
	s.thumbnailRenderer.RunCommand = func(ctx context.Context, name string, args, env []string) error {
		for i, arg := range args {
			if arg == "-i" {
				file, err := os.Open(args[i+1])
				if err != nil {
					return err
				}
				defer file.Close()
				img, err := png.Decode(file)
				if err != nil {
					return err
				}
				red, _, _, alpha := img.At(0, 0).RGBA()
				if red == 0 || alpha != 65535 {
					return fmt.Errorf("invalid RGB image")
				}
			}
		}
		return os.WriteFile(args[len(args)-1], []byte("encoded"), 0600)
	}
	response := doJSONRequest(t, s.Handler(), http.MethodPut, fmt.Sprintf("/api/videos/%d/thumbnail", p), fmt.Sprintf(`{"capture_id":%d}`, c), "", "application/json")
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
