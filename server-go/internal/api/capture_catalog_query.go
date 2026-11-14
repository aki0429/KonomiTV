package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

type catalogFilters map[string]any

func catalogQueryBool(v *fastapiValidation, name string) *bool {
	if !v.query.Has(name) {
		return nil
	}
	value := iptvQueryBool(v, name)
	return &value
}

// catalogQueryDate は Pydantic の日付入力を検証し、カーソルには YYYY-MM-DD を使う。
func catalogQueryDate(v *fastapiValidation, name string) *string {
	if !v.query.Has(name) {
		return nil
	}
	raw := v.queryString(name)
	appendParsing := func(kind, prefix, reason string) {
		v.details = append(v.details, validationDetail{Type: kind, Loc: []any{"query", name}, Msg: prefix + reason, Input: raw, Ctx: &validationContext{Error: reason}})
	}
	if year, month, day, ok := parseSpeedateDateRFC3339([]byte(raw)); ok {
		if year == 0 {
			appendParsing("date_parsing", "Input should be a valid date in the format YYYY-MM-DD, ", pydanticYearOutOfRange)
			return nil
		}
		value := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		return &value
	}
	// Pydantic は時刻がちょうど 00:00:00 の datetime も date に変換できる。
	value, detail := parsePydanticDatetime(raw)
	if detail != nil {
		appendParsing("date_from_datetime_parsing", "Input should be a valid date or datetime, ", detail.Msg)
		return nil
	}
	if _, ok := parseSpeedateDateTime([]byte(raw)); ok {
		// 日付への変換では元のオフセットの暦日を保つ。撮影日時の JST 正規化とは異なる。
		_, _, _, _, position, _ := parseSpeedateTime([]byte(raw), 11)
		offset, _, _ := parseSpeedateTimezone([]byte(raw), position)
		if offset != nil {
			value = value.In(time.FixedZone("", *offset))
		}
	} else {
		value = value.UTC()
	}
	if value.Hour() == 0 && value.Minute() == 0 && value.Second() == 0 && value.Nanosecond() == 0 {
		result := value.Format("2006-01-02")
		return &result
	}
	v.details = append(v.details, validationDetail{Type: "date_from_datetime_inexact", Loc: []any{"query", name}, Msg: "Datetimes provided to dates should have zero time - e.g. be exact dates", Input: raw})
	return nil
}

func parseCatalogFilters(v *fastapiValidation) catalogFilters {
	f := catalogFilters{}
	for _, name := range []string{"capture_program_id", "recorded_program_id", "network_id", "service_id", "event_id"} {
		f[name] = nil
		if v.query.Has(name) {
			f[name] = v.queryInt(name)
		}
	}
	f["start_time"] = nil
	if value, present := v.queryDatetime("start_time"); present {
		f["start_time"] = database.FormatJSONTime(value)
	}
	for _, name := range []string{"is_favorite", "is_caption_composited", "is_comment_composited", "has_caption_text", "has_metadata"} {
		f[name] = nil
		if value := catalogQueryBool(v, name); value != nil {
			f[name] = *value
		}
	}
	f["tag_ids"] = nil
	if values, _ := v.queryIntList("tag_ids"); values != nil {
		f["tag_ids"] = values
	}
	f["channel_ids"] = nil
	if values, ok := v.query["channel_ids"]; ok {
		f["channel_ids"] = values
	}
	for _, name := range []string{"start_date", "end_date"} {
		f[name] = nil
		if value := catalogQueryDate(v, name); value != nil {
			f[name] = *value
		}
	}
	f["query"] = nil
	if v.query.Has("query") {
		f["query"] = v.queryString("query")
	}
	f["query_targets"] = nil
	if values, ok := v.query["query_targets"]; ok {
		f["query_targets"] = values
		for i, value := range values {
			if value != "CaptionText" && value != "ProgramTitle" && value != "ProgramDescription" {
				v.details = append(v.details, literalDetail([]any{"query", "query_targets", i}, value, "'CaptionText', 'ProgramTitle' or 'ProgramDescription'"))
			}
		}
	}
	return f
}

