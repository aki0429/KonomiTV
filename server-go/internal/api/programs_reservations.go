package api

import (
	"fmt"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// timeTableReservation は EID が更新途中の番組を放送時間から照合するための予約。
type timeTableReservation struct {
	start, end time.Time
	response   *timeTableProgramReservationResponse
}

// timeTableReservations は本家 TimeTableAPI と同じ予約状態・競合状態を取得する。
// 予約サーバーの障害で番組表全体を失わないよう、取得失敗時は空の対応表を返す。
func (s *Server) timeTableReservations(channels []database.Channel, now time.Time) (
	map[string]*timeTableProgramReservationResponse, map[string][]timeTableReservation,
) {
	byID := map[string]*timeTableProgramReservationResponse{}
	byTime := map[string][]timeTableReservation{}
	if s.config.General.Backend != "EDCB" {
		return byID, byTime
	}
	client, err := edcbClientFactory(s.config)
	if err != nil {
		s.logger.Warn("Failed to get timetable reservations.", "error", err)
		return byID, byTime
	}
	reserves, ok := client.EnumReserve()
	if !ok {
		s.logger.Warn("Failed to get timetable reservations.")
		return byID, byTime
	}
	channelByService := map[[3]int]string{}
	for _, channel := range channels {
		if channel.TransportStreamID != nil {
			channelByService[[3]int{channel.NetworkID, *channel.TransportStreamID, channel.ServiceID}] = channel.ID
		}
	}
	for _, reserve := range reserves {
		start := normalizeToJSTDatetime(reserve.StartTime)
		end := start.Add(time.Duration(reserve.DurationSecond) * time.Second)
		status := "Reserved"
		if reserve.RecSetting.RecMode >= 5 {
			status = "Disabled"
		} else if !start.After(now.Add(2*time.Hour)) && !end.Before(now.Add(-2*time.Hour)) {
			if client.GetRecFilePath(reserve.ReserveID) != nil {
				status = "Recording"
			}
		}
		availability := "Full"
		switch reserve.OverlapMode {
		case 1:
			availability = "Partial"
		case 2:
			availability = "Unavailable"
		}
		response := &timeTableProgramReservationResponse{
			ID: reserve.ReserveID, Status: status, RecordingAvailability: availability,
		}
		byID[fmt.Sprintf("NID%d-SID%03d-EID%d", reserve.Onid, reserve.Sid, reserve.Eid)] = response
		if channelID, found := channelByService[[3]int{reserve.Onid, reserve.Tsid, reserve.Sid}]; found {
			byTime[channelID] = append(byTime[channelID], timeTableReservation{start, end, response})
		}
	}
	return byID, byTime
}

// matchTimeTableReservation は同一サービスで重なりが最大の予約を一度だけ暫定割当する。
func matchTimeTableReservation(byTime map[string][]timeTableReservation, channelID, startText, endText string) *timeTableProgramReservationResponse {
	start, err := database.ParseDBTime(startText)
	if err != nil {
		return nil
	}
	end, err := database.ParseDBTime(endText)
	if err != nil {
		return nil
	}
	candidates := byTime[channelID]
	best, bestOverlap := -1, time.Duration(0)
	for index, candidate := range candidates {
		overlapStart, overlapEnd := start, end
		if candidate.start.After(overlapStart) {
			overlapStart = candidate.start
		}
		if candidate.end.Before(overlapEnd) {
			overlapEnd = candidate.end
		}
		if overlap := overlapEnd.Sub(overlapStart); overlap > bestOverlap {
			best, bestOverlap = index, overlap
		}
	}
	if best < 0 {
		return nil
	}
	result := candidates[best].response
	byTime[channelID] = append(candidates[:best], candidates[best+1:]...)
	return result
}
