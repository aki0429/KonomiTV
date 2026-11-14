package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

func (s *Server) registerCaptureCatalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/captures/sections", s.handleCatalogSections)
	mux.HandleFunc("GET /api/captures/channels", s.handleCatalogChannels)
	mux.HandleFunc("GET /api/captures/programs", s.handleCatalogPrograms)
	mux.HandleFunc("GET /api/captures/programs/{capture_program_id}", s.handleCatalogProgram)
	// 番組アルバムの経路との交差を避け、画像アクションだけをここで受け付ける。
	mux.HandleFunc("GET /api/captures/{filename}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("action") != "image" {
			http.NotFound(w, r)
			return
		}
		s.handleCatalogImage(w, r)
	})
	mux.HandleFunc("DELETE /api/captures", s.handleCatalogDelete)
}

func (s *Server) catalogFailure(w http.ResponseWriter, err error) {
	s.logger.Error("Capture catalog request failed.", "error", err)
	writeError(w, 500, "Internal Server Error")
}

// handleCatalogCaptures は同時刻の画像を ID で区別してページ送りする。
func (s *Server) handleCatalogCaptures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	v := newFastAPIValidation(r)
	filters := parseCatalogFilters(v)
	order := v.queryLiteral("order", []string{"CapturedAtDesc", "CapturedAtAsc", "BroadcastAtDesc", "BroadcastAtAsc", "PlaybackPosition"}, "CapturedAtDesc", "'CapturedAtDesc', 'CapturedAtAsc', 'BroadcastAtDesc', 'BroadcastAtAsc' or 'PlaybackPosition'")
	limit := iptvQueryInt(v, "limit", 100, 1, 200)
	date := catalogQueryDate(v, "date")
	hour := iptvQueryInt(v, "day_start_hour", 0, 0, 23)
	if v.writeIfInvalid(w) {
		return
	}
	user := s.optionalCurrentUser(r)
	axis := "captured_at"
	if strings.HasPrefix(order, "BroadcastAt") {
		axis = "broadcast_at"
	}
	conditions, args, ok := s.catalogWhere(w, r, filters, user, axis, hour)
	if !ok {
		return
	}
	if axis == "broadcast_at" {
		conditions = append(conditions, "c.broadcast_at IS NOT NULL")
	}
	if date != nil {
		start, _ := time.ParseInLocation("2006-01-02", *date, constants.JST)
		start = start.Add(time.Duration(hour) * time.Hour)
		conditions = append(conditions, "c."+axis+">=? AND c."+axis+"<?")
		args = append(args, database.FormatDBTime(start), database.FormatDBTime(start.AddDate(0, 0, 1)))
	}
	sortExpressions := []string{"c." + axis, "c.id"}
	if order == "PlaybackPosition" {
		sortExpressions = []string{"(c.playback_position IS NULL)", "COALESCE(c.playback_position,0)", "c.captured_at", "c.id"}
	}
	var userID any
	if user != nil {
		userID = user.ID
	}
	signature := catalogSignature([]any{order, date, hour, filters, userID})
	direction, comparison := "ASC", ">"
	if strings.HasSuffix(order, "Desc") {
		direction, comparison = "DESC", "<"
	}
	if v.query.Has("cursor") {
		keys, valid := catalogDecodeCursor(v.queryString("cursor"), signature, len(sortExpressions))
		if !valid {
			writeError(w, 422, "Invalid cursor")
			return
		}
		conditions = append(conditions, "("+strings.Join(sortExpressions, ",")+")"+comparison+"("+catalogPlaceholders(len(keys))+")")
		args = append(args, keys...)
	}
	sortParts := make([]string, len(sortExpressions))
	for i, value := range sortExpressions {
		sortParts[i] = value + " " + direction
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+catalogCaptureColumns+` FROM captures c LEFT JOIN capture_programs p ON p.id=c.capture_program_id WHERE `+strings.Join(conditions, " AND ")+` ORDER BY `+strings.Join(sortParts, ",")+` LIMIT ?`, args...)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	captures := []catalogCapture{}
	for rows.Next() {
		var c catalogCapture
		c, err = scanCatalogCapture(rows.Scan)
		if err != nil {
			break
		}
		captures = append(captures, c)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	var next *string
	if len(captures) > limit {
		captures = captures[:limit]
		last := captures[len(captures)-1]
		if order == "PlaybackPosition" {
			missing := 0
			position := 0.0
			if last.Position == nil {
				missing = 1
			} else {
				position = *last.Position
			}
			next = catalogEncodeCursor(signature, []any{missing, position, last.rawCaptured, last.ID})
		} else {
			value := last.rawCaptured
			if axis == "broadcast_at" {
				value = last.rawBroadcast.String
			}
			next = catalogEncodeCursor(signature, []any{value, last.ID})
		}
	}
	if err = s.personalize(r.Context(), captures, user); err != nil {
		s.catalogFailure(w, err)
		return
	}
	programs, err := s.catalogPrograms(r.Context(), catalogProgramIDs(captures))
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Captures []catalogCapture `json:"captures"`
		Programs []catalogProgram `json:"programs"`
		Next     *string          `json:"next_cursor"`
	}{captures, programs, next})
}

