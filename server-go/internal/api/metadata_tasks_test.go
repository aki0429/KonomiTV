package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecordingMaintenanceTaskLifecycle(t *testing.T) {
	for _, name := range []string{"batch-scan", "background-analysis"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestServer(t, "")
			s.db.SetMaxOpenConns(1)
			if name == "batch-scan" {
				s.config.Video.RecordedFolders = []string{pg5NewSyntheticRecordingFolder(t)}
			}
			held, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			handler := s.Handler()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			route := "/api/maintenance/" + name
			go func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, route+"/run", nil).WithContext(ctx))
				done <- response
			}()
			catalogEventually(t, func() bool {
				response := doJSONRequest(t, handler, http.MethodGet, route+"/status", "", "", "")
				var status catalogTaskStatus
				return response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &status) == nil && status.Running
			})
			response := doJSONRequest(t, handler, http.MethodPost, route+"/run", "", "", "")
			if response.Code != 429 {
				t.Fatalf("concurrent task %d %s", response.Code, response.Body.String())
			}
			cancel()
			select {
			case <-done:
				t.Fatal("client disconnect stopped task")
			default:
			}
			_ = held.Close()
			select {
			case response = <-done:
				if response.Code != 204 {
					t.Fatal(response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("task did not finish")
			}
			response = doJSONRequest(t, handler, http.MethodGet, route+"/status", "", "", "")
			var status catalogTaskStatus
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Running || status.Registered {
				t.Fatal(response.Body.String())
			}
			if name == "batch-scan" && (status.Processed != 1 || status.Total != 1) {
				t.Fatal(response.Body.String())
			}
		})
	}
}

func TestMissingRecordingVisibility(t *testing.T) {
	s, _ := newTestServer(t, "")
	start := time.Now()
	visible := insertTestRecordedProgramWithChannel(t, s.db, nil, "見える番組", start)
	missing := insertTestRecordedProgramWithChannel(t, s.db, nil, "見える欠落番組", start.Add(-time.Hour))
	insertTestRecordedVideoWithFile(t, s.db, visible, "/test/visible.ts", "visible", "Recorded")
	insertTestRecordedVideoWithFile(t, s.db, missing, "/test/missing.ts", "missing", "Missing")
	handler := s.Handler()
	for _, route := range []string{"/api/videos", "/api/videos/search?query=見える", fmt.Sprintf("/api/videos?order=ids&ids=%d&ids=%d", missing, visible)} {
		response := doJSONRequest(t, handler, http.MethodGet, route, "", "", "")
		var list recordedProgramsResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &list) != nil || list.Total != 1 || len(list.RecordedPrograms) != 1 || list.RecordedPrograms[0].ID != visible {
			t.Fatalf("%s %s", route, response.Body.String())
		}
	}
	response := doJSONRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/videos/%d", missing), "", "", "")
	if response.Code != 422 {
		t.Fatal("missing recording detail was available")
	}
	if _, err := s.db.Exec(`UPDATE recorded_videos SET status='Recorded' WHERE recorded_program_id=?`, missing); err != nil {
		t.Fatal(err)
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/videos", "", "", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"total":2`) {
		t.Fatal("restored recording did not return to list")
	}
}
