package stream

import (
	"io"
	"log/slog"
	"sync"
	"testing"
)

// TestImmediateEncoderFailureDisconnectsRegisteredClient は起動中の即時失敗を取りこぼさない。
func TestImmediateEncoderFailureDisconnectsRegisteredClient(t *testing.T) {
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.SetTaskStarter(func(live *LiveStream) {
		live.SetStatus("Offline", "synthetic startup failure", true)
		live.DisconnectAll()
	})
	live := manager.GetLiveStream("gr011", "720p", StreamEncodingOptions{})
	client := manager.Connect(live, "mpegts")
	select {
	case _, ok := <-client.Queue():
		if ok {
			t.Fatal("failed encoder supplied unexpected data")
		}
	default:
		t.Fatal("encoder failure was missed because the client was not registered yet")
	}
	if live.GetStatus().ClientCount != 0 {
		t.Fatal("failed encoder leaked a viewer")
	}
	live.Disconnect(client)
}

// TestConcurrentClientCloseAndWrite はキュー満杯・切断・終了通知が重なっても panic しない。
func TestConcurrentClientCloseAndWrite(t *testing.T) {
	for range 500 {
		client := &LiveStreamClient{ClientType: "mpegts", queue: make(chan []byte, 1)}
		client.queue <- []byte{0x47}
		var workers sync.WaitGroup
		for range 4 {
			workers.Go(func() { client.closeQueue() })
			workers.Go(func() { client.writeStreamData([]byte{0x47}) })
		}
		workers.Wait()
		client.closeQueue()
		for range client.Queue() {
		}
	}
}
