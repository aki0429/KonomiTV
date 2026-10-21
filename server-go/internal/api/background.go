package api

import (
	"context"
	"sync"
	"time"
)

// StartBackgroundTasks は Go 単独運用の EPG・IPTV 更新を開始する。
// 呼び出し元の context で停止し、返した channel の close で全 worker の終了を確認できる。
// Python 版 app.py の Startup / UpdateChannelAndProgram / UpdateIPTVChannels 相当。
func (s *Server) StartBackgroundTasks(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var workers sync.WaitGroup
	if s.proxy == nil && len(s.config.Video.RecordedFolders) > 0 {
		service := s.newMetadataService()
		// 起動時全件解析が遅くても新規録画の検出を待たせない。
		workers.Go(func() {
			if _, err := service.RunBatchScan(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("Startup recording scan failed.", "error", err)
			}
		})
		workers.Go(func() { _ = service.WatchRecordedFolders(ctx, 5*time.Second) })
	}
	if s.config.General.Backend != "IPTV" && s.proxy == nil {
		workers.Go(func() {
			interval := time.Duration(s.config.General.ProgramUpdateInterval * float64(time.Minute))
			runScheduledUpdate(ctx, interval, 2*time.Minute, s.updateDatabaseNative)
		})
	}
	if s.iptv != nil && s.config.IPTV.Enabled {
		workers.Go(func() {
			runScheduledUpdate(ctx, 30*time.Second, 5*time.Minute, func(ctx context.Context) {
				s.iptv.Refresh(ctx, false)
			})
		})
	}
	go func() {
		workers.Wait()
		close(done)
	}()
	return done
}

// runScheduledUpdate は初回を直ちに実行し、完了後に次の周期を待つ。
// 更新が周期より長くても worker を積み増さず、処理期限とキャンセルを引き継ぐ。
func runScheduledUpdate(ctx context.Context, interval, timeout time.Duration, update func(context.Context)) {
	if interval <= 0 {
		interval = time.Minute
	}
	for ctx.Err() == nil {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		update(requestCtx)
		cancel()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
