package psi

import (
	"context"
	"errors"
	"io"
	"testing"
)

// The frozen independent review cancels during the 100th empty Read, before
// Scan checks the io.ErrNoProgress threshold.
type reviewNoProgressCanceled struct {
	calls  int
	cancel context.CancelFunc
}

func (r *reviewNoProgressCanceled) Read([]byte) (int, error) {
	r.calls++
	if r.calls == 100 && r.cancel != nil {
		r.cancel()
	}
	return 0, nil
}

func TestReviewContextPriorityAtNoProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &reviewNoProgressCanceled{cancel: cancel}
	visited := false
	stats, err := Scan(ctx, r, ScanOptions{MaxBytes: 188}, func(Section) error {
		visited = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("cancellation ignored after Read: calls=%d stats=%+v got=%v want=context canceled (without ErrNoProgress)", r.calls, stats, err)
	}
	if r.calls != 100 || visited || stats != (ScanStats{}) {
		t.Fatalf("no-progress cancellation changed stats/callback: calls=%d visited=%v stats=%+v", r.calls, visited, stats)
	}
}

func TestReviewNoProgressWithoutCancellation(t *testing.T) {
	r := &reviewNoProgressCanceled{}
	stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188}, func(Section) error {
		t.Fatal("empty reader reached visitor")
		return nil
	})
	if err != io.ErrNoProgress || r.calls != 100 || stats != (ScanStats{}) {
		t.Fatalf("uncanceled empty reads must retain ErrNoProgress: calls=%d stats=%+v err=%v", r.calls, stats, err)
	}
}
