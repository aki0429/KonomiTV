package stream

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type trackedPipeReader struct {
	io.Reader
	closed atomic.Bool
}

func (r *trackedPipeReader) Close() error { r.closed.Store(true); return nil }

func TestEDCBTunerPipeHandoffCancelsConnect(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 9000, true }}
	backend := fakeEDCBTunerBackend(ctrl)
	backend.NamedPipe = true
	entered := make(chan struct{})
	backend.OpenPipeStream = func(ctx context.Context, pid int) (io.ReadCloser, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	tuner := GetOrCreateEDCBTuner("old", backend)
	defer tuner.Close("", true)
	if !tuner.SetChannel(1, 2, 3, "old") {
		t.Fatal("set channel")
	}
	done := make(chan error, 1)
	go func() { _, err := tuner.ConnectContext(context.Background(), "old"); done <- err }()
	<-entered
	if !tuner.Handoff("old", "new") {
		t.Fatal("handoff")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff did not cancel pending open")
	}
	tuner.mu.Lock()
	defer tuner.mu.Unlock()
	if tuner.ownerLiveStreamID != "new" || tuner.state != EDCBTunerIdle {
		t.Fatal("old connection changed new owner state")
	}
}

func TestEDCBTunerCloseRejectsLatePipe(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 9000, true }}
	backend := fakeEDCBTunerBackend(ctrl)
	backend.NamedPipe = true
	entered, release := make(chan struct{}), make(chan struct{})
	pipe := &trackedPipeReader{Reader: strings.NewReader("TS")}
	backend.OpenPipeStream = func(ctx context.Context, pid int) (io.ReadCloser, error) {
		close(entered)
		<-release
		return pipe, nil
	}
	tuner := GetOrCreateEDCBTuner("owner", backend)
	if !tuner.SetChannel(1, 2, 3, "owner") {
		t.Fatal("set channel")
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = tuner.Connect("owner") }()
	<-entered
	tuner.Close("owner", true)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("late connection not rejected")
	}
	if !pipe.closed.Load() || !tuner.IsDisconnected() {
		t.Fatal("late pipe leaked")
	}
}
