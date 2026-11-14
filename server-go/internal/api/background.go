package api

import (
	"context"
	"math"
	"sync"
	"time"
)

// RunBackgroundUpdates は Go 単独運転で本家 app.py の起動時・定期更新を担う。
// Python 併走時は呼ばず、終了時には全更新がキャンセルされてから戻る。
func (s *Server) RunBackgroundUpdates(ctx context.Context) {
	interval := s.config.General.ProgramUpdateInterval
	if interval <= 0 || math.IsNaN(interval) || math.IsInf(interval, 0) {
		interval = 15
	}
	var workers sync.WaitGroup
	workers.Add(3)
	catalogReady := make(chan struct{})
	go func() {
		defer workers.Done()
		if ctx.Err() == nil {
			s.updateDatabaseNative(ctx)
		}
		close(catalogReady)
		runPeriodicUpdate(ctx, time.Duration(interval*float64(time.Minute)), false, s.updateDatabaseNative)
	}()
	go func() {
		defer workers.Done()
		runPeriodicUpdate(ctx, 30*time.Second, false, s.updateJikkyoStatus)
	}()
	go func() {
		defer workers.Done()
		// 新規 DB でも地域を選べるよう、初回チャンネル取得の完了を待つ。
		select {
		case <-ctx.Done():
			return
		case <-catalogReady:
		}
		runPeriodicUpdate(ctx, 3*time.Hour, true, s.updateToruCounts)
	}()
	workers.Wait()
}

// runPeriodicUpdate は前回の処理終了後から周期を測り、遅い更新を重複実行しない。
func runPeriodicUpdate(ctx context.Context, interval time.Duration, immediate bool, update func(context.Context)) {
	if interval <= 0 {
		interval = time.Minute
	}
	if immediate && ctx.Err() == nil {
		update(ctx)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			update(ctx)
			timer.Reset(interval)
		}
	}
}
