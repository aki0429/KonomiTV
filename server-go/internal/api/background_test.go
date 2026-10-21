package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduledUpdateLifecycle は初回・周期・非重複・停止を合成処理で検証する。
func TestScheduledUpdateLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan context.Context, 2)
	release := make(chan struct{})
	done := make(chan struct{})
	var active, maximum atomic.Int32
	go func() {
		defer close(done)
		runScheduledUpdate(ctx, time.Millisecond, time.Second, func(callCtx context.Context) {
			count := active.Add(1)
			maximum.Store(max(maximum.Load(), count))
			defer active.Add(-1)
			select {
			case called <- callCtx:
			case <-callCtx.Done():
				return
			}
			select {
			case <-release:
			case <-callCtx.Done():
			}
		})
	}()
	select {
	case callCtx := <-called:
		if _, ok := callCtx.Deadline(); !ok {
			t.Fatal("missing per-update deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup update did not execute")
	}
	close(release)
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic update did not execute")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
	if maximum.Load() != 1 {
		t.Fatal("updates overlapped")
	}
}

// TestBackgroundUpdatesNativeEPG は起動時に合成 Mirakurun から DB が更新されることを検証する。
type startupMirakurun struct {
	updateDatabaseFakeMirakurun
	programs chan struct{}
}

func (f *startupMirakurun) Programs(ctx context.Context) ([]byte, error) {
	defer close(f.programs)
	return f.updateDatabaseFakeMirakurun.Programs(ctx)
}

func TestBackgroundUpdatesNativeEPG(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "Mirakurun"
	s.config.IPTV.Enabled = false
	fake := &startupMirakurun{programs: make(chan struct{})}
	s.mirakurunSource = fake
	s.jikkyoStatusFetch = func(context.Context) ([]byte, error) { return []byte("[]"), nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := s.StartBackgroundTasks(ctx)
	defer cancel()
	select {
	case <-fake.programs:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("startup update did not reach program fetch")
	}
	// 読取ループで合成 DB の commit と競合させず、更新 worker の終了後に結果を確認する。
	cancel()
	<-done
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM channels WHERE id='NID32736-SID1024'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("native startup channel count=%d err=%v", count, err)
	}
}