type catalogSection struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}
type catalogSectionsResponse struct {
	Sections []catalogSection `json:"sections"`
	Total    int64            `json:"total"`
}
type catalogCachedSections struct {
	generation uint64
	result     catalogSectionsResponse
}

func (s *Server) handleCatalogSections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	v := newFastAPIValidation(r)
	filters := parseCatalogFilters(v)
	axis := v.queryLiteral("axis", []string{"CapturedAt", "BroadcastAt"}, "CapturedAt", "'CapturedAt' or 'BroadcastAt'")
	hour := iptvQueryInt(v, "day_start_hour", 0, 0, 23)
	if v.writeIfInvalid(w) {
		return
	}
	// 絞り込みのない全体集計だけを保持し、登録・欠落・削除があれば世代番号で無効化する。
	canCache := true
	for _, value := range filters {
		if value != nil {
			canCache = false
			break
		}
	}
	catalog := s.captureCatalog
	generation := catalog.sectionsGeneration.Load()
	cacheKey := axis + ":" + strconv.Itoa(hour)
	if canCache {
		catalog.sectionsMu.Lock()
		cached, ok := catalog.sectionsCache[cacheKey]
		catalog.sectionsMu.Unlock()
		if ok && cached.generation == generation {
			writeJSON(w, 200, cached.result)
			return
		}
	}
	column := "captured_at"
	if axis == "BroadcastAt" {
		column = "broadcast_at"
	}
	conditions, args, ok := s.catalogWhere(w, r, filters, s.optionalCurrentUser(r), column, hour)
	if !ok {
		return
	}
	conditions = append(conditions, "c."+column+" IS NOT NULL")
	expression := `date(c.` + column + `,'+9 hours','-` + strconv.Itoa(hour) + ` hours')`
	join := ""
	for _, condition := range conditions {
		if strings.Contains(condition, "p.") {
			join = ` LEFT JOIN capture_programs p ON p.id=c.capture_program_id`
			break
		}
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+expression+`,COUNT(*) FROM captures c`+join+` WHERE `+strings.Join(conditions, " AND ")+` GROUP BY `+expression+` ORDER BY 1 DESC`, args...)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	defer func() { _ = rows.Close() }()
	sections := []catalogSection{}
	var total int64
	for rows.Next() {
		var value catalogSection
		if err = rows.Scan(&value.Date, &value.Count); err != nil {
			s.catalogFailure(w, err)
			return
		}
		sections = append(sections, value)
		total += value.Count
	}
	if err = rows.Err(); err != nil {
		s.catalogFailure(w, err)
		return
	}
	result := catalogSectionsResponse{sections, total}
	if canCache {
		catalog.sectionsMu.Lock()
		if catalog.sectionsGeneration.Load() == generation {
			if catalog.sectionsCache == nil {
				catalog.sectionsCache = map[string]catalogCachedSections{}
			}
			catalog.sectionsCache[cacheKey] = catalogCachedSections{generation, result}
		}
		catalog.sectionsMu.Unlock()
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleCatalogChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT ch.id,ch.display_channel_id,ch.name,ch.type FROM channels ch WHERE EXISTS(SELECT 1 FROM capture_programs p WHERE p.network_id=ch.network_id AND p.service_id=ch.service_id AND EXISTS(SELECT 1 FROM captures c WHERE c.capture_program_id=p.id AND `+catalogAvailable+`)) ORDER BY ch.remocon_id,ch.channel_number,ch.id`)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	defer func() { _ = rows.Close() }()
	channels := []catalogChannel{}
	for rows.Next() {
		var ch catalogChannel
		if err = rows.Scan(&ch.ID, &ch.DisplayID, &ch.Name, &ch.Type); err != nil {
			s.catalogFailure(w, err)
			return
		}
		channels = append(channels, ch)
	}
	if err = rows.Err(); err != nil {
		s.catalogFailure(w, err)
		return
	}
	writeJSON(w, 200, channels)
}

func (s *Server) handleCatalogPrograms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	v := newFastAPIValidation(r)
	limit := iptvQueryInt(v, "limit", 60, 1, 200)
	recorded := catalogQueryBool(v, "has_recorded_program")
	order := v.queryLiteral("order", []string{"BroadcastAtDesc", "BroadcastAtAsc"}, "BroadcastAtDesc", "'BroadcastAtDesc' or 'BroadcastAtAsc'")
	if v.writeIfInvalid(w) {
		return
	}
	user := s.optionalCurrentUser(r)
	var userID any
	if user != nil {
		userID = user.ID
	}
	var query any
	if v.query.Has("query") {
		query = v.queryString("query")
	}
	signature := catalogSignature([]any{"Programs", query, recorded, order, userID})
	conditions := []string{`EXISTS(SELECT 1 FROM captures c WHERE c.capture_program_id=p.id AND ` + catalogAvailable + `)`}
	args := []any{}
	if value, ok := query.(string); ok {
		for _, keyword := range strings.Fields(value) {
			conditions = append(conditions, `(LOWER(p.title) LIKE ? ESCAPE '\' OR EXISTS(SELECT 1 FROM channels ch WHERE ch.network_id=p.network_id AND ch.service_id=p.service_id AND LOWER(ch.name) LIKE ? ESCAPE '\'))`)
			pattern := catalogPattern(keyword)
			args = append(args, pattern, pattern)
		}
	}
	if recorded != nil {
		prefix := ""
		if !*recorded {
			prefix = "NOT "
		}
		conditions = append(conditions, prefix+`EXISTS(SELECT 1 FROM recorded_programs r JOIN recorded_videos rv ON rv.recorded_program_id=r.id WHERE r.network_id=p.network_id AND r.service_id=p.service_id AND r.event_id=p.event_id AND r.start_time=p.start_time AND rv.status!='Missing')`)
	}
	direction, comparison := "ASC", ">"
	if strings.HasSuffix(order, "Desc") {
		direction, comparison = "DESC", "<"
	}
	if v.query.Has("cursor") {
		keys, ok := catalogDecodeCursor(v.queryString("cursor"), signature, 2)
		if !ok {
			writeError(w, 422, "Invalid cursor")
			return
		}
		conditions = append(conditions, `(p.start_time,p.id)`+comparison+`(?,?)`)
		args = append(args, keys...)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(r.Context(), `SELECT p.id,CAST(p.start_time AS TEXT) FROM capture_programs p WHERE `+strings.Join(conditions, " AND ")+` ORDER BY p.start_time `+direction+`,p.id `+direction+` LIMIT ?`, args...)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	ids := []int64{}
	starts := []string{}
	for rows.Next() {
		var id int64
		var start string
		if err = rows.Scan(&id, &start); err != nil {
			break
		}
		ids = append(ids, id)
		starts = append(starts, start)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	var next *string
	if len(ids) > limit {
		next = catalogEncodeCursor(signature, []any{starts[limit-1], ids[limit-1]})
		ids = ids[:limit]
	}
	albums, err := s.catalogAlbums(r.Context(), ids, user)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Programs []catalogAlbum `json:"programs"`
		Next     *string        `json:"next_cursor"`
	}{albums, next})
}

