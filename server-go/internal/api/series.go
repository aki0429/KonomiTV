package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// seriesPageSize はページングで一度に取得するシリーズ番組の数 (Python 版と同じ) 。
const seriesPageSize = 30

// seriesResponse は schemas.Series 互換のレスポンス。
type seriesResponse struct {
	ID               int64                           `json:"id"`
	Title            string                          `json:"title"`
	Description      string                          `json:"description"`
	Genres           json.RawMessage                 `json:"genres"`
	BroadcastPeriods []seriesBroadcastPeriodResponse `json:"broadcast_periods"`
	CreatedAt        string                          `json:"created_at"`
	UpdatedAt        string                          `json:"updated_at"`
}

// seriesBroadcastPeriodResponse は schemas.SeriesBroadcastPeriod 互換のレスポンス。
type seriesBroadcastPeriodResponse struct {
	Channel          *channelResponse          `json:"channel"`
	StartDate        string                    `json:"start_date"`
	EndDate          string                    `json:"end_date"`
	RecordedPrograms []recordedProgramResponse `json:"recorded_programs"`
}

// seriesListResponse は schemas.SeriesList 互換のレスポンス。
type seriesListResponse struct {
	Total      int64            `json:"total"`
	SeriesList []seriesResponse `json:"series_list"`
}

// handleSeriesList は GET /api/series (シリーズ番組一覧 API) を処理する。
func (s *Server) handleSeriesList(w http.ResponseWriter, r *http.Request) {
	s.serveSeriesList(w, r, "")
}

// handleSeriesSearch は GET /api/series/search (シリーズ番組検索 API) を処理する。
func (s *Server) handleSeriesSearch(w http.ResponseWriter, r *http.Request) {
	// クエリが空の場合は全件取得と同じ挙動にする
	s.serveSeriesList(w, r, r.URL.Query().Get("query"))
}

