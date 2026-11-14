package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// captureTagResponse は本家 schemas.CaptureTag の応答。
type captureTagResponse struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	CaptureCount int64  `json:"capture_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// registerCaptureTagRoutes は本人だけが操作できるタグ・お気に入りを登録する。
func (s *Server) registerCaptureTagRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/captures/tags", s.handleCaptureTags)
	mux.HandleFunc("POST /api/captures/tags", s.handleCaptureTagWrite)
	mux.HandleFunc("PUT /api/captures/tags/{tag_id}", s.handleCaptureTagWrite)
	mux.HandleFunc("DELETE /api/captures/tags/{tag_id}", s.handleCaptureTagDelete)
	mux.HandleFunc("POST /api/captures/favorites", s.handleCapturePersonalItems)
	mux.HandleFunc("DELETE /api/captures/favorites", s.handleCapturePersonalItems)
	mux.HandleFunc("POST /api/captures/tags/{tag_id}/items", s.handleCapturePersonalItems)
	mux.HandleFunc("DELETE /api/captures/tags/{tag_id}/items", s.handleCapturePersonalItems)
}

const captureTagQuery = `SELECT t.id,t.name,t.created_at,t.updated_at,
	(SELECT COUNT(*) FROM capture_tag_items ti WHERE ti.tag_id=t.id AND EXISTS
	 (SELECT 1 FROM capture_files f WHERE f.capture_id=ti.capture_id AND f.status='Available'))
	FROM capture_tags t WHERE t.user_id=?`

// scanCaptureTag は日時を本家と同じ JST の ISO8601 表現へ変換する。
func scanCaptureTag(scan func(...any) error) (*captureTagResponse, error) {
	var response captureTagResponse
	var created, updated string
	if err := scan(&response.ID, &response.Name, &created, &updated, &response.CaptureCount); err != nil {
		return nil, err
	}
	createdTime, err := database.ParseDBTime(created)
	if err != nil {
		return nil, err
	}
	updatedTime, err := database.ParseDBTime(updated)
	if err != nil {
		return nil, err
	}
	response.CreatedAt = database.FormatJSONTime(createdTime)
	response.UpdatedAt = database.FormatJSONTime(updatedTime)
	return &response, nil
}

func (s *Server) handleCaptureTags(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), captureTagQuery+` ORDER BY t.name`, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	defer func() { _ = rows.Close() }()
	result := []captureTagResponse{}
	for rows.Next() {
		tag, err := scanCaptureTag(rows.Scan)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		result = append(result, *tag)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, result)
}

// ownedCaptureTag は存在しないタグと他人のタグを同じ 404 にする。
func (s *Server) ownedCaptureTag(w http.ResponseWriter, r *http.Request, userID int64) (int64, bool) {
	validation := newFastAPIValidation(r)
	id, _ := validation.pathInt(r, "tag_id")
	if validation.writeIfInvalid(w) {
		return 0, false
	}
	var found int64
	err := s.db.QueryRowContext(r.Context(), `SELECT id FROM capture_tags WHERE id=? AND user_id=?`, id, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Capture tag not found")
		return 0, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return 0, false
	}
	return found, true
}

func (s *Server) handleCaptureTagWrite(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name *string `json:"name"`
	}
	if !decodeJSONBody(r, &body) || body.Name == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	name := strings.TrimSpace(*body.Name)
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 255 {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	var id int64
	now := database.NowForDB()
	var err error
	status := http.StatusOK
	if r.Method == http.MethodPost {
		var result sql.Result
		result, err = s.writeDB.ExecContext(r.Context(), `INSERT INTO capture_tags(user_id,name,created_at,updated_at) VALUES(?,?,?,?)`, user.ID, name, now, now)
		if err == nil {
			id, err = result.LastInsertId()
		}
		status = http.StatusCreated
	} else {
		id, ok = s.ownedCaptureTag(w, r, user.ID)
		if !ok {
			return
		}
		_, err = s.writeDB.ExecContext(r.Context(), `UPDATE capture_tags SET name=?,updated_at=? WHERE id=? AND user_id=?`, name, now, id, user.ID)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusUnprocessableEntity, "Capture tag name already exists")
		} else {
			writeError(w, http.StatusInternalServerError, "Internal Server Error")
		}
		return
	}
	tag, err := scanCaptureTag(s.db.QueryRowContext(r.Context(), captureTagQuery+` AND t.id=?`, user.ID, id).Scan)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, status, tag)
}

func (s *Server) handleCaptureTagDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	id, ok := s.ownedCaptureTag(w, r, user.ID)
	if !ok {
		return
	}
	if _, err := s.writeDB.ExecContext(r.Context(), `DELETE FROM capture_tags WHERE id=? AND user_id=?`, id, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mutateCapturePersonalItems は全 ID の検証と一括変更を同一トランザクションで行う。
func mutateCapturePersonalItems(ctx context.Context, db *sql.DB, userID int64, tagID *int64, ids []int64, add bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if add {
		for _, id := range ids {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM captures WHERE id=?`, id).Scan(&found); err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		if tagID == nil {
			if add {
				_, err = tx.ExecContext(ctx, `INSERT INTO capture_favorites(user_id,capture_id,created_at) VALUES(?,?,?) ON CONFLICT(user_id,capture_id) DO NOTHING`, userID, id, database.NowForDB())
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM capture_favorites WHERE user_id=? AND capture_id=?`, userID, id)
			}
		} else if add {
			_, err = tx.ExecContext(ctx, `INSERT INTO capture_tag_items(tag_id,capture_id,created_at) VALUES(?,?,?) ON CONFLICT(tag_id,capture_id) DO NOTHING`, *tagID, id, database.NowForDB())
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM capture_tag_items WHERE tag_id=? AND capture_id=?`, *tagID, id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) handleCapturePersonalItems(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireCurrentUser(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs *[]json.RawMessage `json:"capture_ids"`
	}
	if !decodeJSONBody(r, &body) || body.IDs == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	var tagID *int64
	if r.PathValue("tag_id") != "" {
		id, found := s.ownedCaptureTag(w, r, user.ID)
		ok = found
		if !ok {
			return
		}
		tagID = &id
	}
	unique := map[int64]bool{}
	ids := []int64{}
	for _, raw := range *body.IDs {
		value, err := searchJSONInteger(raw)
		if err != nil || !value.IsInt64() {
			writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
			return
		}
		id := value.Int64()
		if !unique[id] {
			unique[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	err := mutateCapturePersonalItems(r.Context(), s.writeDB, user.ID, tagID, ids, r.Method == http.MethodPost)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "Capture not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
