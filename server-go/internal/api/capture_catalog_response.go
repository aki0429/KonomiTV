package api

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// catalogCapture は保存場所を公開しない本家の Capture 応答。
type catalogCapture struct {
	ID                int64    `json:"id"`
	Hash              string   `json:"file_hash"`
	Size              int64    `json:"file_size"`
	Captured          string   `json:"captured_at"`
	Broadcast         *string  `json:"broadcast_at"`
	Width             int      `json:"image_width"`
	Height            int      `json:"image_height"`
	Metadata          string   `json:"metadata_status"`
	Position          *float64 `json:"playback_position"`
	Caption           *string  `json:"caption_text"`
	CaptionComposited bool     `json:"is_caption_composited"`
	CommentComposited bool     `json:"is_comment_composited"`
	ProgramID         *int64   `json:"capture_program_id"`
	Favorite          *bool    `json:"is_favorite"`
	Tags              []int64  `json:"tag_ids"`
	// rawCaptured/rawBroadcast はカーソルで DB の表現をそのまま比較するために保持する。
	rawCaptured  string
	rawBroadcast sql.NullString
}

const catalogCaptureColumns = `c.id,c.file_hash,c.file_size,CAST(c.captured_at AS TEXT),CAST(c.broadcast_at AS TEXT),c.image_width,c.image_height,c.metadata_status,c.playback_position,c.caption_text,c.is_caption_composited,c.is_comment_composited,c.capture_program_id`
const catalogAvailable = `EXISTS(SELECT 1 FROM capture_files f WHERE f.capture_id=c.id AND f.status='Available')`

func catalogJSONTime(raw string) (string, error) {
	t, err := database.ParseDBTime(raw)
	if err != nil {
		return "", err
	}
	return database.FormatJSONTime(t), nil
}

func scanCatalogCapture(scan func(...any) error) (catalogCapture, error) {
	var c catalogCapture
	var hash []byte
	err := scan(&c.ID, &hash, &c.Size, &c.rawCaptured, &c.rawBroadcast, &c.Width, &c.Height, &c.Metadata, &c.Position, &c.Caption, &c.CaptionComposited, &c.CommentComposited, &c.ProgramID)
	if err != nil {
		return c, err
	}
	c.Hash = fmt.Sprintf("%x", hash)
	if c.Captured, err = catalogJSONTime(c.rawCaptured); err != nil {
		return c, err
	}
	if c.rawBroadcast.Valid {
		value, e := catalogJSONTime(c.rawBroadcast.String)
		if e != nil {
			return c, e
		}
		c.Broadcast = &value
	}
	return c, nil
}

func catalogPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// personalize はページ全体に対して二回の問い合わせで本人のタグとお気に入りを重ねる。
func (s *Server) personalize(ctx context.Context, captures []catalogCapture, user *database.User) error {
	if user == nil || len(captures) == 0 {
		return nil
	}
	args := []any{user.ID}
	indices := map[int64]int{}
	for i := range captures {
		value := false
		captures[i].Favorite = &value
		captures[i].Tags = []int64{}
		args = append(args, captures[i].ID)
		indices[captures[i].ID] = i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT capture_id FROM capture_favorites WHERE user_id=? AND capture_id IN (`+catalogPlaceholders(len(captures))+`)`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		if i, ok := indices[id]; ok {
			*captures[i].Favorite = true
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT ti.capture_id,ti.tag_id FROM capture_tag_items ti JOIN capture_tags t ON t.id=ti.tag_id WHERE t.user_id=? AND ti.capture_id IN (`+catalogPlaceholders(len(captures))+`) ORDER BY ti.tag_id`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, tag int64
		if err := rows.Scan(&id, &tag); err != nil {
			return err
		}
		if i, ok := indices[id]; ok {
			captures[i].Tags = append(captures[i].Tags, tag)
		}
	}
	return rows.Err()
}

type catalogChannel struct {
	ID        string `json:"id"`
	DisplayID string `json:"display_channel_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
}
type catalogProgram struct {
	ID          int64           `json:"id"`
	NID         int             `json:"network_id"`
	SID         int             `json:"service_id"`
	EID         int             `json:"event_id"`
	Start       string          `json:"start_time"`
	End         *string         `json:"end_time"`
	Duration    *float64        `json:"duration"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Channel     *catalogChannel `json:"channel"`
	RecordedIDs []int64         `json:"recorded_program_ids"`
}
type catalogAlbum struct {
	catalogProgram
	Count int64          `json:"capture_count"`
	Cover catalogCapture `json:"cover_capture"`
}

