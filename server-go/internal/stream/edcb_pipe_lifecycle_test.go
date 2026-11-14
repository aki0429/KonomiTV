package stream

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestEDCBTunerPendingPipeCancelled(t *testing.T) {
	for _, action := range []string{"disconnect", "handoff", "close", "cancelling", "context"} {
		t.Run(action, func(t *testing.T) {
			resetEDCBTunerPool()
			started := make(chan struct{})
			ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 123, true }}
			backend := fakeEDCBTunerBackend(ctrl)
			backend.NamedPipe = true
			backend.OpenPipeStream = func(ctx context.Context, processID int) (io.ReadCloser, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			tuner := GetOrCreateEDCBTuner("a", backend)
			defer tuner.Close("", true)
			if !tuner.SetChannel(1, 2, 3, "a") {
				t.Fatal("SetChannel failed")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := tuner.ConnectContext(ctx, "a"); done <- err }()
			<-started
			switch action {
			case "disconnect":
				tuner.Disconnect("a")
			case "handoff":
				tuner.Handoff("a", "b")
			case "close":
				tuner.Close("a", false)
			case "cancelling":
				tuner.SetState(EDCBTunerCancelling)
			case "context":
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled connection: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("pipe connection did not stop")
			}
			if action == "cancelling" && tuner.GetState() != EDCBTunerCancelling {
				t.Fatal("old connection overwrote Cancelling state")
			}
		})
	}
}

func TestEDCBTunerLatePipeCannotOverwriteNewOwner(t *testing.T) {
	resetEDCBTunerPool()
	started, release := make(chan struct{}), make(chan struct{})
	oldConn, oldPeer := net.Pipe()
	defer oldPeer.Close()
	newConn, newPeer := net.Pipe()
	defer newPeer.Close()
	var attempts atomic.Int32
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 123, true }}
	backend := fakeEDCBTunerBackend(ctrl)
	backend.NamedPipe = true
	backend.OpenPipeStream = func(ctx context.Context, processID int) (io.ReadCloser, error) {
		if attempts.Add(1) == 1 {
			close(started)
			<-release
			// 接続完了とキャンセルが競合した状況を再現する。
			return oldConn, nil
		}
		return newConn, nil
	}
	tuner := GetOrCreateEDCBTuner("a", backend)
	defer tuner.Close("", true)
	if !tuner.SetChannel(1, 2, 3, "a") {
		t.Fatal("SetChannel failed")
	}
	done := make(chan error, 1)
	go func() { _, err := tuner.Connect("a"); done <- err }()
	<-started
	if !tuner.Handoff("a", "b") {
		t.Fatal("handoff failed")
	}
	reader, err := tuner.Connect("b")
	if err != nil || reader == nil {
		t.Fatalf("new owner: %v, %v", reader, err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("old owner: %v", err)
	}
	_ = oldPeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := oldPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("late old connection leaked: %v", err)
	}
	tuner.Disconnect("a")
	if tuner.GetState() != EDCBTunerActive || tuner.IsDisconnected() {
		t.Fatal("old attempt changed the new owner's state")
	}
	tuner.Disconnect("b")
	if !tuner.IsDisconnected() {
		t.Fatal("new owner could not disconnect")
	}
}