// serveSeriesList はシリーズ番組の一覧・検索 API の共通処理。
func (s *Server) serveSeriesList(w http.ResponseWriter, r *http.Request, searchQuery string) {
	v := newFastAPIValidation(r)

	// order は Literal['desc', 'asc'] で、宣言順に従って page より先に検証する。
	// 欠落時は既定値 'desc' を使う (missing にはしない) 。
	order := v.queryLiteral("order", []string{"desc", "asc"}, "desc", "'desc' or 'asc'")

	// page は int 型で既定値 1。指定があるときだけ int_parsing を検証し、
	// 欠落時は既定値 1 を使う (missing にはしない) 。
	page := 1
	if _, exists := r.URL.Query()["page"]; exists {
		page = int(v.queryInt("page"))
	}

	// 検証エラー (literal_error / int_parsing) があれば宣言順の配列で 422 を返す
	if v.writeIfInvalid(w) {
		return
	}

	// ページ番号から OFFSET を求める。Python の int は上限がないため、
	// int 演算がオーバーフローするほど大きいページでも結果が空になる値へ寄せる
	offset := (page - 1) * seriesPageSize
	if page > 1 && page > math.MaxInt/seriesPageSize {
		offset = math.MaxInt
	}

	// シリーズ番組の一覧を取得する
	seriesList, total, err := database.ListSeries(
		r.Context(), s.db, searchQuery, order == "desc", offset, seriesPageSize,
	)
	if err != nil {
		s.logger.Error("failed to list series", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	responses, err := s.buildSeriesResponses(r, seriesList)
	if err != nil {
		s.logger.Error("failed to build series response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	writeJSON(w, http.StatusOK, seriesListResponse{Total: total, SeriesList: responses})
}

// handleSeries は GET /api/series/{series_id} (シリーズ番組 API) を処理する。
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	// series_id は必須の int パスパラメータ。解析できない場合は Pydantic と同じ
	// int_parsing (loc: ["path", "series_id"]) を 422 で返す
	v := newFastAPIValidation(r)
	seriesID, ok := v.pathInt(r, "series_id")
	if !ok {
		v.writeIfInvalid(w)
		return
	}

	// Python 版は get_or_none で取得するため、存在しない ID は 422 になる
	target, err := database.GetSeriesByID(r.Context(), s.db, seriesID)
	if err != nil {
		s.logger.Error("failed to get series", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if target == nil {
		s.logger.Warn("[SeriesRouter][SeriesAPI] Specified series_id was not found.", "series_id", seriesID)
		writeError(w, http.StatusUnprocessableEntity, "Specified series_id was not found")
		return
	}

	responses, err := s.buildSeriesResponses(r, []database.Series{*target})
	if err != nil {
		s.logger.Error("failed to build series response", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, responses[0])
}

// buildSeriesResponses はシリーズ番組のレスポンスを構築する (関連する放送期間・録画番組も含む) 。
func (s *Server) buildSeriesResponses(r *http.Request, seriesList []database.Series) ([]seriesResponse, error) {
	responses := make([]seriesResponse, 0, len(seriesList))
	if len(seriesList) == 0 {
		return responses, nil
	}

	// 放送期間を取得する
	seriesIDs := make([]int64, 0, len(seriesList))
	for _, series := range seriesList {
		seriesIDs = append(seriesIDs, series.ID)
	}
	periods, err := database.ListBroadcastPeriodsBySeriesIDs(r.Context(), s.db, seriesIDs)
	if err != nil {
		return nil, err
	}

	// 放送期間に紐づく録画番組を取得する
	periodIDs := make([]int64, 0, len(periods))
	for _, period := range periods {
		periodIDs = append(periodIDs, period.ID)
	}
	programs, err := database.ListRecordedProgramsByBroadcastPeriodIDs(r.Context(), s.db, periodIDs)
	if err != nil {
		return nil, err
	}
	programIDs := make([]int64, 0, len(programs))
	for _, program := range programs {
		programIDs = append(programIDs, program.ID)
	}
	videos, err := database.GetRecordedVideosByProgramIDs(r.Context(), s.db, programIDs)
	if err != nil {
		return nil, err
	}

	// チャンネル情報をまとめて取得する (放送期間と録画番組の両方で使う)
	channelIDs := make([]string, 0, len(periods)+len(programs))
	seenChannelIDs := map[string]bool{}
	appendChannelID := func(channelID *string) {
		if channelID == nil || seenChannelIDs[*channelID] {
			return
		}
		seenChannelIDs[*channelID] = true
		channelIDs = append(channelIDs, *channelID)
	}
	for _, period := range periods {
		channelID := period.ChannelID
		appendChannelID(&channelID)
	}
	for index := range programs {
		appendChannelID(programs[index].ChannelID)
	}
	channels, err := database.ListChannelsByIDs(r.Context(), s.db, channelIDs)
	if err != nil {
		return nil, err
	}

	// 放送期間ごとに録画番組をグループ化する
	programsByPeriod := map[int64][]database.RecordedProgram{}
	for index := range programs {
		program := programs[index]
		if program.SeriesBroadcastPeriodID == nil {
			continue
		}
		programsByPeriod[*program.SeriesBroadcastPeriodID] = append(programsByPeriod[*program.SeriesBroadcastPeriodID], program)
	}
	periodsBySeries := map[int64][]database.SeriesBroadcastPeriod{}
	for _, period := range periods {
		periodsBySeries[period.SeriesID] = append(periodsBySeries[period.SeriesID], period)
	}

	for _, series := range seriesList {
		if !json.Valid([]byte(series.Genres)) {
			return nil, fmt.Errorf("invalid JSON in series.genres (%d)", series.ID)
		}
		periodResponses := make([]seriesBroadcastPeriodResponse, 0)
		for _, period := range periodsBySeries[series.ID] {
			programResponses := make([]recordedProgramResponse, 0)
			for _, program := range programsByPeriod[period.ID] {
				var video *database.RecordedVideo
				if stored, ok := videos[program.ID]; ok {
					video = &stored
				}
				var channel *database.Channel
				if program.ChannelID != nil {
					if stored, ok := channels[*program.ChannelID]; ok {
						channel = &stored
					}
				}
				programResponse, err := buildRecordedProgramResponse(&program, video, channel)
				if err != nil {
					return nil, err
				}
				programResponses = append(programResponses, *programResponse)
			}
			var periodChannel *database.Channel
			if stored, ok := channels[period.ChannelID]; ok {
				periodChannel = &stored
			}
			periodResponses = append(periodResponses, seriesBroadcastPeriodResponse{
				Channel:          buildChannelResponse(periodChannel),
				StartDate:        formatDateString(period.StartDate),
				EndDate:          formatDateString(period.EndDate),
				RecordedPrograms: programResponses,
			})
		}
		responses = append(responses, seriesResponse{
			ID:               series.ID,
			Title:            series.Title,
			Description:      series.Description,
			Genres:           json.RawMessage(series.Genres),
			BroadcastPeriods: periodResponses,
			CreatedAt:        database.FormatJSONTime(series.CreatedAt),
			UpdatedAt:        database.FormatJSONTime(series.UpdatedAt),
		})
	}
	return responses, nil
}

// formatDateString は DATE カラムの値を Pydantic の date と同じ "YYYY-MM-DD" 形式に変換する。
func formatDateString(value string) string {
	// Tortoise は DATE カラムに "YYYY-MM-DD" 形式で保存するが、
	// 念のため日時付きで保存されていた場合も日付部分だけを取り出す
	if len(value) >= len("2006-01-02") && value[4] == '-' && value[7] == '-' {
		return value[:10]
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.Format("2006-01-02")
	}
	return value
}
