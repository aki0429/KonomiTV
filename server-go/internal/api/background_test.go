package api

import (
	"context"
	"testing"
	"time"
)

// TestPeriodicUpdateLifecycle は初回更新・反復・実行中のキャンセルを確認する。
func TestPeriodicUpdateLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan int, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		runPeriodicUpdate(ctx, time.Millisecond, true, func(ctx context.Context) {
			count++
			calls <- count
			if count == 2 {
				<-ctx.Done()
			}
		})
	}()
	for _, want := range []int{1, 2} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("call=%d want=%d", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("update did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("running update did not stop")
	}
	select {
	case <-calls:
		t.Fatal("update ran after cancellation")
	default:
	}
}

// TestPeriodicUpdateWaitFirst は実況更新を起動時の番組更新と二重に開始しないことを確認する。
func TestPeriodicUpdateWaitFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runPeriodicUpdate(ctx, time.Hour, false, func(context.Context) { t.Error("canceled update ran") })
}