func catalogPattern(keyword string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(keyword)) + "%"
}

// catalogWhere は EXISTS と ID 集合を使い、複数の保存場所やタグで画像を重複させない。
func (s *Server) catalogWhere(w http.ResponseWriter, r *http.Request, f catalogFilters, user *database.User, axis string, hour int) ([]string, []any, bool) {
	conditions := []string{}
	args := []any{}
	add := func(query string, values ...any) {
		conditions = append(conditions, query)
		args = append(args, values...)
	}
	keyCount := 0
	for _, name := range []string{"network_id", "service_id", "event_id", "start_time"} {
		if f[name] != nil {
			keyCount++
		}
	}
	if keyCount > 0 && keyCount != 4 {
		writeError(w, 422, "Specify all four program key fields")
		return nil, nil, false
	}
	if keyCount == 4 {
		start, _ := time.Parse(time.RFC3339Nano, f["start_time"].(string))
		add(`p.network_id=? AND p.service_id=? AND p.event_id=? AND p.start_time=?`, f["network_id"], f["service_id"], f["event_id"], database.FormatDBTime(start))
	}
	if f["capture_program_id"] != nil {
		add(`c.capture_program_id=?`, f["capture_program_id"])
	}
	if f["recorded_program_id"] != nil {
		add(`c.capture_program_id IN (SELECT p4.id FROM recorded_programs r JOIN capture_programs p4 ON p4.network_id=r.network_id AND p4.service_id=r.service_id AND p4.event_id=r.event_id AND p4.start_time=r.start_time WHERE r.id=?)`, f["recorded_program_id"])
	}
	if f["is_favorite"] == true {
		if user == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, 401, "Not authenticated")
			return nil, nil, false
		}
		add(`EXISTS(SELECT 1 FROM capture_favorites cf WHERE cf.capture_id=c.id AND cf.user_id=?)`, user.ID)
	}
	if tags, ok := f["tag_ids"].([]int64); ok {
		seen := map[int64]bool{}
		for _, tag := range tags {
			if seen[tag] {
				continue
			}
			seen[tag] = true
			if user == nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, 401, "Not authenticated")
				return nil, nil, false
			}
			var exists bool
			if err := s.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM capture_tags WHERE id=? AND user_id=?)`, tag, user.ID).Scan(&exists); err != nil {
				writeError(w, 500, "Internal Server Error")
				return nil, nil, false
			}
			if !exists {
				writeError(w, 404, "Capture tag not found")
				return nil, nil, false
			}
			add(`EXISTS(SELECT 1 FROM capture_tag_items ti WHERE ti.capture_id=c.id AND ti.tag_id=?)`, tag)
		}
	}
	if channels, ok := f["channel_ids"].([]string); ok && len(channels) > 0 {
		values := make([]any, len(channels))
		for i, ch := range channels {
			values[i] = ch
		}
		add(`c.capture_program_id IN (SELECT p3.id FROM capture_programs p3 JOIN channels ch3 ON ch3.network_id=p3.network_id AND ch3.service_id=p3.service_id WHERE ch3.id IN (`+catalogPlaceholders(len(channels))+`))`, values...)
	}
	for _, name := range []string{"start_date", "end_date"} {
		if value, ok := f[name].(string); ok {
			start, _ := time.ParseInLocation("2006-01-02", value, constants.JST)
			start = start.Add(time.Duration(hour) * time.Hour)
			op := ">="
			if name == "end_date" {
				start = start.AddDate(0, 0, 1)
				op = "<"
			}
			add(`c.`+axis+op+`?`, database.FormatDBTime(start))
		}
	}
	for _, name := range []string{"is_caption_composited", "is_comment_composited"} {
		if value, ok := f[name].(bool); ok {
			add(`c.`+name+`=?`, value)
		}
	}
	if value, ok := f["has_caption_text"].(bool); ok {
		if value {
			add(`(c.caption_text IS NOT NULL AND c.caption_text!='')`)
		} else {
			add(`(c.caption_text IS NULL OR c.caption_text='')`)
		}
	}
	if value, ok := f["has_metadata"].(bool); ok {
		if value {
			add(`c.metadata_status='Parsed'`)
		} else {
			add(`c.metadata_status!='Parsed'`)
		}
	}
	targets := map[string]bool{}
	if values, ok := f["query_targets"].([]string); ok {
		for _, value := range values {
			targets[value] = true
		}
	}
	all := len(targets) == 0 || len(targets) == 3
	query, _ := f["query"].(string)
	for _, keyword := range strings.Fields(query) {
		pattern := catalogPattern(keyword)
		parts := []string{}
		if all || targets["CaptionText"] {
			parts = append(parts, `c.caption_text LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		programParts := []string{}
		if all || targets["ProgramTitle"] {
			programParts = append(programParts, `p2.title LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		if all || targets["ProgramDescription"] {
			programParts = append(programParts, `p2.description LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		if all {
			programParts = append(programParts, `EXISTS(SELECT 1 FROM channels ch WHERE ch.network_id=p2.network_id AND ch.service_id=p2.service_id AND ch.name LIKE ? ESCAPE '\')`)
			args = append(args, pattern)
		}
		if len(programParts) > 0 {
			parts = append(parts, `c.capture_program_id IN (SELECT p2.id FROM capture_programs p2 WHERE `+strings.Join(programParts, " OR ")+`)`)
		}
		if all {
			parts = append(parts, `c.id IN (SELECT f.capture_id FROM capture_files f WHERE f.id IN (SELECT f2.id FROM capture_files f2 WHERE f2.file_name LIKE ? ESCAPE '\'))`)
			args = append(args, pattern)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	conditions = append(conditions, catalogAvailable)
	return conditions, args, true
}

// catalogSignature は Python json.dumps(sort_keys=True) の空白・ASCII エスケープを再現する。
func catalogSignature(values any) string {
	raw, _ := marshalPythonJSON(values)
	var out strings.Builder
	quoted, escaped := false, false
	for _, ch := range string(raw) {
		if ch == '"' && !escaped {
			quoted = !quoted
		}
		if ch > 127 {
			for _, unit := range utf16.Encode([]rune{ch}) {
				fmt.Fprintf(&out, `\u%04x`, unit)
			}
		} else {
			out.WriteRune(ch)
		}
		if !quoted && (ch == ',' || ch == ':') {
			out.WriteByte(' ')
		}
		if ch == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(out.String())))
}

type catalogCursor struct {
	Signature string `json:"signature"`
	Keys      []any  `json:"keys"`
}

func catalogEncodeCursor(signature string, keys []any) *string {
	raw, _ := marshalPythonJSON(catalogCursor{signature, keys})
	value := base64.URLEncoding.EncodeToString(raw)
	return &value
}
func catalogDecodeCursor(raw, signature string, count int) ([]any, bool) {
	// Python は URL 用と標準の両方の Base64 文字を受け付ける。
	if strings.ContainsAny(raw, "\r\n") {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(raw))
	if err != nil {
		return nil, false
	}
	var cursor catalogCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.UseNumber()
	if err = decoder.Decode(&cursor); err != nil || cursor.Signature != signature || len(cursor.Keys) != count {
		return nil, false
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, false
	}
	for i, key := range cursor.Keys {
		switch value := key.(type) {
		case string:
		case json.Number:
			if integer, err := value.Int64(); err == nil {
				cursor.Keys[i] = integer
			} else if number, err := strconv.ParseFloat(value.String(), 64); err == nil {
				cursor.Keys[i] = number
			} else {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return cursor.Keys, true
}

func catalogProgramIDs(captures []catalogCapture) []int64 {
	set := map[int64]bool{}
	for _, c := range captures {
		if c.ProgramID != nil {
			set[*c.ProgramID] = true
		}
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
