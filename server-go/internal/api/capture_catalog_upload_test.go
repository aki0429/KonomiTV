package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func catalogUploadRequest(t *testing.T, filename string, data []byte, modified *string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if modified != nil {
		if err = writer.WriteField("file_modified_at", *modified); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/captures", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestCatalogUploadValidationAndConcurrentNames(t *testing.T) {
	_, s, folder, _ := newCatalogTest(t)
	handler := s.Handler()
	data := catalogJPEG(t, catalogMetadataFixture(), 30)
	for _, test := range []struct {
		name, modified string
		status         int
		data           []byte
	}{
		{"../outside.jpg", "", 422, data}, {`..\outside.jpg`, "", 422, data}, {"C:outside.jpg", "", 422, data}, {"image.bat", "", 422, data},
		{"image.jpg", "NaN", 422, data}, {"image.jpg", "Inf", 422, data}, {"image.jpg", "1e100", 422, data}, {"broken.jpg", "", 422, []byte{255, 216, 255}},
	} {
		var modified *string
		if test.modified != "" {
			modified = &test.modified
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, catalogUploadRequest(t, test.name, test.data, modified))
		if response.Code != test.status {
			t.Fatalf("%s %s got %d %s", test.name, test.modified, response.Code, response.Body.String())
		}
	}
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected upload left a file")
	}
	modified := "1763082000123.456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, catalogUploadRequest(t, "sanitized?.jpg", data, &modified))
	var first catalogCapture
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &first) != nil || first.ID == 0 || first.Favorite != nil || first.Tags != nil {
		t.Fatal(response.Body.String())
	}
	stat, err := os.Stat(filepath.Join(folder, "sanitized_.jpg"))
	if err != nil || stat.ModTime().UnixMicro() != 1763082000123456 {
		t.Fatalf("modification time not preserved: %v %v", stat, err)
	}
	// 保存名が同じで内容が異なる並行アップロードでも、上書きせずに全て保存する。
	requests := make([]*http.Request, 8)
	for i := range requests {
		meta := catalogMetadataFixture()
		meta["caption_text"] = fmt.Sprintf("concurrent-%d", i)
		requests[i] = catalogUploadRequest(t, "simultaneous.jpg", catalogJPEG(t, meta, byte(32+i*24)), nil)
	}
	var group sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, len(requests))
	for _, request := range requests {
		group.Add(1)
		go func(r *http.Request) {
			defer group.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			responses <- response
		}(request)
	}
	group.Wait()
	close(responses)
	ids := map[int64]bool{}
	for response := range responses {
		var capture catalogCapture
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &capture) != nil || ids[capture.ID] {
			t.Fatalf("concurrent upload %d %s", response.Code, response.Body.String())
		}
		ids[capture.ID] = true
	}
	for i := range requests {
		name := "simultaneous.jpg"
		if i > 0 {
			name = fmt.Sprintf("simultaneous-%d.jpg", i)
		}
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Fatal(err)
		}
	}
	// 同一内容の再送はファイルを増やさず、古いハッシュの実体が無い場合は保存をやり直す。
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, catalogUploadRequest(t, "duplicate.jpg", data, nil))
	var duplicate catalogCapture
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &duplicate) != nil || duplicate.ID != first.ID {
		t.Fatal(response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(folder, "duplicate.jpg")); !os.IsNotExist(err) {
		t.Fatal("duplicate file created")
	}
	if err := os.Remove(filepath.Join(folder, "sanitized_.jpg")); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, catalogUploadRequest(t, "restored.jpg", data, nil))
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(folder, "restored.jpg")); err != nil {
		t.Fatal(err)
	}
	// 上限の直前まで読み込み、保存も登録も始める前に 413 を返す。
	large := make([]byte, catalogUploadLimit+1)
	copy(large, data)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, catalogUploadRequest(t, "large.jpg", large, nil))
	if response.Code != 413 {
		t.Fatalf("oversize %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(folder, "large.jpg")); !os.IsNotExist(err) {
		t.Fatal("oversize file created")
	}
}
