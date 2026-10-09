package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/captures"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// capturePageSize はキャプチャ一覧 API の 1 ページあたりの表示件数。
const capturePageSize = captures.PageSize

// capturesResponse はキャプチャ一覧レスポンス (server/app/schemas.py の Captures 相当) 。
type capturesResponse struct {
	Total    int                   `json:"total"`
	Captures []captureResponseItem `json:"captures"`
}

// captureResponseItem はキャプチャ画像のメタデータ (server/app/schemas.py の Capture 相当) 。
type captureResponseItem struct {
	Filename        string             `json:"filename"`
	FileSize        int64              `json:"file_size"`
	FileModifiedAt  string             `json:"file_modified_at"`
	MimeType        string             `json:"mime_type"`
	ImageWidth      int                `json:"image_width"`
	ImageHeight     int                `json:"image_height"`
	CaptureMetadata *captures.Metadata `json:"capture_metadata"`
}

// captureFoldersResponse はキャプチャフォルダ一覧レスポンス (server/app/schemas.py の CaptureFolders 相当) 。
type captureFoldersResponse struct {
	Total   int                     `json:"total"`
	Folders []captureFolderResponse `json:"folders"`
}

// captureFolderResponse はキャプチャフォルダ情報 (server/app/schemas.py の CaptureFolder 相当) 。
type captureFolderResponse struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	SortOrder    int    `json:"sort_order"`
	CaptureCount int    `json:"capture_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// toCaptureResponse は captures.Capture をレスポンス用の構造体に変換する。
func toCaptureResponse(capture captures.Capture) captureResponseItem {
	// ファイルの更新日時は UTC の ISO8601 形式 (Pydantic は UTC の datetime を "Z" 付きで出力する)
	return captureResponseItem{
		Filename:        capture.Filename,
		FileSize:        capture.FileSize,
		FileModifiedAt:  formatJSONTimeUTC(capture.FileModifiedAt),
		MimeType:        capture.MimeType,
		ImageWidth:      capture.ImageWidth,
		ImageHeight:     capture.ImageHeight,
		CaptureMetadata: capture.Metadata,
	}
}

// formatJSONTimeUTC は UTC の日時を Pydantic v2 と同じ形式 (末尾が "Z") で出力する。
// Python 版は datetime.fromtimestamp(mtime, tz=UTC) を使うため、オフセットではなく Z が付く。
func formatJSONTimeUTC(value time.Time) string {
	// Python の datetime はマイクロ秒精度のため、マイクロ秒未満は四捨五入する
	// (Python の round() は銀行丸めのため、ちょうど 0.5 マイクロ秒の場合は偶数側に丸める)
	inUTC := roundToMicrosecond(value.UTC())
	if inUTC.Nanosecond() == 0 {
		return inUTC.Format("2006-01-02T15:04:05Z")
	}
	return inUTC.Format("2006-01-02T15:04:05.000000Z")
}

// handleCaptures はキャプチャ一覧 API (GET /api/captures) を処理する。
// 移植元: server/app/routers/CapturesRouter.py の CaptureListAPI()
func (s *Server) handleCaptures(w http.ResponseWriter, r *http.Request) {
	order := r.URL.Query().Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "desc" && order != "asc" {
		writeError(w, http.StatusUnprocessableEntity, "Input should be 'desc' or 'asc'")
		return
	}
	page, ok := parsePositiveQueryInt(w, r, "page", 1)
	if !ok {
		return
	}
	search := r.URL.Query().Get("search")
	hasSearch := r.URL.Query().Has("search")

	folders := captures.UploadFolders(s.config.Capture.UploadFolders)
	files := captures.CollectCaptureFiles(folders)

	// 検索が指定されている場合はフィルタリングする
	if hasSearch {
		lowerSearch := strings.ToLower(search)
		// 検索語に一致するチャンネルの network_id / service_id の組を事前に収集する
		channelPairs := map[[2]int]bool{}
		channels, err := database.SearchChannelsByName(r.Context(), s.db, search)
		if err != nil {
			s.logger.Error("[CapturesRouter][CaptureListAPI] Failed to search channels.", "error", err)
		}
		for _, channel := range channels {
			channelPairs[[2]int{channel.NetworkID, channel.ServiceID}] = true
		}

		filtered := []captures.File{}
		for _, file := range files {
			// Step 1: ファイル名での部分一致検索
			if strings.Contains(strings.ToLower(file.Filename), lowerSearch) {
				filtered = append(filtered, file)
				continue
			}
			// Step 2: ファイル名でマッチしなかった場合は EXIF メタデータを読み取る
			metadata := captures.ExtractMetadataOnly(file.Path)
			if metadata == nil {
				continue
			}
			// Step 2a: 番組名での部分一致検索
			if strings.Contains(strings.ToLower(metadata.Title), lowerSearch) {
				filtered = append(filtered, file)
				continue
			}
			// Step 2b: チャンネル名での検索
			if channelPairs[[2]int{metadata.NetworkID, metadata.ServiceID}] {
				filtered = append(filtered, file)
			}
		}
		files = filtered
	}

	// ファイルの更新日時でソートする
	sortCapturesByModifiedAt(files, order == "desc")

	// ページネーションを適用する
	pageFiles := paginateCaptures(files, page)

	// 現在のページのファイルからメタデータを抽出する
	items := []captureResponseItem{}
	for _, file := range pageFiles {
		items = append(items, toCaptureResponse(captures.ExtractCaptureInfo(file.Path)))
	}
	writeJSON(w, http.StatusOK, capturesResponse{Total: len(files), Captures: items})
}

// handleCaptureFolderList はキャプチャフォルダ一覧 API (GET /api/captures/folders) を処理する。
func (s *Server) handleCaptureFolderList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folders, counts, err := database.ListCaptureFolders(r.Context(), s.writeDB, user.ID)
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderListAPI] Failed to list capture folders.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to list capture folders")
		return
	}
	items := []captureFolderResponse{}
	for _, folder := range folders {
		items = append(items, toCaptureFolderResponse(folder, counts[folder.ID]))
	}
	writeJSON(w, http.StatusOK, captureFoldersResponse{Total: len(items), Folders: items})
}

// handleCaptureFolderCreate はキャプチャフォルダ作成 API (POST /api/captures/folders) を処理する。
func (s *Server) handleCaptureFolderCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !decodeCaptureJSONBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" {
		writeError(w, http.StatusUnprocessableEntity, "Folder name cannot be empty")
		return
	}
	folder, err := database.CreateCaptureFolder(r.Context(), s.writeDB, user.ID, strings.TrimSpace(request.Name))
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCreateAPI] Failed to create the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to create the capture folder")
		return
	}
	writeJSON(w, http.StatusCreated, toCaptureFolderResponse(*folder, 0))
}

// handleCaptureFolderUpdate はキャプチャフォルダ更新 API (PUT /api/captures/folders/{folder_id}) を処理する。
func (s *Server) handleCaptureFolderUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folderID, ok := parsePathInt(w, r, "folder_id")
	if !ok {
		return
	}
	var request struct {
		Name      *string `json:"name"`
		SortOrder *int    `json:"sort_order"`
	}
	if !decodeCaptureJSONBody(w, r, &request) {
		return
	}
	if request.Name != nil && strings.TrimSpace(*request.Name) == "" {
		writeError(w, http.StatusUnprocessableEntity, "Folder name cannot be empty")
		return
	}
	var name *string
	if request.Name != nil {
		trimmed := strings.TrimSpace(*request.Name)
		name = &trimmed
	}
	folder, err := database.UpdateCaptureFolder(r.Context(), s.writeDB, folderID, user.ID, name, request.SortOrder)
	if errors.Is(err, database.ErrCaptureFolderNotFound) {
		writeError(w, http.StatusNotFound, "Capture folder not found")
		return
	}
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderUpdateAPI] Failed to update the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to update the capture folder")
		return
	}
	count, err := database.CountCaptureBookmarks(r.Context(), s.writeDB, folder.ID)
	if err != nil {
		count = 0
	}
	writeJSON(w, http.StatusOK, toCaptureFolderResponse(*folder, count))
}

// handleCaptureFolderDelete はキャプチャフォルダ削除 API (DELETE /api/captures/folders/{folder_id}) を処理する。
func (s *Server) handleCaptureFolderDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folderID, ok := parsePathInt(w, r, "folder_id")
	if !ok {
		return
	}
	err := database.DeleteCaptureFolder(r.Context(), s.writeDB, folderID, user.ID)
	if errors.Is(err, database.ErrCaptureFolderNotFound) {
		writeError(w, http.StatusNotFound, "Capture folder not found")
		return
	}
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderDeleteAPI] Failed to delete the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to delete the capture folder")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCaptureFolderCaptureList はフォルダ内キャプチャ一覧 API
// (GET /api/captures/folders/{folder_id}/captures) を処理する。
func (s *Server) handleCaptureFolderCaptureList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folderID, ok := parsePathInt(w, r, "folder_id")
	if !ok {
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "desc" && order != "asc" {
		writeError(w, http.StatusUnprocessableEntity, "Input should be 'desc' or 'asc'")
		return
	}
	page, ok := parsePositiveQueryInt(w, r, "page", 1)
	if !ok {
		return
	}
	folder, err := database.GetCaptureFolder(r.Context(), s.writeDB, folderID, user.ID)
	if errors.Is(err, database.ErrCaptureFolderNotFound) {
		writeError(w, http.StatusNotFound, "Capture folder not found")
		return
	}
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCaptureListAPI] Failed to get the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get the capture folder")
		return
	}

	filenames, err := database.ListCaptureBookmarks(r.Context(), s.writeDB, folder.ID)
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCaptureListAPI] Failed to list capture bookmarks.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to list capture bookmarks")
		return
	}

	// 実ファイルが存在するキャプチャのみを収集する
	folders := captures.UploadFolders(s.config.Capture.UploadFolders)
	files := []captures.File{}
	for _, filename := range filenames {
		path := captures.FindFile(folders, filename)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		files = append(files, captures.File{
			Filename:   filename,
			ModifiedAt: float64(info.ModTime().UnixNano()) / 1e9,
			Path:       path,
		})
	}
	sortCapturesByModifiedAt(files, order == "desc")
	pageFiles := paginateCaptures(files, page)

	items := []captureResponseItem{}
	for _, file := range pageFiles {
		items = append(items, toCaptureResponse(captures.ExtractCaptureInfo(file.Path)))
	}
	writeJSON(w, http.StatusOK, capturesResponse{Total: len(files), Captures: items})
}

// handleCaptureFolderCaptureAdd はフォルダへキャプチャ追加 API
// (POST /api/captures/folders/{folder_id}/captures) を処理する。
func (s *Server) handleCaptureFolderCaptureAdd(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folderID, ok := parsePathInt(w, r, "folder_id")
	if !ok {
		return
	}
	request, ok := decodeCaptureBookmarkRequest(w, r)
	if !ok {
		return
	}
	folder, err := database.GetCaptureFolder(r.Context(), s.writeDB, folderID, user.ID)
	if errors.Is(err, database.ErrCaptureFolderNotFound) {
		writeError(w, http.StatusNotFound, "Capture folder not found")
		return
	}
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCaptureAddAPI] Failed to get the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get the capture folder")
		return
	}
	folders := captures.UploadFolders(s.config.Capture.UploadFolders)
	for _, filename := range request.Filenames {
		// 実ファイルの存在を確認する
		if captures.FindFile(folders, filename) == "" {
			s.logger.Warn("[CapturesRouter][CaptureFolderCaptureAddAPI] Capture file not found, skipping.", "filename", filename)
			continue
		}
		if err := database.AddCaptureBookmark(r.Context(), s.writeDB, folder.ID, filename); err != nil {
			s.logger.Error("[CapturesRouter][CaptureFolderCaptureAddAPI] Failed to add the capture bookmark.", "error", err)
			writeError(w, http.StatusInternalServerError, "Failed to add the capture bookmark")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCaptureFolderCaptureRemove はフォルダからキャプチャ削除 API
// (DELETE /api/captures/folders/{folder_id}/captures) を処理する。
func (s *Server) handleCaptureFolderCaptureRemove(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	folderID, ok := parsePathInt(w, r, "folder_id")
	if !ok {
		return
	}
	request, ok := decodeCaptureBookmarkRequest(w, r)
	if !ok {
		return
	}
	folder, err := database.GetCaptureFolder(r.Context(), s.writeDB, folderID, user.ID)
	if errors.Is(err, database.ErrCaptureFolderNotFound) {
		writeError(w, http.StatusNotFound, "Capture folder not found")
		return
	}
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCaptureRemoveAPI] Failed to get the capture folder.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to get the capture folder")
		return
	}
	if err := database.RemoveCaptureBookmarks(r.Context(), s.writeDB, folder.ID, request.Filenames); err != nil {
		s.logger.Error("[CapturesRouter][CaptureFolderCaptureRemoveAPI] Failed to remove the capture bookmarks.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to remove the capture bookmarks")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCaptureImage はキャプチャ画像取得 API (GET /api/captures/{filename}) を処理する。
// thumbnail=true が指定された場合は、サムネイル用に縮小した JPEG 画像を返す。
func (s *Server) handleCaptureImage(w http.ResponseWriter, r *http.Request) {
	// FastAPI はハンドラ本体より先にクエリパラメータを検証するため、ファイル存在確認 (404) より前に
	// thumbnail の bool 検証を行い、解釈できない値なら検証エラー配列を返す
	// (実機 Python でも GET /api/captures/sample?thumbnail=abc は 404 ではなく 422 になる) 。
	thumbnail := false
	if values, present := r.URL.Query()["thumbnail"]; present {
		raw := lastQueryValue(values)
		parsed, ok := parseFastAPIBool(raw)
		if !ok {
			writeValidationDetails(w, []validationDetail{
				boolParsingDetail([]any{"query", "thumbnail"}, raw),
			})
			return
		}
		thumbnail = parsed
	}

	filename := r.PathValue("filename")
	folders := captures.UploadFolders(s.config.Capture.UploadFolders)
	path := captures.FindFile(folders, filename)
	if path == "" {
		s.logger.Error("[CapturesRouter][CaptureImageAPI] Capture file not found.", "filename", filename)
		writeError(w, http.StatusNotFound, "Capture file not found")
		return
	}

	// サムネイル画像が要求された場合は、リサイズして返す
	if thumbnail {
		data, err := captures.GenerateThumbnail(path)
		if err != nil {
			s.logger.Error("[CapturesRouter][CaptureImageAPI] Failed to generate the thumbnail.", "filename", filename, "error", err)
			writeError(w, http.StatusUnprocessableEntity, "Failed to generate thumbnail")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return
	}

	// 元の画像をそのまま返す
	file, err := os.Open(path)
	if err != nil {
		s.logger.Error("[CapturesRouter][CaptureImageAPI] Failed to read the capture file.", "filename", filename, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Failed to read capture file")
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Failed to read capture file")
		return
	}
	w.Header().Set("Content-Type", captures.MimeTypeForFilename(filename))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// handleCaptureDelete はキャプチャ画像削除 API (DELETE /api/captures/{filename}) を処理する。
func (s *Server) handleCaptureDelete(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	folders := captures.UploadFolders(s.config.Capture.UploadFolders)
	path := captures.FindFile(folders, filename)
	if path == "" {
		s.logger.Error("[CapturesRouter][CaptureDeleteAPI] Capture file not found.", "filename", filename)
		writeError(w, http.StatusNotFound, "Capture file not found")
		return
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrPermission) {
			s.logger.Error("[CapturesRouter][CaptureDeleteAPI] Permission denied to delete the file.", "filename", filename)
			writeError(w, http.StatusUnprocessableEntity, "Permission denied to delete the file")
			return
		}
		s.logger.Error("[CapturesRouter][CaptureDeleteAPI] Failed to delete the file.", "filename", filename, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Unexpected error occurred while deleting the file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCaptureUpload はキャプチャ画像アップロード API (POST /api/captures) を処理する。
// クライアント側でキャプチャした画像をサーバー設定で指定されたフォルダに保存する。
func (s *Server) handleCaptureUpload(w http.ResponseWriter, r *http.Request) {
	// multipart/form-data のリクエストを解析する (最大 64MB)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}
	defer func() { _ = file.Close() }()

	// 画像が JPEG または PNG かをチェックする (magic bytes による判定)
	mimeType, err := detectImageMimeType(file)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}
	if mimeType != "image/jpeg" && mimeType != "image/png" {
		s.logger.Error("[CapturesRouter][CaptureUploadAPI] Invalid image file was uploaded.")
		writeError(w, http.StatusUnprocessableEntity, "Please upload JPEG or PNG image")
		return
	}

	// 保存先フォルダを順に確認する
	uploadFolders := s.config.Capture.UploadFolders
	for index, folder := range uploadFolders {
		info, err := os.Stat(folder)
		if err != nil || !info.IsDir() {
			continue
		}
		// 保存先フォルダの空き容量が 10MB 未満なら、最後のフォルダでなければ次の保存先フォルダを探す
		if free, err := freeDiskSpace(folder); err == nil && free < 10*1024*1024 && index < len(uploadFolders)-1 {
			continue
		}
		filename := filepath.Base(header.Filename)
		if !isSafeCaptureFilename(filename) {
			s.logger.Error("[CapturesRouter][CaptureUploadAPI] Invalid filename was specified.")
			writeError(w, http.StatusUnprocessableEntity, "Specified filename is invalid")
			return
		}
		path := filepath.Join(folder, filename)
		// 既にファイルが存在していた場合は上書きしないようにリネームする
		extension := filepath.Ext(filename)
		stem := strings.TrimSuffix(filename, extension)
		count := 1
		for {
			if _, err := os.Stat(path); err != nil {
				break
			}
			path = filepath.Join(folder, fmt.Sprintf("%s-%d%s", stem, count, extension))
			count++
		}
		if err := saveUploadedFile(path, file); err != nil {
			if isDiskFullError(err) {
				s.logger.Error("[CapturesRouter][CaptureUploadAPI] No space left on the device.")
				writeError(w, http.StatusUnprocessableEntity, "No space left on the device")
				return
			}
			if errors.Is(err, os.ErrPermission) {
				s.logger.Error("[CapturesRouter][CaptureUploadAPI] Permission denied to save the file.")
				writeError(w, http.StatusUnprocessableEntity, "Permission denied to save the file")
				return
			}
			s.logger.Error("[CapturesRouter][CaptureUploadAPI] Failed to save the file.", "error", err)
			writeError(w, http.StatusUnprocessableEntity, "Unexpected error occurred while saving the file")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.logger.Error("[CapturesRouter][CaptureUploadAPI] No available folder to save the file.")
	writeError(w, http.StatusUnprocessableEntity, "No available folder to save the file")
}

// roundToMicrosecond はナノ秒以下を銀行丸めでマイクロ秒に丸める。
// Python の datetime.fromtimestamp() がマイクロ秒精度で丸めるのと同じ挙動にする。
func roundToMicrosecond(value time.Time) time.Time {
	nanoseconds := value.Nanosecond()
	microseconds := nanoseconds / 1000
	remainder := nanoseconds % 1000
	if remainder > 500 || (remainder == 500 && microseconds%2 == 1) {
		microseconds++
	}
	// 丸めによってマイクロ秒が繰り上がった場合も、ナノ秒との差分を引くだけで正しく繰り上がる
	return value.Add(-time.Duration(nanoseconds-microseconds*1000) * time.Nanosecond)
}

// toCaptureFolderResponse はデータベースのフォルダレコードをレスポンス用の構造体に変換する。
func toCaptureFolderResponse(folder database.CaptureFolder, captureCount int) captureFolderResponse {
	return captureFolderResponse{
		ID:           folder.ID,
		Name:         folder.Name,
		SortOrder:    folder.SortOrder,
		CaptureCount: captureCount,
		CreatedAt:    database.FormatJSONTime(folder.CreatedAt),
		UpdatedAt:    database.FormatJSONTime(folder.UpdatedAt),
	}
}

// sortCapturesByModifiedAt はファイルの更新日時でソートする。
func sortCapturesByModifiedAt(files []captures.File, reverse bool) {
	// 安定ソートを使うことで、同じ更新日時のファイルの順序を Python 版 (list.sort) と一致させる
	sort.SliceStable(files, func(left, right int) bool {
		if reverse {
			return files[left].ModifiedAt > files[right].ModifiedAt
		}
		return files[left].ModifiedAt < files[right].ModifiedAt
	})
}

// paginateCaptures はページネーションを適用して現在のページのファイルを返す。
func paginateCaptures(files []captures.File, page int) []captures.File {
	offset := (page - 1) * capturePageSize
	if offset >= len(files) {
		return []captures.File{}
	}
	end := min(offset+capturePageSize, len(files))
	return files[offset:end]
}

// parsePositiveQueryInt はクエリパラメーターから 1 以上の整数を取得する。
// 指定されていない場合は default_ を返す。
//
// Python 側の CapturesRouter は `page: Query(ge=1)` の既定値つきパラメータなので、
// 解析不能は int_parsing、1 未満は greater_than_equal (ctx.ge つき) の検証エラー配列にする
// (クライアントは detail の型で表示を分岐するため、文字列 detail を返してはならない) 。
func parsePositiveQueryInt(w http.ResponseWriter, r *http.Request, name string, default_ int) (int, bool) {
	v := newFastAPIValidation(r)
	lowerBound := 1
	value := v.queryIntRange(name, int64(default_), &lowerBound, nil)
	if v.writeIfInvalid(w) {
		return 0, false
	}
	return int(value), true
}

// parsePathInt はパスパラメーターから整数を取得する。
func parsePathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value := r.PathValue(name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Input should be a valid integer")
		return 0, false
	}
	return parsed, true
}

// parseFastAPIBool は FastAPI/Pydantic v2 の bool 解釈規則でクエリ文字列を真偽値へ変換する。
// 受理する値は true/false を表す次の表現で、大文字小文字は区別しない。
//
//	true:  "true", "1", "yes", "on", "t", "y"
//	false: "false", "0", "no", "off", "f", "n"
//
// それ以外 (空文字を含む) は bool として解釈できないため ok=false を返す。
func parseFastAPIBool(value string) (bool, bool) {
	switch strings.ToLower(value) {
	case "true", "1", "yes", "on", "t", "y":
		return true, true
	case "false", "0", "no", "off", "f", "n":
		return false, true
	}
	return false, false
}

// boolParsingDetail は Pydantic の "bool_parsing" エラーを生成する
// (twitter.go の missingDetail / literalDetail / stringTypeDetail と同じ検証エラー群) 。
func boolParsingDetail(loc []any, input string) validationDetail {
	return validationDetail{
		Type:  "bool_parsing",
		Loc:   loc,
		Msg:   "Input should be a valid boolean, unable to interpret input",
		Input: input,
	}
}

// captureBookmarkRequest はキャプチャブックマーク操作のリクエスト (server/app/schemas.py の CaptureBookmarkRequest 相当) 。
type captureBookmarkRequest struct {
	Filenames []string `json:"filenames"`
}

// decodeCaptureBookmarkRequest はキャプチャブックマーク操作のリクエストを解析する。
func decodeCaptureBookmarkRequest(w http.ResponseWriter, r *http.Request) (captureBookmarkRequest, bool) {
	var request captureBookmarkRequest
	if !decodeCaptureJSONBody(w, r, &request) {
		return request, false
	}
	return request, true
}

// detectImageMimeType はファイルの先頭バイト列から画像の MIME タイプを判定する。
// 移植元: Python 版の puremagic.magic_stream() による判定
func detectImageMimeType(file multipart.File) (string, error) {
	header := make([]byte, 512)
	read, err := file.Read(header)
	if err != nil && err != io.EOF {
		return "", err
	}
	// 読み取り位置を元に戻す (Python 版の image.file.seek(0) に相当)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(header[:read]), nil
}

// saveUploadedFile はアップロードされたファイルを保存する。
func saveUploadedFile(path string, file multipart.File) error {
	output, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	_, err = io.Copy(output, file)
	return err
}

// isSafeCaptureFilename はアップロードされたファイル名が保存先フォルダ内に収まるかを判定する。
func isSafeCaptureFilename(filename string) bool {
	if filename == "" || filename == "." || filename == ".." {
		return false
	}
	if strings.ContainsAny(filename, "/\\:") {
		return false
	}
	return true
}

// decodeCaptureJSONBody はリクエストボディの JSON を解析する。
// 解析に失敗した場合は 422 を返す。
func decodeCaptureJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if !decodeJSONBody(r, target) {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return false
	}
	return true
}
