package api

import (
	"bytes"
	"encoding/json"
	"image"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/captures"
)

// captureFixturePath はテスト用のキャプチャ画像 (EXIF メタデータ付き) のパス。
const captureFixturePath = "../captures/testdata/capture_exif.jpg"

// setupCapturesTestServer はキャプチャ API のテスト用サーバーを用意する。
// 戻り値はサーバーとキャプチャの保存先フォルダ。
func setupCapturesTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	server, _ := newTestServer(t, "")
	uploadFolder := t.TempDir()
	server.config.Capture.UploadFolders = []string{uploadFolder}
	return server, uploadFolder
}

// copyCaptureFixture はテスト用のキャプチャ画像を保存先フォルダにコピーする。
func copyCaptureFixture(t *testing.T, folder string, filename string) {
	t.Helper()
	data, err := os.ReadFile(captureFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, filename), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCapturesList はキャプチャ一覧 API の挙動を検証する。
func TestCapturesList(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	copyCaptureFixture(t, uploadFolder, "capture1.jpg")
	copyCaptureFixture(t, uploadFolder, "capture2.jpg")
	// キャプチャ画像以外のファイルは無視される
	if err := os.WriteFile(filepath.Join(uploadFolder, "note.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var list struct {
		Total    int `json:"total"`
		Captures []struct {
			Filename        string          `json:"filename"`
			FileSize        int64           `json:"file_size"`
			FileModifiedAt  string          `json:"file_modified_at"`
			MimeType        string          `json:"mime_type"`
			ImageWidth      int             `json:"image_width"`
			ImageHeight     int             `json:"image_height"`
			CaptureMetadata json.RawMessage `json:"capture_metadata"`
		} `json:"captures"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Captures) != 2 {
		t.Fatalf("total = %d, captures = %d", list.Total, len(list.Captures))
	}
	first := list.Captures[0]
	if first.Filename != "capture1.jpg" && first.Filename != "capture2.jpg" {
		t.Errorf("filename = %q", first.Filename)
	}
	if first.MimeType != "image/jpeg" || first.ImageWidth != 1200 || first.ImageHeight != 800 {
		t.Errorf("capture = %+v", first)
	}
	// ファイルの更新日時は UTC の ISO8601 形式 (Pydantic は Z 付きで出力する)
	if !strings.HasSuffix(first.FileModifiedAt, "Z") {
		t.Errorf("file_modified_at = %q", first.FileModifiedAt)
	}
	// EXIF メタデータが抽出されている
	metadata := string(first.CaptureMetadata)
	if !strings.Contains(metadata, `"title":"テスト番組 第1話「日本語タイトル」"`) {
		t.Errorf("capture_metadata = %s", metadata)
	}
	if !strings.Contains(metadata, `"network_id":32736`) {
		t.Errorf("capture_metadata = %s", metadata)
	}

	// ファイル名での検索
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?search=capture2", "", "", "")
	if !strings.Contains(response.Body.String(), `"filename":"capture2.jpg"`) {
		t.Errorf("body = %s", response.Body.String())
	}
	// 番組名での検索 (EXIF メタデータ)
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?search=テスト番組", "", "", "")
	var searched struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if searched.Total != 2 {
		t.Errorf("search total = %d, want 2", searched.Total)
	}
	// チャンネル名での検索 (メタデータの NID/SID と一致するチャンネル)
	insertTestChannel(t, server.db, testChannel{
		ID: "NID32736-SID1024", DisplayChannelID: "gr011", NetworkID: 32736, ServiceID: 1024,
		RemoconID: 1, ChannelNumber: "011", Type: "GR", Name: "NHK総合1・東京", IsWatchable: true,
	})
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?search=NHK総合", "", "", "")
	if err := json.Unmarshal(response.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if searched.Total != 2 {
		t.Errorf("search by channel total = %d, want 2", searched.Total)
	}
	// 一致しない検索
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?search=not-found-keyword", "", "", "")
	if err := json.Unmarshal(response.Body.Bytes(), &searched); err != nil {
		t.Fatal(err)
	}
	if searched.Total != 0 {
		t.Errorf("search total = %d, want 0", searched.Total)
	}

	// 不正な order は 422
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?order=invalid", "", "", "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	// page=0 は 422
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?page=0", "", "", "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	// 範囲外のページは空になる
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures?page=100", "", "", "")
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Captures) != 0 {
		t.Errorf("total = %d, captures = %d", list.Total, len(list.Captures))
	}
}

// TestCaptureImage はキャプチャ画像取得 API の挙動を検証する。
func TestCaptureImage(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	copyCaptureFixture(t, uploadFolder, "capture.jpg")
	handler := server.Handler()

	// 元の画像
	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures/capture.jpg", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/jpeg" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", cacheControl)
	}
	if response.Body.Len() == 0 {
		t.Error("body is empty")
	}

	// サムネイル画像 (JPEG に変換され、長辺 400 に縮小される)
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/capture.jpg?thumbnail=true", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "image/jpeg" {
		t.Errorf("Content-Type = %q", contentType)
	}
	config, format, err := decodeImageConfig(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || config.Width != 267 || config.Height != 400 {
		t.Errorf("thumbnail = %s %dx%d", format, config.Width, config.Height)
	}

	// 存在しないファイルは 404
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/not-found.jpg", "", "", "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	// ディレクトリトラバーサルは 404
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/..%2Fcapture.jpg", "", "", "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

// TestCaptureDelete はキャプチャ画像削除 API の挙動を検証する。
func TestCaptureDelete(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	copyCaptureFixture(t, uploadFolder, "capture.jpg")
	handler := server.Handler()

	response := doJSONRequest(t, handler, http.MethodDelete, "/api/captures/capture.jpg", "", "", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(uploadFolder, "capture.jpg")); !os.IsNotExist(err) {
		t.Error("ファイルが削除されていません")
	}
	// 存在しないファイルは 404
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures/capture.jpg", "", "", "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

// TestCaptureUpload はキャプチャ画像アップロード API の挙動を検証する。
func TestCaptureUpload(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	handler := server.Handler()

	// JPEG をアップロードする
	response := uploadCapture(t, handler, "uploaded.jpg", captureFixturePath)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(uploadFolder, "uploaded.jpg")); err != nil {
		t.Fatalf("ファイルが保存されていません: %v", err)
	}

	// 同じファイル名を再度アップロードすると連番が付与される
	response = uploadCapture(t, handler, "uploaded.jpg", captureFixturePath)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if _, err := os.Stat(filepath.Join(uploadFolder, "uploaded-1.jpg")); err != nil {
		t.Fatalf("リネームされたファイルが保存されていません: %v", err)
	}

	// JPEG / PNG 以外は 422
	textPath := filepath.Join(t.TempDir(), "text.txt")
	if err := os.WriteFile(textPath, []byte("this is not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = uploadCapture(t, handler, "text.txt", textPath)
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
}

// TestCaptureFolders はキャプチャフォルダ CRUD とブックマーク API の挙動を検証する。
func TestCaptureFolders(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	userID := createTestUser(t, server.db, "capture_user", "password", false)
	token := issueTestToken(t, userID)
	handler := server.Handler()
	copyCaptureFixture(t, uploadFolder, "capture1.jpg")
	copyCaptureFixture(t, uploadFolder, "capture2.jpg")

	// 未ログインの場合は 401
	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders", "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	// フォルダが 1 つもない場合は空の一覧
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders", "", token, "")
	if response.Code != http.StatusOK || response.Body.String() != "{\"total\":0,\"folders\":[]}\n" {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	// フォルダを作成する (sort_order は 0 から始まる)
	response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/folders", `{"name":"フォルダ1"}`, token, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	folder := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &folder); err != nil {
		t.Fatal(err)
	}
	if folder["name"] != "フォルダ1" || folder["sort_order"] != float64(0) || folder["capture_count"] != float64(0) {
		t.Errorf("folder = %v", folder)
	}
	folderID := int(folder["id"].(float64))

	// 2 つ目のフォルダの sort_order は 1
	response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/folders", `{"name":"フォルダ2"}`, token, "")
	second := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second["sort_order"] != float64(1) {
		t.Errorf("sort_order = %v", second["sort_order"])
	}

	// 名前が空の場合は 422
	response = doJSONRequest(t, handler, http.MethodPost, "/api/captures/folders", `{"name":"  "}`, token, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}

	// キャプチャをフォルダに追加する (存在しないファイルは無視される)
	response = doJSONRequest(
		t, handler, http.MethodPost,
		"/api/captures/folders/"+strconv.Itoa(folderID)+"/captures",
		`{"filenames":["capture1.jpg","capture2.jpg","not-found.jpg"]}`, token, "",
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	// フォルダ内のキャプチャ一覧
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders/"+strconv.Itoa(folderID)+"/captures", "", token, "")
	var captures_ struct {
		Total    int `json:"total"`
		Captures []struct {
			Filename string `json:"filename"`
		} `json:"captures"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &captures_); err != nil {
		t.Fatal(err)
	}
	if captures_.Total != 2 || len(captures_.Captures) != 2 {
		t.Errorf("total = %d, captures = %d", captures_.Total, len(captures_.Captures))
	}

	// フォルダ一覧にキャプチャ数が反映される
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders", "", token, "")
	if !strings.Contains(response.Body.String(), `"capture_count":2`) {
		t.Errorf("body = %s", response.Body.String())
	}

	// フォルダ名と表示順序を更新する
	response = doJSONRequest(
		t, handler, http.MethodPut, "/api/captures/folders/"+strconv.Itoa(folderID),
		`{"name":"新しい名前","sort_order":5}`, token, "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	updated := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated["name"] != "新しい名前" || updated["sort_order"] != float64(5) || updated["capture_count"] != float64(2) {
		t.Errorf("folder = %v", updated)
	}

	// 他のユーザーのフォルダにはアクセスできない (404)
	otherUserID := createTestUser(t, server.db, "other_user", "password", false)
	otherToken := issueTestToken(t, otherUserID)
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders/"+strconv.Itoa(folderID)+"/captures", "", otherToken, "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}

	// フォルダからキャプチャを削除する
	response = doJSONRequest(
		t, handler, http.MethodDelete,
		"/api/captures/folders/"+strconv.Itoa(folderID)+"/captures",
		`{"filenames":["capture1.jpg"]}`, token, "",
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders/"+strconv.Itoa(folderID)+"/captures", "", token, "")
	if err := json.Unmarshal(response.Body.Bytes(), &captures_); err != nil {
		t.Fatal(err)
	}
	if captures_.Total != 1 {
		t.Errorf("total = %d, want 1", captures_.Total)
	}

	// フォルダを削除する (ブックマークも削除される)
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures/folders/"+strconv.Itoa(folderID), "", token, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	response = doJSONRequest(t, handler, http.MethodGet, "/api/captures/folders", "", token, "")
	if !strings.Contains(response.Body.String(), `"total":1`) {
		t.Errorf("body = %s", response.Body.String())
	}
	// ブックマークが残っていないことを確認する
	var count int
	if err := server.writeDB.QueryRow("SELECT COUNT(*) FROM capture_bookmarks").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("capture_bookmarks = %d, want 0", count)
	}
	// フォルダの削除後は 404
	response = doJSONRequest(t, handler, http.MethodDelete, "/api/captures/folders/"+strconv.Itoa(folderID), "", token, "")
	if response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

// TestCapturesResponseJSON はキャプチャ一覧の JSON フィールドが Python 版と一致することを検証する。
func TestCapturesResponseJSON(t *testing.T) {
	server, uploadFolder := setupCapturesTestServer(t)
	copyCaptureFixture(t, uploadFolder, "capture.jpg")
	handler := server.Handler()

	response := doJSONRequest(t, handler, http.MethodGet, "/api/captures", "", "", "")
	body := response.Body.String()
	// フィールドの並び順も Python 版 (schemas.Capture) と一致させる
	expectedFields := []string{
		`"filename"`, `"file_size"`, `"file_modified_at"`, `"mime_type"`,
		`"image_width"`, `"image_height"`, `"capture_metadata"`,
	}
	position := -1
	for _, field := range expectedFields {
		index := strings.Index(body, field)
		if index < 0 {
			t.Fatalf("フィールド %s がありません: %s", field, body)
		}
		if index < position {
			t.Errorf("フィールドの並び順が異なります: %s", body)
		}
		position = index
	}
	// capture_metadata のフィールドの並び順
	expectedMetadataFields := []string{
		`"captured_at"`, `"captured_playback_position"`, `"network_id"`, `"service_id"`, `"event_id"`,
		`"title"`, `"description"`, `"start_time"`, `"end_time"`, `"duration"`,
		`"caption_text"`, `"is_caption_composited"`, `"is_comment_composited"`,
	}
	position = -1
	for _, field := range expectedMetadataFields {
		index := strings.Index(body, field)
		if index < 0 {
			t.Fatalf("フィールド %s がありません: %s", field, body)
		}
		if index < position {
			t.Errorf("フィールドの並び順が異なります: %s", body)
		}
		position = index
	}
}

// uploadCapture はキャプチャ画像をアップロードするリクエストを実行する。
func uploadCapture(t *testing.T, handler http.Handler, filename string, path string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/captures", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// decodeImageConfig は画像のサイズと形式を取得する。
func decodeImageConfig(data []byte) (image.Config, string, error) {
	return image.DecodeConfig(bytes.NewReader(data))
}

// TestCaptureThumbnailMaxSize はサムネイルの最大サイズが Python 版と同じであることを検証する。
func TestCaptureThumbnailMaxSize(t *testing.T) {
	if captures.ThumbnailMaxSize != 400 {
		t.Errorf("ThumbnailMaxSize = %d, want 400", captures.ThumbnailMaxSize)
	}
	if captures.PageSize != 36 {
		t.Errorf("PageSize = %d, want 36", captures.PageSize)
	}
}
