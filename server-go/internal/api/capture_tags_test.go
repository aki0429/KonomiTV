package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// TestCaptureTagsAndFavorites は所有者の分離・一括変更の原子性・実ファイルのある件数を検証する。
func TestCaptureTagsAndFavorites(t *testing.T) {
	s, _ := newTestServer(t, "")
	// 実サービスと同じく、単一の書き込み接続で外部キーを有効にする。
	s.writeDB.SetMaxOpenConns(1)
	if _, err := s.writeDB.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := database.EnsureSchema(context.Background(), s.writeDB); err != nil {
		t.Fatal(err)
	}
	first := createTestUser(t, s.db, "first", "password", false)
	second := createTestUser(t, s.db, "second", "password", false)
	firstToken, secondToken := issueTestToken(t, first), issueTestToken(t, second)
	handler := s.Handler()
	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures/tags", "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatal("anonymous tag list was allowed")
	}
	response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/tags", `{"name":"  アニメ  "}`, firstToken, "")
	var tag captureTagResponse
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &tag) != nil || tag.Name != "アニメ" {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	tagPath := fmt.Sprintf("/api/captures/tags/%d", tag.ID)
	response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/tags", `{"name":"アニメ"}`, firstToken, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("duplicate name was allowed")
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		response = doJSONRequest(t, handler, method, tagPath, `{"name":"other"}`, secondToken, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("other user tag access=%d", response.Code)
		}
	}
	_, err := s.writeDB.Exec(`
		INSERT INTO captures(id,file_hash,file_size,captured_at,image_width,image_height,metadata_status) VALUES
		 (1,X'01',10,'2026-11-14 10:00:00+09:00',1920,1080,'NoMetadata'),
		 (2,X'02',10,'2026-11-14 10:00:01+09:00',1920,1080,'NoMetadata');
		INSERT INTO capture_files(capture_id,file_path,file_name,file_size,file_created_at,file_modified_at,status) VALUES
		 (1,'one.jpg','one.jpg',10,'2026-11-14 10:00:00+09:00','2026-11-14 10:00:00+09:00','Available'),
		 (1,'duplicate.jpg','duplicate.jpg',10,'2026-11-14 10:00:00+09:00','2026-11-14 10:00:00+09:00','Available'),
		 (2,'missing.jpg','missing.jpg',10,'2026-11-14 10:00:00+09:00','2026-11-14 10:00:00+09:00','Missing');
	`)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{tagPath + "/items", "/api/captures/favorites"} {
		response = doJSONRequest(t, handler, http.MethodPost, target, `{"capture_ids":[1,999]}`, firstToken, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("invalid id status=%d body=%s", response.Code, response.Body.String())
		}
		table := "capture_favorites"
		if target != "/api/captures/favorites" {
			table = "capture_tag_items"
		}
		var partialCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&partialCount); err != nil || partialCount != 0 {
			t.Fatalf("invalid ID caused partial changes in %s: %d %v", table, partialCount, err)
		}
		for range 2 {
			response = doJSONRequest(t, handler, http.MethodPost, target, `{"capture_ids":[1,"2",1.0]}`, firstToken, "")
			if response.Code != http.StatusNoContent {
				t.Fatalf("add status=%d body=%s", response.Code, response.Body.String())
			}
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_favorites WHERE user_id=?`, first).Scan(&count); err != nil || count != 2 {
		t.Fatalf("favorite duplicates or missing rows: %d %v", count, err)
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/tags", "", firstToken, "")
	var tags []captureTagResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &tags) != nil || len(tags) != 1 || tags[0].CaptureCount != 1 || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("tag count/privacy status=%d body=%s", response.Code, response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/tags", "", secondToken, "")
	if response.Code != http.StatusOK || response.Body.String() != "[]\n" {
		t.Fatalf("other user saw tags: %s", response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures/favorites", `{"capture_ids":[1,999]}`, secondToken, "")
	if response.Code != http.StatusNoContent {
		t.Fatal("favorite deletion was not idempotent")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_favorites WHERE user_id=?`, first).Scan(&count); err != nil || count != 2 {
		t.Fatal("other user removed favorites")
	}
	response = doJSONRequest(t, handler, http.MethodPut, tagPath, `{"name":"ドラマ"}`, firstToken, "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &tag) != nil || tag.Name != "ドラマ" || tag.CaptureCount != 1 {
		t.Fatalf("rename status=%d body=%s", response.Code, response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodDelete, tagPath, "", firstToken, "")
	if response.Code != http.StatusNoContent {
		t.Fatal("owner could not delete tag")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM capture_tag_items`).Scan(&count); err != nil || count != 0 {
		t.Fatal("tag items did not cascade on deletion")
	}
}