func (s *Server) handleCatalogProgram(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	v := newFastAPIValidation(r)
	id, _ := v.pathInt(r, "capture_program_id")
	if v.writeIfInvalid(w) {
		return
	}
	albums, err := s.catalogAlbums(r.Context(), []int64{id}, s.optionalCurrentUser(r))
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	if len(albums) == 0 {
		writeError(w, 404, "Capture program not found")
		return
	}
	writeJSON(w, 200, albums[0])
}

func (s *Server) handleCatalogDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	v := newFastAPIValidation(r)
	r.SetPathValue("capture_id", r.PathValue("filename"))
	id, _ := v.pathInt(r, "capture_id")
	if v.writeIfInvalid(w) {
		return
	}
	capture, err := scanCatalogCapture(s.db.QueryRowContext(r.Context(), `SELECT `+catalogCaptureColumns+` FROM captures c WHERE c.id=?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Capture not found")
		return
	}
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	captures := []catalogCapture{capture}
	if err = s.personalize(r.Context(), captures, s.optionalCurrentUser(r)); err != nil {
		s.catalogFailure(w, err)
		return
	}
	var program *catalogProgram
	if capture.ProgramID != nil {
		programs, err := s.catalogPrograms(r.Context(), []int64{*capture.ProgramID})
		if err != nil {
			s.catalogFailure(w, err)
			return
		}
		if len(programs) > 0 {
			program = &programs[0]
		}
	}
	files, err := s.captureCatalog.files(r.Context(), id)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	type location struct {
		Name   string `json:"file_name"`
		Path   string `json:"file_path"`
		Status string `json:"status"`
	}
	locations := []location{}
	for _, file := range files {
		locations = append(locations, location{file.Name, file.Path, file.Status})
	}
	writeJSON(w, 200, struct {
		catalogCapture
		Program *catalogProgram `json:"program"`
		Files   []location      `json:"files"`
	}{captures[0], program, locations})
}

var catalogHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) handleCatalogImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	v := newFastAPIValidation(r)
	r.SetPathValue("capture_id", r.PathValue("filename"))
	id, _ := v.pathInt(r, "capture_id")
	hash := v.queryString("hash")
	if !v.query.Has("hash") {
		v.details = append(v.details, validationDetail{Type: "missing", Loc: []any{"query", "hash"}, Msg: "Field required"})
	} else if !catalogHashPattern.MatchString(hash) {
		v.details = append(v.details, validationDetail{Type: "string_pattern_mismatch", Loc: []any{"query", "hash"}, Msg: "String should match pattern '^[0-9a-f]{64}$'", Input: hash})
	}
	if v.writeIfInvalid(w) {
		return
	}
	data, err := s.captureCatalog.image(r.Context(), id, hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Capture image not found")
		return
	}
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	mime := "image/jpeg"
	if bytes.HasPrefix(data, []byte{137, 80, 78, 71}) {
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+hash+`"`)
	_, _ = w.Write(data)
}

func (s *Server) handleCatalogDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}
	var body struct {
		IDs *[]json.RawMessage `json:"capture_ids"`
	}
	if !decodeJSONBody(r, &body) || body.IDs == nil {
		writeError(w, 422, "Invalid request body")
		return
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, raw := range *body.IDs {
		value, err := searchJSONInteger(raw)
		if err != nil || !value.IsInt64() {
			writeError(w, 422, "Invalid request body")
			return
		}
		id := value.Int64()
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	deleted, failed := []int64{}, []int64{}
	for _, id := range ids {
		ok, err := s.captureCatalog.delete(r.Context(), id)
		if err != nil {
			s.logger.Warn("Failed to delete capture.", "id", id, "error", err)
		}
		if ok && err == nil {
			deleted = append(deleted, id)
		} else {
			failed = append(failed, id)
		}
	}
	writeJSON(w, 200, struct {
		Deleted []int64 `json:"deleted_capture_ids"`
		Failed  []int64 `json:"failed_capture_ids"`
	}{deleted, failed})
}
