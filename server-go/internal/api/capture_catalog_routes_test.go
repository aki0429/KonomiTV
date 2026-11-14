package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCatalogRoutes は実ファイル登録からページング・本人情報・表紙・ハッシュ確認まで通す。
func TestCatalogRoutes(t *testing.T) {
	c, s, folder, _ := newCatalogTest(t)
	ctx := context.Background()
	ids := []int64{}
	images := [][]byte{}
	for i := 0; i < 5; i++ {
		meta := catalogMetadataFixture()
		meta["captured_playback_position"] = i * 10
		meta["caption_text"] = fmt.Sprintf("字幕%d 100%%_日本", i)
		data := catalogJPEG(t, meta, byte(10+i))
		path := filepath.Join(folder, fmt.Sprintf("%d.jpg", i))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		id, err := c.register(ctx, path, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		images = append(images, data)
	}
	data := catalogJPEG(t, nil, 200)
	path := filepath.Join(folder, "outside.jpg")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.register(ctx, path, nil, false); err != nil {
		t.Fatal(err)
	}
	first := createTestUser(t, s.db, "catalog-first", "password", true)
	second := createTestUser(t, s.db, "catalog-second", "password", false)
	token := issueTestToken(t, first)
	secondToken := issueTestToken(t, second)
	handler := s.Handler()
	response := doJSONRequest(t, handler, http.MethodPost, "/api/captures/tags", `{"name":"本人タグ"}`, token, "")
	var tag captureTagResponse
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &tag) != nil {
		t.Fatal(response.Body.String())
	}
	for _, endpoint := range []string{"favorites", fmt.Sprintf("tags/%d/items", tag.ID)} {
		response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/"+endpoint, fmt.Sprintf(`{"capture_ids":[%d,%d]}`, ids[1], ids[2]), token, "")
		if response.Code != 204 {
			t.Fatal(response.Body.String())
		}
	}
	type page struct {
		Captures []catalogCapture `json:"captures"`
		Programs []catalogProgram `json:"programs"`
		Next     *string          `json:"next_cursor"`
	}
	for _, order := range []string{"CapturedAtDesc", "CapturedAtAsc", "PlaybackPosition", "BroadcastAtDesc", "BroadcastAtAsc"} {
		seen := map[int64]bool{}
		endpoint := "/api/captures?limit=2&order=" + order
		for n := 0; n < 4; n++ {
			response = doJSONRequest(t, handler, http.MethodGet, endpoint, "", token, "")
			var p page
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &p) != nil {
				t.Fatalf("%s status=%d %s", order, response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("private response cached")
			}
			for _, capture := range p.Captures {
				if seen[capture.ID] {
					t.Fatalf("%s duplicate %d endpoint=%s body=%s", order, capture.ID, endpoint, response.Body.String())
				}
				seen[capture.ID] = true
				if capture.Favorite == nil || capture.Tags == nil {
					t.Fatal("missing personal state")
				}
			}
			if p.Next == nil {
				break
			}
			endpoint = "/api/captures?limit=2&order=" + order + "&cursor=" + url.QueryEscape(*p.Next)
			if n == 0 {
				bad := doJSONRequest(t, handler, http.MethodGet, endpoint+"&query=changed", "", token, "")
				if bad.Code != 422 {
					t.Fatal("cursor reused with other filters")
				}
				bad = doJSONRequest(t, handler, http.MethodGet, endpoint, "", secondToken, "")
				if bad.Code != 422 {
					t.Fatal("cursor reused by other user")
				}
			}
		}
		expected := 6
		if order == "BroadcastAtDesc" || order == "BroadcastAtAsc" {
			expected = 5
		}
		if len(seen) != expected {
			t.Fatalf("%s got %d want %d", order, len(seen), expected)
		}
	}
	for _, test := range []struct {
		query, token  string
		status, count int
	}{
		{"?query=100%25_&query_targets=CaptionText", token, 200, 5},
		{"?query=100%25_&query_targets=ProgramTitle", token, 200, 0},
		{"?is_favorite=true", token, 200, 2}, {"?is_favorite=true", secondToken, 200, 0}, {"?is_favorite=true", "", 401, 0},
		{fmt.Sprintf("?tag_ids=%d", tag.ID), secondToken, 404, 0},
		{fmt.Sprintf("?tag_ids=%d&tag_ids=%d", tag.ID, tag.ID), token, 200, 2},
		{"?network_id=4", token, 422, 0}, {"?network_id=4&service_id=101&event_id=123&start_time=2026-11-14T10:00:00%2B09:00", token, 200, 5},
		{"?limit=0&day_start_hour=24", token, 422, 0}, {"?cursor=bad", token, 422, 0}, {"?has_metadata=false", token, 200, 1},
	} {
		response = doJSONRequest(t, handler, http.MethodGet, "/api/captures"+test.query, "", test.token, "")
		if response.Code != test.status {
			t.Fatalf("%s %d %s", test.query, response.Code, response.Body.String())
		}
		if test.status == 200 {
			var p page
			if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil || len(p.Captures) != test.count {
				t.Fatalf("%s body=%s", test.query, response.Body.String())
			}
		}
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?has_metadata=true", "", "", "")
	var p page
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &p) != nil || len(p.Programs) != 1 || p.Captures[0].Favorite != nil || p.Captures[0].Tags != nil {
		t.Fatalf("anonymous %s", response.Body.String())
	}
	program := p.Programs[0]
	response = doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/captures/programs/%d", program.ID), "", token, "")
	var album catalogAlbum
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &album) != nil || album.Count != 5 || album.Cover.ID != ids[2] {
		t.Fatalf("album %s", response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/programs?has_recorded_program=false&query=合成", "", token, "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"capture_count":5`)) {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/sections?has_metadata=true&day_start_hour=11", "", token, "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"date":"2026-11-13","count":5`)) {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/captures/%d", ids[0]), "", token, "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"file_path":`)) {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/captures/%d/image?hash=%s", ids[0], p.Captures[4].Hash), "", "", "")
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), images[0]) || response.Header().Get("ETag") != `"`+p.Captures[4].Hash+`"` {
		t.Fatalf("image status=%d", response.Code)
	}
	if err := os.WriteFile(filepath.Join(folder, "0.jpg"), images[1], 0600); err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/captures/%d/image?hash=%s", ids[0], p.Captures[4].Hash), "", "", "")
	if response.Code != 404 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("stale image was served")
	}
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures", fmt.Sprintf(`{"capture_ids":[%d,%d,999999]}`, ids[1], ids[1]), secondToken, "")
	if response.Code != 403 {
		t.Fatal("non-admin deleted images")
	}
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures", fmt.Sprintf(`{"capture_ids":[%d,%d,999999]}`, ids[1], ids[1]), token, "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"failed_capture_ids":[]`)) {
		t.Fatal(response.Body.String())
	}
}

