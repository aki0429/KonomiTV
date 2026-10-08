package videostream

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestSessionKeepAliveSupersedesExpiredCallback は発火済みだが mu を取得できない旧タイマーを再現する。
// KeepAlive と同じロック内更新を先に完了させ、旧 callback が新しい生存期限を消さないことを検証する。
func TestSessionKeepAliveSupersedesExpiredCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(testLogger())
		manager.SessionTimeout = 120 * time.Millisecond
		session := newTestSession(t, manager, "renew", testProgram(60, "MPEG-TS"), "FFmpeg", nil)

		// root の Sleep も同じ期限で解除するため、mu 待ち callback がいても仮想時刻は先へ進めなくてよい。
		session.mu.Lock()
		time.Sleep(manager.SessionTimeout)
		// Stop=false は callback が既に発火し、Stop だけでは実行を取り消せないことの前提確認。
		if session.destroyTimer.Stop() {
			session.mu.Unlock()
			t.Fatal("旧タイマーがまだ発火していません")
		}
		session.armDestroyTimerLocked()
		session.mu.Unlock()
		synctest.Wait()
		if session.IsDestroyed() || manager.Count() != 1 || manager.Lookup("renew") != session {
			t.Fatal("発火済みの旧タイマーが KeepAlive 後のセッションを破棄しました")
		}

		time.Sleep(manager.SessionTimeout - time.Nanosecond)
		synctest.Wait()
		if session.IsDestroyed() || manager.Count() != 1 {
			t.Fatal("新しい期限より前に破棄されました")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !session.IsDestroyed() || manager.Count() != 0 {
			t.Fatal("新しい期限で破棄されませんでした")
		}
	})
}

// sessionTimerEncoder は終了を明示的に許可するまで生存する。Cancel の回数と Run 終了を分離する。
type sessionTimerEncoder struct {
	release chan struct{}
	cancels atomic.Int32
}

func (e *sessionTimerEncoder) Run(context.Context, int) error {
	<-e.release
	return nil
}

func (e *sessionTimerEncoder) Cancel() { e.cancels.Add(1) }

// TestSessionTimerDestroyShutdownAndReplacement は破棄再入・期限後の KeepAlive・同名新世代を検証する。
// 終了待機の 500ms と manager の登録解除順序は変更せず、古いタスクが新世代を kill しないことも確認する。
func TestSessionTimerDestroyShutdownAndReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(testLogger())
		manager.SessionTimeout = 120 * time.Millisecond
		old := newTestSession(t, manager, "generation", testProgram(60, "MPEG-TS"), "FFmpeg", nil)
		old.GetVirtualPlaylist("k", "primary")
		future := old.Segments()[0].future
		encoder := &sessionTimerEncoder{release: make(chan struct{})}
		// 失敗経路でも bubble 内の待機 goroutine を残さない。
		defer func() {
			select {
			case <-encoder.release:
			default:
				close(encoder.release)
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		task := &encodingTask{encoder: encoder, cancel: cancel, done: make(chan struct{})}
		old.mu.Lock()
		old.currentTask = task
		old.mu.Unlock()
		go old.runTask(ctx, task, 0)

		// 期限に破棄開始するが、終了待機中は従来どおり manager に登録を保持する。
		time.Sleep(manager.SessionTimeout)
		synctest.Wait()
		if !old.IsDestroyed() || manager.Lookup(old.SessionID) != old || encoder.cancels.Load() != 1 || ctx.Err() == nil {
			t.Fatal("期限で破棄開始・Cancel・登録保持が行われませんでした")
		}
		old.KeepAlive()
		for range 4 {
			go old.Destroy()
		}
		synctest.Wait()
		if encoder.cancels.Load() != 1 || !old.IsDestroyed() {
			t.Fatal("破棄再入または KeepAlive が終了処理を再始動しました")
		}
		time.Sleep(encoderShutdownTimeout - time.Nanosecond)
		synctest.Wait()
		if manager.Count() != 1 {
			t.Fatal("エンコーダー終了待機期限より前に登録解除されました")
		}
		select {
		case <-future.done:
			t.Fatal("終了待機中にセグメント待機者を早期解放しました")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if manager.Count() != 0 || len(old.Segments()) != 0 || !future.resolved || len(future.data) != 0 {
			t.Fatal("終了待機期限で登録解除・セグメント解放が行われませんでした")
		}

		// 同名の新世代を登録した後に、旧タスクの遅延終了と Destroy 再入を到着させる。
		fresh := newTestSession(t, manager, old.SessionID, testProgram(60, "MPEG-TS"), "FFmpeg", nil)
		fresh.GetVirtualPlaylist("k", "primary")
		fresh.Segments()[0].SetEncoded([]byte("new generation"))
		freshEncoder := &sessionTimerEncoder{release: make(chan struct{})}
		defer func() {
			select {
			case <-freshEncoder.release:
			default:
				close(freshEncoder.release)
			}
		}()
		freshCtx, freshCancel := context.WithCancel(context.Background())
		freshTask := &encodingTask{encoder: freshEncoder, cancel: freshCancel, done: make(chan struct{})}
		fresh.mu.Lock()
		fresh.currentTask = freshTask
		fresh.mu.Unlock()
		go fresh.runTask(freshCtx, freshTask, 0)
		close(encoder.release)
		old.Destroy()
		old.KeepAlive()
		synctest.Wait()
		if manager.Count() != 1 || manager.Lookup(old.SessionID) != fresh || fresh.IsDestroyed() || freshEncoder.cancels.Load() != 0 || freshCtx.Err() != nil {
			t.Fatal("旧世代の終了処理が新世代を破棄または Cancel しました")
		}
		if data, err := fresh.GetSegment(context.Background(), 0, "primary"); err != nil || string(data) != "new generation" {
			t.Fatalf("旧世代の終了が新世代のデータを変更しました: %q, %v", data, err)
		}
		close(freshEncoder.release)
		fresh.Destroy()
		synctest.Wait()
	})
}

// TestSessionDestroyDetachedDoesNotCancelTask は登録解除済み旧セッションへの明示 Destroy の既存契約を固定する。
func TestSessionDestroyDetachedDoesNotCancelTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(testLogger())
		old := newTestSession(t, manager, "detached", testProgram(60, "MPEG-TS"), "FFmpeg", nil)
		encoder := &sessionTimerEncoder{release: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		old.currentTask = &encodingTask{encoder: encoder, cancel: cancel, done: make(chan struct{})}
		manager.unregister(old)
		fresh := newTestSession(t, manager, old.SessionID, testProgram(60, "MPEG-TS"), "FFmpeg", nil)
		old.Destroy()
		synctest.Wait()
		if !old.IsDestroyed() || encoder.cancels.Load() != 0 || ctx.Err() != nil || manager.Lookup(old.SessionID) != fresh || fresh.IsDestroyed() {
			t.Fatal("登録解除済みセッションの Destroy がタスクまたは新世代へ干渉しました")
		}
	})
}