// catalogPrograms は番組・チャンネル・録画番組をまとめて読み、渡された ID の順に返す。
func (s *Server) catalogPrograms(ctx context.Context, ids []int64) ([]catalogProgram, error) {
	result := []catalogProgram{}
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := catalogPlaceholders(len(ids))
	rows, err := s.db.QueryContext(ctx, `SELECT id,network_id,service_id,event_id,start_time,end_time,duration,title,description FROM capture_programs WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*catalogProgram{}
	for rows.Next() {
		var p catalogProgram
		var start string
		var end sql.NullString
		if err = rows.Scan(&p.ID, &p.NID, &p.SID, &p.EID, &start, &end, &p.Duration, &p.Title, &p.Description); err != nil {
			break
		}
		if p.Start, err = catalogJSONTime(start); err != nil {
			break
		}
		if end.Valid {
			var value string
			value, err = catalogJSONTime(end.String)
			if err != nil {
				break
			}
			p.End = &value
		}
		p.RecordedIDs = []int64{}
		byID[p.ID] = &p
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT p.id,ch.id,ch.display_channel_id,ch.name,ch.type FROM capture_programs p JOIN channels ch ON ch.network_id=p.network_id AND ch.service_id=p.service_id WHERE p.id IN (`+placeholders+`) ORDER BY ch.id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var ch catalogChannel
		if err = rows.Scan(&id, &ch.ID, &ch.DisplayID, &ch.Name, &ch.Type); err != nil {
			break
		}
		if p := byID[id]; p != nil && p.Channel == nil {
			p.Channel = &ch
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT p.id,r.id FROM capture_programs p JOIN recorded_programs r ON r.network_id=p.network_id AND r.service_id=p.service_id AND r.event_id=p.event_id AND r.start_time=p.start_time JOIN recorded_videos rv ON rv.recorded_program_id=r.id WHERE p.id IN (`+placeholders+`) AND rv.status!='Missing' ORDER BY r.id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, recorded int64
		if err = rows.Scan(&id, &recorded); err != nil {
			break
		}
		if p := byID[id]; p != nil {
			p.RecordedIDs = append(p.RecordedIDs, recorded)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if p := byID[id]; p != nil {
			result = append(result, *p)
		}
	}
	return result, nil
}

// catalogAlbums は再生位置のある画像の中央を表紙にし、位置不明の画像を後ろに並べる。
func (s *Server) catalogAlbums(ctx context.Context, ids []int64, user *database.User) ([]catalogAlbum, error) {
	result := []catalogAlbum{}
	if len(ids) == 0 {
		return result, nil
	}
	programs, err := s.catalogPrograms(ctx, ids)
	if err != nil {
		return nil, err
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+catalogCaptureColumns+`,a.album_count FROM (
	 SELECT c.id,ROW_NUMBER() OVER album_order AS album_position,COUNT(c.playback_position) OVER album AS positioned_count,COUNT(*) OVER album AS album_count
	 FROM captures c WHERE c.capture_program_id IN (`+catalogPlaceholders(len(ids))+`) AND `+catalogAvailable+`
	 WINDOW album AS (PARTITION BY c.capture_program_id),album_order AS (PARTITION BY c.capture_program_id ORDER BY (c.playback_position IS NULL),c.playback_position,c.captured_at,c.id)
	 ) a JOIN captures c ON c.id=a.id WHERE a.album_position=a.positioned_count/2+1`, args...)
	if err != nil {
		return nil, err
	}
	covers := []catalogCapture{}
	counts := map[int64]int64{}
	for rows.Next() {
		var c catalogCapture
		var count int64
		c, err = scanCatalogCapture(func(dest ...any) error { return rows.Scan(append(dest, &count)...) })
		if err != nil {
			break
		}
		covers = append(covers, c)
		if c.ProgramID != nil {
			counts[*c.ProgramID] = count
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err = s.personalize(ctx, covers, user); err != nil {
		return nil, err
	}
	byID := map[int64]catalogCapture{}
	for _, c := range covers {
		if c.ProgramID != nil {
			byID[*c.ProgramID] = c
		}
	}
	for _, p := range programs {
		if cover, ok := byID[p.ID]; ok {
			result = append(result, catalogAlbum{catalogProgram: p, Count: counts[p.ID], Cover: cover})
		}
	}
	return result, nil
}
