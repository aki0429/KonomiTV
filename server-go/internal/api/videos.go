package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/jikkyo"
)

// videosPageSize はページングで一度に取得する録画番組の数 (Python 版の PAGE_SIZE) 。
const videosPageSize = 30

// jikkyoHTTPClient はニコニコ実況 過去ログ API へアクセスするための HTTP クライアント。
var jikkyoHTTPClient = &http.Client{Timeout: 30 * time.Second}

// recordedProgramsResponse は schemas.RecordedPrograms 互換のレスポンス。
type recordedProgramsResponse struct {
	Total            int64                     `json:"total"`
	RecordedPrograms []recordedProgramResponse `json:"recorded_programs"`
}

// handleVideos は録画番組一覧 API (GET /api/videos) を処理する。
func (s *Server) handleVideos(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	order := query.Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "desc" && order != "asc" && order != "ids" {
		writeError(w, http.StatusUnprocessableEntity, "Invalid order")
		return
	}
	page, ok := parsePositivePage(query.Get("page"))
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "Invalid page")
		return
	}
	ids, ok := parseIDList(query["ids"])
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "Invalid ids")
		return
	}

	response, err := s.listRecordedPrograms(r.Context(), order, page, ids)
	if err != nil {
		s.logger.Error("[VideosAPI] Failed to execute the SQL query.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to execute raw SQL query")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleVideosSearch は録画番組検索 API (GET /api/videos/search) を処理する。
func (s *Server) handleVideosSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	searchQuery := query.Get("query")
	order := query.Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "desc" && order != "asc" {
		writeError(w, http.StatusUnprocessableEntity, "Invalid order")
		return
	}
	page, ok := parsePositivePage(query.Get("page"))
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "Invalid page")
		return
	}

	// クエリが空の場合は全件取得と同じ挙動にする
	if strings.TrimSpace(searchQuery) == "" {
		response, err := s.listRecordedPrograms(r.Context(), order, page, nil)
		if err != nil {
			s.logger.Error("[VideosSearchAPI] Failed to execute the SQL query.", "error", err)
			writeError(w, http.StatusInternalServerError, "Failed to execute raw SQL query")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	condition, params, keywords := buildVideosSearchCondition(searchQuery)
	if len(keywords) == 0 {
		response, err := s.listRecordedPrograms(r.Context(), order, page, nil)
		if err != nil {
			s.logger.Error("[VideosSearchAPI] Failed to execute the SQL query.", "error", err)
			writeError(w, http.StatusInternalServerError, "Failed to execute raw SQL query")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	response, err := s.searchRecordedPrograms(r.Context(), order, page, condition, params)
	if err != nil {
		s.logger.Error("[VideosSearchAPI] Failed to execute the SQL query.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to execute raw SQL query")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// listRecordedPrograms は録画番組の一覧を取得する。
func (s *Server) listRecordedPrograms(
	ctx context.Context,
	order string,
	page int,
	ids []int64,
) (*recordedProgramsResponse, error) {
	// ids が指定されていない場合はすべての録画番組を返す
	targetIDs := ids
	offset := (page - 1) * videosPageSize
	if len(ids) > 0 && order == "ids" {
		// ページングを考慮して必要な範囲の ID のみを使用する
		targetIDs = []int64{}
		start := offset
		if start < len(ids) {
			end := start + videosPageSize
			if end > len(ids) {
				end = len(ids)
			}
			targetIDs = ids[start:end]
		}
		if len(targetIDs) == 0 {
			total, err := database.CountRecordedPrograms(ctx, s.db, ids, "", nil)
			if err != nil {
				return nil, err
			}
			return &recordedProgramsResponse{Total: total, RecordedPrograms: []recordedProgramResponse{}}, nil
		}
	}

	orderDesc := order != "asc"
	options := database.ListRecordedProgramDetailsOptions{
		OrderDesc: orderDesc,
		IDs:       targetIDs,
		Limit:     videosPageSize,
		Offset:    offset,
	}
	if len(ids) > 0 && order == "ids" {
		// 指定された順序を維持するため、OFFSET は 0 固定にする
		options.Offset = 0
	}

	details, err := database.ListRecordedProgramDetails(ctx, s.db, options)
	if err != nil {
		return nil, err
	}
	total, err := database.CountRecordedPrograms(ctx, s.db, ids, "", nil)
	if err != nil {
		return nil, err
	}

	programs := []recordedProgramResponse{}
	for index := range details {
		detail := details[index]
		program, err := buildRecordedProgramResponse(&detail.Program, &detail.Video, detail.Channel)
		if err != nil {
			return nil, err
		}
		if program != nil {
			programs = append(programs, *program)
		}
	}

	// order が 'ids' の場合は、指定された順序を維持する
	if len(ids) > 0 && order == "ids" {
		indexByID := map[int64]int{}
		for index, id := range targetIDs {
			indexByID[id] = index
		}
		sortRecordedProgramsByIDs(programs, indexByID)
	}

	return &recordedProgramsResponse{Total: total, RecordedPrograms: programs}, nil
}

// searchRecordedPrograms は検索条件に一致する録画番組の一覧を取得する。
func (s *Server) searchRecordedPrograms(
	ctx context.Context,
	order string,
	page int,
	condition string,
	params []any,
) (*recordedProgramsResponse, error) {
	details, err := database.ListRecordedProgramDetails(ctx, s.db, database.ListRecordedProgramDetailsOptions{
		OrderDesc:       order != "asc",
		SearchCondition: condition,
		SearchParams:    params,
		Limit:           videosPageSize,
		Offset:          (page - 1) * videosPageSize,
	})
	if err != nil {
		return nil, err
	}
	total, err := database.CountRecordedPrograms(ctx, s.db, nil, condition, params)
	if err != nil {
		return nil, err
	}

	programs := []recordedProgramResponse{}
	for index := range details {
		detail := details[index]
		program, err := buildRecordedProgramResponse(&detail.Program, &detail.Video, detail.Channel)
		if err != nil {
			return nil, err
		}
		if program != nil {
			programs = append(programs, *program)
		}
	}
	return &recordedProgramsResponse{Total: total, RecordedPrograms: programs}, nil
}

// buildVideosSearchCondition は検索キーワードから SQL の WHERE 句とパラメータを生成する。
// 移植元: server/app/routers/VideosRouter.py の VideosSearchAPI() の BuildSearchConditions()
func buildVideosSearchCondition(query string) (string, []any, []string) {
	// 半角・全角スペースで分割する
	keywords := []string{}
	for _, keyword := range strings.Split(strings.ReplaceAll(query, "　", " "), " ") {
		if trimmed := strings.TrimSpace(keyword); trimmed != "" {
			keywords = append(keywords, trimmed)
		}
	}
	if len(keywords) == 0 {
		return "", nil, nil
	}

	// 各キーワードに対する検索条件を生成する
	conditions := []string{}
	params := []any{}
	for _, keyword := range keywords {
		// LIKE 検索用のパラメータを生成 (前後に % を付与)
		param := "%" + strings.ToLower(keyword) + "%"
		conditions = append(conditions, `(
			LOWER(ch.name) LIKE ? OR
			LOWER(rp.title) LIKE ? OR
			LOWER(rp.series_title) LIKE ? OR
			LOWER(rp.subtitle) LIKE ?
		)`)
		params = append(params, param, param, param, param)
	}
	return strings.Join(conditions, " AND "), params, keywords
}

// sortRecordedProgramsByIDs は録画番組の一覧を指定された ID の順序に並び替える。
func sortRecordedProgramsByIDs(programs []recordedProgramResponse, indexByID map[int64]int) {
	for index := 1; index < len(programs); index++ {
		for current := index; current > 0; current-- {
			previous := programs[current-1]
			target := programs[current]
			previousIndex, previousOk := indexByID[previous.ID]
			targetIndex, targetOk := indexByID[target.ID]
			if !previousOk || !targetOk || previousIndex <= targetIndex {
				break
			}
			programs[current-1], programs[current] = programs[current], programs[current-1]
		}
	}
}

// parsePositivePage はページ番号を解析する (1 以上の整数のみ) 。
func parsePositivePage(value string) (int, bool) {
	if value == "" {
		return 1, true
	}
	page, err := strconv.Atoi(value)
	if err != nil || page < 1 {
		return 0, false
	}
	return page, true
}

// parseIDList は録画番組 ID のリストを解析する。
func parseIDList(values []string) ([]int64, bool) {
	// ids=1&ids=2 の形式と、ids=1,2 の形式の両方を受け付ける
	rawValues := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				rawValues = append(rawValues, trimmed)
			}
		}
	}
	if len(rawValues) == 0 {
		return nil, true
	}
	ids := make([]int64, 0, len(rawValues))
	for _, value := range rawValues {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// getRecordedProgramDetail は指定された録画番組を取得する。
// 存在しない場合は 422 を返して false を返す。
func (s *Server) getRecordedProgramDetail(w http.ResponseWriter, r *http.Request, videoID int64) (*database.RecordedProgramDetail, bool) {
	detail, err := database.GetRecordedProgramDetail(r.Context(), s.db, videoID)
	if err != nil {
		s.logger.Error("[VideosRouter] Failed to get the recorded program.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return nil, false
	}
	if detail == nil {
		s.logger.Error("[VideosRouter][GetRecordedProgram] Specified video_id was not found.", "video_id", videoID)
		writeError(w, http.StatusUnprocessableEntity, "Specified video_id was not found")
		return nil, false
	}
	return detail, true
}

// handleVideo は録画番組 API (GET /api/videos/{video_id}) を処理する。
func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	program, err := buildRecordedProgramResponse(&detail.Program, &detail.Video, detail.Channel)
	if err != nil {
		s.logger.Error("[VideoAPI] Failed to build the response.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, program)
}

// handleVideoThumbnail は録画番組サムネイル画像取得 API (GET /api/videos/{video_id}/thumbnail) を処理する。
func (s *Server) handleVideoThumbnail(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	s.writeThumbnailResponse(w, r, detail, false)
}

// handleVideoThumbnailTile はシークバー用サムネイルタイル画像取得 API
// (GET /api/videos/{video_id}/thumbnail/tiled) を処理する。
func (s *Server) handleVideoThumbnailTile(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	s.writeThumbnailResponse(w, r, detail, true)
}

// writeThumbnailResponse はサムネイル画像のレスポンスを生成する。
// 移植元: server/app/routers/VideosRouter.py の GetThumbnailResponse()
func (s *Server) writeThumbnailResponse(
	w http.ResponseWriter,
	r *http.Request,
	detail *database.RecordedProgramDetail,
	returnTiled bool,
) {
	// 録画中のファイルは常にデフォルトのサムネイル画像を返す
	if detail.Video.Status == "Recording" {
		s.writeDefaultThumbnail(w, r)
		return
	}

	// サムネイル画像のパスを生成する (WebP のみを試す)
	suffix := ""
	if returnTiled {
		suffix = "_tile"
	}
	thumbnailPath := filepath.Join(s.paths.ThumbnailsDir, detail.Video.FileHash+suffix+".webp")
	statResult, err := os.Stat(thumbnailPath)
	if err != nil || statResult.IsDir() {
		// サムネイル画像が存在しない場合はデフォルトのサムネイル画像を返す
		s.writeDefaultThumbnail(w, r)
		return
	}

	// FileResponse 相当のヘッダーを設定する (ETag と Last-Modified によるキャッシュ制御)
	etag := fmt.Sprintf(`"%x-%x"`, statResult.ModTime().UnixNano()/int64(time.Millisecond), statResult.Size())
	lastModified := statResult.ModTime().UTC().Format(http.TimeFormat)
	w.Header().Set("Cache-Control", "public, no-transform, immutable, max-age=2592000")
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastModified)

	// If-None-Match と If-Modified-Since の検証 (FileResponse が実装していない 304 判定)
	if isContentNotModified(r, etag, statResult.ModTime()) {
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	http.ServeFile(w, r, thumbnailPath)
}

// writeDefaultThumbnail はデフォルトのサムネイル画像を返す (キャッシュさせない) 。
func (s *Server) writeDefaultThumbnail(w http.ResponseWriter, r *http.Request) {
	defaultThumbnailPath := filepath.Join(s.paths.StaticDir, "thumbnails", "default.webp")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "image/webp")
	http.ServeFile(w, r, defaultThumbnailPath)
}

// isContentNotModified はリクエストヘッダーから 304 を返すべきかどうかを判定する。
// 移植元: server/app/routers/VideosRouter.py の IsContentNotModified()
func isContentNotModified(r *http.Request, etag string, lastModified time.Time) bool {
	// If-None-Match による判定 (優先度が最も高い)
	ifNoneMatch := r.Header.Get("If-None-Match")
	if ifNoneMatch != "" {
		requestTags := parseIfNoneMatch(ifNoneMatch)
		if _, ok := requestTags["*"]; ok {
			// * はリソースが存在するなら常に 304 を返す
			return etag != ""
		}
		if etag != "" {
			normalizedETag := strings.Trim(etag, `"`)
			if _, ok := requestTags[normalizedETag]; ok {
				return true
			}
			if _, ok := requestTags[etag]; ok {
				return true
			}
		}
		// If-None-Match が存在する場合は If-Modified-Since を無視する (RFC 準拠)
		return false
	}

	// If-Modified-Since による判定 (If-None-Match が無い場合のみ評価)
	ifModifiedSince := r.Header.Get("If-Modified-Since")
	if ifModifiedSince == "" {
		return false
	}
	parsedIfModifiedSince, err := http.ParseTime(ifModifiedSince)
	if err != nil {
		return false
	}
	// HTTP の日時は秒単位なので、比較も秒単位で行う
	return !parsedIfModifiedSince.Before(lastModified.Truncate(time.Second))
}

// parseIfNoneMatch は If-None-Match ヘッダーを RFC 準拠の形で解析してタグ集合に変換する。
func parseIfNoneMatch(headerValue string) map[string]struct{} {
	tags := map[string]struct{}{}
	for _, rawTag := range strings.Split(headerValue, ",") {
		tag := strings.TrimSpace(rawTag)
		if tag == "" {
			continue
		}
		if tag == "*" {
			tags["*"] = struct{}{}
			continue
		}
		tag = strings.TrimPrefix(tag, "W/")
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
			tag = tag[1 : len(tag)-1]
		}
		tags[tag] = struct{}{}
	}
	return tags
}

// handleVideoDownload は録画ファイルダウンロード API (GET /api/videos/{video_id}/download) を処理する。
func (s *Server) handleVideoDownload(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}

	// 録画中にダウンロードすると途中まで録画されたファイルがダウンロードされる可能性があるため、
	// 明示的に録画完了状態のファイルのみを対象とする
	if detail.Video.Status == "Recording" {
		s.logger.Error("[VideosRouter][VideoDownloadAPI] Recorded video is recording.", "video_id", videoID)
		writeError(w, http.StatusUnprocessableEntity, "The recorded video is recording")
		return
	}

	filePath := detail.Video.FilePath
	// 万が一ファイルが存在しない場合は明示的に 404 エラーを返す
	statResult, err := os.Stat(filePath)
	if err != nil || statResult.IsDir() {
		s.logger.Error("[VideosRouter][VideoDownloadAPI] Recorded file was not found.", "video_id", videoID)
		writeError(w, http.StatusNotFound, "The recorded file was not found")
		return
	}

	// 動画コンテナに合わせて MIME タイプを設定する
	mediaType := "application/octet-stream"
	switch detail.Video.ContainerFormat {
	case "MPEG-TS":
		mediaType = "video/mp2t"
	case "MPEG-4":
		mediaType = "video/mp4"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", contentDispositionAttachment(filepath.Base(filePath)))
	// ブラウザ側で MPEG-2 映像をリアルタイム変換して再生する際にも使われるため、
	// シークできるように Range リクエストに対応させる (http.ServeFile が処理する) 。
	http.ServeFile(w, r, filePath)
}

// contentDispositionAttachment は Content-Disposition ヘッダーの値を生成する。
// 移植元: Starlette の FileResponse (ASCII の場合は filename、非 ASCII の場合は filename*=utf-8”...) 。
func contentDispositionAttachment(filename string) string {
	// ASCII のみの場合はそのまま使える
	ascii := true
	for _, character := range filename {
		if character > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, `\"`))
	}
	// 非 ASCII 文字を含む場合は filename* 形式で出力する
	encoded := ""
	for _, byteValue := range []byte(filename) {
		if (byteValue >= 'a' && byteValue <= 'z') || (byteValue >= 'A' && byteValue <= 'Z') ||
			(byteValue >= '0' && byteValue <= '9') || strings.ContainsRune("-_.~/!$&'()*+,;=:@", rune(byteValue)) {
			encoded += string(byteValue)
			continue
		}
		encoded += fmt.Sprintf("%%%02X", byteValue)
	}
	return "attachment; filename*=utf-8''" + encoded
}

// handleVideoJikkyo は録画番組過去ログコメント API (GET /api/videos/{video_id}/jikkyo) を処理する。
func (s *Server) handleVideoJikkyo(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}

	// チャンネル情報と録画開始時刻/録画終了時刻の情報がある場合のみ取得する
	if detail.Channel == nil || detail.Video.RecordingStartTime == nil || detail.Video.RecordingEndTime == nil {
		writeJSON(w, http.StatusOK, &jikkyo.Comments{
			IsSuccess: false,
			Comments:  []jikkyo.Comment{},
			Detail:    "チャンネル情報または録画開始時刻/録画終了時刻の情報がない録画番組です。",
		})
		return
	}

	// 実況チャンネル ID を解決する
	jikkyoID, _, found := s.jikkyoChannelMap().Resolve(detail.Channel.NetworkID, detail.Channel.ServiceID)
	if !found {
		writeJSON(w, http.StatusOK, &jikkyo.Comments{
			IsSuccess: false,
			Comments:  []jikkyo.Comment{},
			Detail:    "このチャンネルに対応するニコニコ実況のチャンネルが見つかりませんでした。",
		})
		return
	}

	comments, err := jikkyo.FetchJikkyoComments(jikkyoHTTPClient, jikkyoID, *detail.Video.RecordingStartTime, *detail.Video.RecordingEndTime)
	if err != nil {
		s.logger.Error("[VideoJikkyoCommentsAPI] Failed to fetch the jikkyo comments.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to fetch the jikkyo comments")
		return
	}
	writeJSON(w, http.StatusOK, comments)
}

// handleVideoDelete は録画ファイル削除 API (DELETE /api/videos/{video_id}) を処理する。
func (s *Server) handleVideoDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}

	filePath := detail.Video.FilePath
	fileHash := detail.Video.FileHash
	fileDirectory := filepath.Dir(filePath)
	fileName := filepath.Base(filePath)

	// 万が一処理が失敗してもダメージが比較的少ない順に実行する
	// 1. データベースから録画番組情報・録画ファイル情報を削除 (recorded_videos は CASCADE で削除される)
	duplicateCount, err := database.CountRecordedProgramsWithFileHash(r.Context(), s.db, fileHash, videoID)
	if err != nil {
		s.logger.Error("[VideoDeleteAPI] Failed to count the duplicate records.", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete recorded program: %v", err))
		return
	}
	if err := database.DeleteRecordedProgram(r.Context(), s.writeDB, videoID); err != nil {
		s.logger.Error("[VideoDeleteAPI] Failed to delete recorded program from database.", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete recorded program from database: %v", err))
		return
	}

	// 2. サムネイルファイルの削除 (同じ file_hash を持つ他のレコードが存在する場合はスキップ)
	if duplicateCount == 0 {
		thumbnailsDirectory := s.paths.ThumbnailsDir
		if statResult, err := os.Stat(thumbnailsDirectory); err == nil && statResult.IsDir() {
			for _, suffix := range []string{"", "_tile"} {
				for _, extension := range []string{".webp", ".jpg"} {
					thumbnailPath := filepath.Join(thumbnailsDirectory, fileHash+suffix+extension)
					if _, err := os.Stat(thumbnailPath); err == nil {
						if err := os.Remove(thumbnailPath); err != nil {
							s.logger.Error("[VideoDeleteAPI] Failed to delete thumbnail file.", "path", thumbnailPath, "error", err)
							writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete thumbnail file: %v", err))
							return
						}
					} else if extension == ".webp" {
						// JPEG はよほど長尺でない限り発生しないので WebP のみ警告する
						s.logger.Warn("[VideoDeleteAPI] Thumbnail file does not exist.", "path", thumbnailPath)
					}
				}
			}
		}
	} else {
		s.logger.Info("[VideoDeleteAPI] Skip deleting thumbnail files because other records with the same file_hash exist.", "file_hash", fileHash)
	}

	// 3. 関連する補助ファイルの削除 (.ts.program.txt, .ts.err)
	for _, auxiliaryPath := range []string{
		filepath.Join(fileDirectory, fileName+".program.txt"),
		filepath.Join(fileDirectory, fileName+".err"),
	} {
		if _, err := os.Stat(auxiliaryPath); err != nil {
			continue
		}
		if err := os.Remove(auxiliaryPath); err != nil {
			s.logger.Error("[VideoDeleteAPI] Failed to delete the auxiliary file.", "path", auxiliaryPath, "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete the auxiliary file: %v", err))
			return
		}
	}

	// 4. 録画ファイル本体の削除
	if _, err := os.Stat(filePath); err == nil {
		if err := os.Remove(filePath); err != nil {
			// 録画ファイル本体の削除に失敗した場合はクリティカルなエラー
			s.logger.Error("[VideoDeleteAPI] Failed to delete recorded video file.", "path", filePath, "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to delete recorded video file: %v", err))
			return
		}
		s.logger.Info("[VideoDeleteAPI] Successfully deleted recorded video file.", "path", filePath)
	} else {
		s.logger.Warn("[VideoDeleteAPI] Recorded video file does not exist.", "path", filePath)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleVideoReanalyze は録画番組メタデータ再解析 API (POST /api/videos/{video_id}/reanalyze) を処理する。
// Go 版では未実装のため、Python 版へプロキシする。
func (s *Server) handleVideoReanalyze(w http.ResponseWriter, r *http.Request) {
	s.proxyRequest(w, r)
}

// handleVideoThumbnailRegenerate はサムネイル画像再生成 API
// (POST /api/videos/{video_id}/thumbnail/regenerate) を処理する。
// Go 版では未実装のため、Python 版へプロキシする。
func (s *Server) handleVideoThumbnailRegenerate(w http.ResponseWriter, r *http.Request) {
	s.proxyRequest(w, r)
}

// parsePathID はパスパラメータの ID を解析する (不正な場合は 422 を返す) 。
// 本文は FastAPI/Pydantic の検証エラー配列に合わせる (移植元: Path(int) の int_parsing) 。
// クライアント (services/APIClient.ts) は detail が配列か文字列かで表示を分岐するため、
// 文字列の {"detail":"Invalid video_id"} では互換にならない。
func parsePathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value := r.PathValue(name)
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		writeValidationDetails(w, []validationDetail{{
			Type:  "int_parsing",
			Loc:   []string{"path", name},
			Msg:   "Input should be a valid integer, unable to parse string as an integer",
			Input: value,
		}})
		return 0, false
	}
	return id, true
}

// proxyRequest はリクエストを Python 版サーバーへプロキシする。
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.proxy.ServeHTTP(w, r)
}
