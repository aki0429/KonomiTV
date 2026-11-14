package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// handleVideoThumbnailUpdate は同じ放送回のキャプチャで代表サムネイルを差し替える。
// 移植元: VideosRouter.VideoThumbnailUpdateAPI()。
func (s *Server) handleVideoThumbnailUpdate(w http.ResponseWriter, r *http.Request) {
	videoID, ok := parsePathID(w, r, "video_id")
	if !ok {
		return
	}
	detail, ok := s.getRecordedProgramDetail(w, r, videoID)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSONBody(r, &body) {
		writeError(w, 422, "Invalid request body")
		return
	}
	raw, found := body["capture_id"]
	if !found {
		writeValidationDetails(w, []validationDetail{missingDetail([]any{"body", "capture_id"}, body)})
		return
	}
	value, err := searchJSONInteger(raw)
	if err != nil {
		validation := err.(searchValidationError)
		writeValidationDetails(w, []validationDetail{{Type: validation.kind, Loc: []any{"body", "capture_id"}, Msg: validation.message, Input: raw}})
		return
	}
	if value.Sign() <= 0 {
		zero := 0
		writeValidationDetails(w, []validationDetail{{Type: "greater_than", Loc: []any{"body", "capture_id"}, Msg: "Input should be greater than 0", Input: raw, Ctx: &validationContext{Gt: &zero}}})
		return
	}
	if detail.Video.ThumbnailInfoJSON == nil {
		writeError(w, 422, "The thumbnails of the recorded program have not been generated yet")
		return
	}
	if !value.IsInt64() {
		writeError(w, 422, "Specified capture_id was not found")
		return
	}
	var digest []byte
	var programStart sql.NullString
	// 番組タイトルでは照合しない。キャプチャ一覧と同じ放送局・番組 ID・開始時刻で照合する。
	err = s.db.QueryRowContext(r.Context(), `SELECT c.file_hash, (SELECT CAST(p.start_time AS TEXT) FROM capture_programs p
		WHERE p.id=c.capture_program_id AND p.network_id IS ? AND p.service_id IS ? AND p.event_id IS ?
		) FROM captures c WHERE c.id=?`,
		detail.Program.NetworkID, detail.Program.ServiceID, detail.Program.EventID,
		value.Int64()).Scan(&digest, &programStart)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 422, "Specified capture_id was not found")
		return
	}
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	start, parseErr := database.ParseDBTime(programStart.String)
	if !programStart.Valid || parseErr != nil || !start.Equal(detail.Program.StartTime) {
		writeError(w, 422, "The capture does not belong to the recorded program")
		return
	}
	files, err := s.captureCatalog.files(r.Context(), value.Int64())
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	var imageData []byte
	for _, file := range files {
		if file.Status != "Available" {
			continue
		}
		data, err := os.ReadFile(file.Path)
		if err != nil {
			continue
		}
		actual := sha256.Sum256(data)
		if bytes.Equal(actual[:], digest) {
			imageData = data
			break
		}
	}
	if imageData == nil {
		writeError(w, 422, "The capture file was not found or is not readable")
		return
	}
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		writeError(w, 422, "The capture file is not a valid image")
		return
	}
	output := filepath.Join(s.paths.ThumbnailsDir, detail.Video.FileHash+".webp")
	if err := s.thumbnailRenderer.SaveRepresentativeFromImage(r.Context(), img, output); err != nil {
		s.logger.Error("[VideoThumbnailUpdateAPI] Failed to write the thumbnail.", "video_id", videoID, "error", err)
		writeError(w, 500, "Failed to save the thumbnail")
		return
	}
	// 内容ハッシュで共有される画像の更新番号を全レコードで進める。タイル情報は保持する。
	_, err = s.writeDB.ExecContext(r.Context(), `UPDATE recorded_videos SET thumbnail_info=json_set(thumbnail_info,
		'$.representative.revision', COALESCE(json_extract(thumbnail_info,'$.representative.revision'),0)+1)
		WHERE file_hash=? AND thumbnail_info IS NOT NULL`, detail.Video.FileHash)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	var info string
	if err := s.db.QueryRowContext(r.Context(), `SELECT thumbnail_info FROM recorded_videos WHERE id=?`, detail.Video.ID).Scan(&info); err != nil {
		s.catalogFailure(w, fmt.Errorf("failed to read updated thumbnail: %w", err))
		return
	}
	writeJSON(w, 200, json.RawMessage(info))
}