// TestCatalogAssociationsAndSections は同一サービスの別チャンネル・Missing 録画・集計の更新を検証する。
func TestCatalogAssociationsAndSections(t *testing.T) {
	c, s, folder, _ := newCatalogTest(t)
	ctx := context.Background()
	handler := s.Handler()
	for i, id := range []string{"zz", "aa"} {
		insertTestChannel(t, s.writeDB, testChannel{ID: id, DisplayChannelID: id, NetworkID: 4, ServiceID: 101, RemoconID: i + 1, ChannelNumber: fmt.Sprintf("10%d", i), Type: "BS", Name: "共有チャンネル"})
	}
	start, err := time.Parse(time.RFC3339, "2026-11-14T10:00:00+09:00")
	if err != nil {
		t.Fatal(err)
	}
	recorded := []int64{}
	videos := []int64{}
	for _, status := range []string{"Recorded", "Missing"} {
		id := insertTestRecordedProgramWithChannel(t, s.writeDB, nil, "録画", start)
		if _, err := s.writeDB.Exec(`UPDATE recorded_programs SET network_id=4,service_id=101,event_id=123,start_time=? WHERE id=?`, database.FormatDBTime(start), id); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, id)
		videos = append(videos, insertTestRecordedVideoWithFile(t, s.writeDB, id, "/fixture/"+status+".ts", status, status))
	}
	path := filepath.Join(folder, "one.jpg")
	if err := os.WriteFile(path, catalogJPEG(t, catalogMetadataFixture(), 11), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.register(ctx, path, nil, false); err != nil {
		t.Fatal(err)
	}
	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures?channel_ids=zz", "", "", "")
	var page struct {
		Captures []catalogCapture
		Programs []catalogProgram
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Captures) != 1 || len(page.Programs) != 1 {
		t.Fatal(response.Body.String())
	}
	program := page.Programs[0]
	if program.Channel == nil || program.Channel.ID != "aa" || len(program.RecordedIDs) != 1 || program.RecordedIDs[0] != recorded[0] {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/channels", "", "", "")
	var channels []catalogChannel
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &channels) != nil || len(channels) != 2 || channels[0].ID != "zz" {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/captures?recorded_program_id=%d", recorded[0]), "", "", "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Captures) != 1 {
		t.Fatal(response.Body.String())
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/programs?has_recorded_program=true", "", "", "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"capture_count":1`)) {
		t.Fatal(response.Body.String())
	}
	if _, err := s.writeDB.Exec(`UPDATE recorded_videos SET status='Missing' WHERE id=?`, videos[0]); err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/programs?has_recorded_program=true", "", "", "")
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"programs":[]`)) {
		t.Fatal(response.Body.String())
	}
	sections := func(expected int64) {
		t.Helper()
		response := doJSONRequest(t, handler, http.MethodGet, "/api/captures/sections", "", "", "")
		var result catalogSectionsResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Total != expected {
			t.Fatalf("sections want %d %s", expected, response.Body.String())
		}
	}
	sections(1)
	sections(1)
	second := filepath.Join(folder, "two.jpg")
	if err := os.WriteFile(second, catalogJPEG(t, catalogMetadataFixture(), 200), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.register(ctx, second, nil, false); err != nil {
		t.Fatal(err)
	}
	sections(2)
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := c.markMissing(ctx, second); err != nil {
		t.Fatal(err)
	}
	sections(1)
}
