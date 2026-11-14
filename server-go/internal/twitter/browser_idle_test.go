package twitter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedComposeBackend struct {
	*fakeBackend
	entered, release chan struct{}
	stopped          atomic.Int32
}

func (b *blockedComposeBackend) PostTweetViaComposeUI(ctx context.Context, request ComposeRequest) (*ComposeResult, error) {
	close(b.entered)
	<-b.release
	return &ComposeResult{ErrorMessage: stringPointer("local fixture error")}, nil
}

func (b *blockedComposeBackend) Shutdown(context.Context) error { b.stopped.Add(1); return nil }

func TestBrowserIdleShutdownDoesNotInterruptCompose(t *testing.T) {
	backend := &blockedComposeBackend{fakeBackend: &fakeBackend{isSetupComplete: true, processAlive: true}, entered: make(chan struct{}), release: make(chan struct{})}
	client := NewDetachedClient(&Account{ID: 1}, backend, nil)
	var clock atomic.Int64
	clock.Store(100)
	client.SetNowForTesting(func() time.Time { return time.Unix(clock.Load(), 0) })
	var callbacks []func()
	client.SetShutdownSchedulerForTesting(func(_ time.Duration, callback func()) { callbacks = append(callbacks, callback) })
	client.KeepAlive()
	if len(callbacks) != 1 {
		t.Fatal("initial idle shutdown was not scheduled")
	}
	old := callbacks[0]
	done := make(chan struct{})
	go func() { client.CreateTweet(context.Background(), "local", nil, nil); close(done) }()
	<-backend.entered
	clock.Store(200)
	old()
	client.KeepAlive()
	if backend.stopped.Load() != 0 || len(callbacks) != 1 {
		t.Fatal("idle callback was active during the compose flow")
	}
	close(backend.release)
	<-done
	if len(callbacks) != 2 {
		t.Fatal("compose completion did not reschedule idle shutdown")
	}
	clock.Store(300)
	old()
	if backend.stopped.Load() != 0 {
		t.Fatal("an obsolete idle callback stopped the browser")
	}
	callbacks[1]()
	if backend.stopped.Load() != 1 {
		t.Fatal("the current idle callback did not stop the inactive browser")
	}
}

func TestBrowserClientConcurrentAccountRefresh(t *testing.T) {
	backend := NewBrowserBackend(&Account{ID: 1}, BrowserOptions{})
	client := NewDetachedClient(&Account{ID: 1}, backend, nil)
	var group sync.WaitGroup
	for worker := range 10 {
		group.Go(func() {
			for index := range 50 {
				client.SetAccount(&Account{ID: 1, ScreenName: fmt.Sprintf("fixture_%d_%d", worker, index)})
				_ = client.LogPrefix()
				client.finishRequest(context.Background())
				if client.Account().ID != 1 {
					t.Error("account identity changed")
				}
			}
		})
	}
	group.Wait()
	actual := backend.(*BrowserBackend)
	actual.mu.RLock()
	defer actual.mu.RUnlock()
	if actual.account.ScreenName != client.Account().ScreenName {
		t.Fatal("browser and client finished with different account snapshots")
	}
}

func TestBrowserRebindClosesPreviousInstance(t *testing.T) {
	ResetInstances()
	defer ResetInstances()
	old, fresh := &fakeBackend{isSetupComplete: true}, &fakeBackend{isSetupComplete: true}
	GetInstance(&Account{ID: 20}, func(*Account) Backend { return old }, nil)
	temporaryID := int64(-1)
	temporary := GetInstance(&Account{ID: temporaryID}, func(*Account) Backend { return fresh }, nil)
	actual := RebindInstance(&temporaryID, &Account{ID: 20}, nil, nil)
	if actual != temporary || old.shutdownCalls != 1 || fresh.shutdownCalls != 0 {
		t.Fatal("rebinding leaked or closed the wrong browser")
	}
	if _, exists := GetInstanceIfExists(temporaryID); exists {
		t.Fatal("temporary instance remained registered")
	}
	if got, exists := GetInstanceIfExists(20); !exists || got != temporary {
		t.Fatal("rebound instance is not registered")
	}
}
