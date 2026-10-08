package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/epgupdate"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// updateDatabaseFromEDCB はチャンネル情報・実況ステータス・番組情報を EDCB から順に更新する。
// 移植元: MaintenanceRouter.UpdateDatabaseAPI() (Channel.update / Channel.updateJikkyoStatus / Program.update)
func (s *Server) updateDatabaseFromEDCB(ctx context.Context) {
	s.updateDatabaseMu.Lock()
	defer s.updateDatabaseMu.Unlock()

	start := time.Now()
	s.logger.Info("Channels updating...")
	if source, err := s.newEDCBUpdateSource(5 * time.Second); err != nil {
		s.logger.Error("Failed to update channels:", "error", err)
	} else if err := epgupdate.UpdateChannelsFromEDCB(ctx, s.writeDB, source, s.config.TV.PreferredTerrestrialRegion, s.logger); err != nil {
		s.logger.Error("Failed to update channels:", "error", err)
	}
	s.logger.Info(fmt.Sprintf("Channels update complete. (%.3f sec)", time.Since(start).Seconds()))

	fetch := s.jikkyoStatusFetch
	if fetch == nil {
		fetch = epgupdate.HTTPFetchJikkyoChannels(&http.Client{})
	}
	if err := epgupdate.UpdateJikkyoStatus(ctx, s.writeDB, s.jikkyoChannelMap(), &s.jikkyoStatuses, fetch, time.Now(), s.logger); err != nil {
		s.logger.Error("Failed to update jikkyo status:", "error", err)
	}

	start = time.Now()
	s.logger.Info("Programs updating...")
	if source, err := s.newEDCBUpdateSource(10 * time.Second); err != nil {
		s.logger.Error("Failed to update programs from EDCB:", "error", err)
	} else if err := epgupdate.UpdateProgramsFromEDCB(ctx, s.writeDB, source, time.Now(), s.logger); err != nil {
		s.logger.Error("Failed to update programs from EDCB:", "error", err)
	}
	s.logger.Info(fmt.Sprintf("Programs update complete. (%.3f sec)", time.Since(start).Seconds()))
}

// newEDCBUpdateSource は更新処理用の EDCB クライアントを生成する (テストでは差し替え可能) 。
func (s *Server) newEDCBUpdateSource(timeout time.Duration) (epgupdate.EDCBSource, error) {
	if s.edcbUpdateSource != nil {
		return s.edcbUpdateSource, nil
	}
	client, err := reservations.NewClientFromURL(s.config.General.EDCBURL)
	if err != nil {
		return nil, err
	}
	client.SetTimeout(timeout)
	return client, nil
}
