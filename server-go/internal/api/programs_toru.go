package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/torne"
)

// updateToruCounts は本家 TorneClient の地域別取得を Go で実行する。
func (s *Server) updateToruCounts(ctx context.Context) {
	channels, err := database.ListWatchableChannels(ctx, s.db)
	if err != nil {
		s.logger.Warn("Failed to list channels for Toru counts.", "error", err)
		return
	}
	values := make([]string, 6)
	for index := range values {
		values[index], err = interlacedValue(s.paths.StaticDir, index+4)
		if err != nil {
			s.logger.Warn("Toru count API credentials are unavailable.")
			return
		}
	}
	services := make([]torne.Channel, 0, len(channels))
	for _, channel := range channels {
		services = append(services, torne.Channel{Type: channel.Type, NetworkID: channel.NetworkID, ServiceID: channel.ServiceID})
	}
	fetch := torne.HTTPFetcher(&http.Client{}, "https://syn.so-net.ne.jp/sen", torne.Credentials{
		Username: values[0], Password: values[1], MobileIDAuth: values[2], UserID: values[3], DeviceID: values[4], Version: values[5],
	})
	if err := s.toruCounts.Update(ctx, services, time.Now(), fetch); err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("Failed to fetch Toru counts.", "error", err)
		}
		return
	}
	s.logger.Info("Toru counts updated.", "programs", len(s.toruCounts.Snapshot()))
}

// handleToruCounts は本家と同じく、初回取得前には空の辞書を返す。
func (s *Server) handleToruCounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.toruCounts.Snapshot())
}

// handleToruRanking は放送波別の基準に対する倍率で上位 100 件を並べる。
func (s *Server) handleToruRanking(w http.ResponseWriter, r *http.Request) {
	validation := newFastAPIValidation(r)
	channelType := validation.queryLiteral("channel_type", []string{"GR", "BS", "CS"}, "", "'GR', 'BS' or 'CS'")
	minimumPage := 1
	page := validation.queryIntRange("page", 1, &minimumPage, nil)
	if validation.writeIfInvalid(w) {
		return
	}
	programs, err := database.ListUnfinishedPrograms(r.Context(), s.db, time.Now(), channelType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	counts := s.toruCounts.Snapshot()
	type ranked struct {
		program *database.Program
		count   int64
		ratio   float64
	}
	ranking := []ranked{}
	genre, hasGenre := r.URL.Query()["genre"]
	for _, program := range programs {
		if hasGenre {
			var genres []genreSchema
			if json.Unmarshal([]byte(program.Genres), &genres) != nil || len(genres) == 0 || genres[0].Major != lastQueryValue(genre) {
				continue
			}
		}
		count, exists := counts[program.ID]
		if !exists {
			continue
		}
		ratio := 0.0
		if count.HighlightBase != nil && *count.HighlightBase != 0 {
			ratio = float64(count.Count) / float64(*count.HighlightBase)
		}
		ranking = append(ranking, ranked{program, count.Count, ratio})
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		if ranking[i].ratio != ranking[j].ratio {
			return ranking[i].ratio > ranking[j].ratio
		}
		if ranking[i].count != ranking[j].count {
			return ranking[i].count > ranking[j].count
		}
		return ranking[i].program.StartTime.Before(ranking[j].program.StartTime)
	})
	if len(ranking) > 100 {
		ranking = ranking[:100]
	}
	result := []programResponse{}
	// page は int64 最大値まで有効なので、掛け算の前に件数から空ページを判定する。
	if page <= int64((len(ranking)+19)/20) {
		start := int(page-1) * 20
		end := start + 20
		if end > len(ranking) {
			end = len(ranking)
		}
		for _, row := range ranking[start:end] {
			response, err := buildProgramResponse(row.program)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Internal Server Error")
				return
			}
			result = append(result, *response)
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Total    int               `json:"total"`
		Programs []programResponse `json:"programs"`
	}{len(ranking), result})
}
