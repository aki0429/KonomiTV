package psi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

type reviewReadErrorWithData struct {
	packet []byte
	calls  int
	err    error
}

func (r *reviewReadErrorWithData) Read(b []byte) (int, error) {
	r.calls++
	if r.calls > 1 {
		return 0, r.err
	}
	return copy(b, r.packet), r.err
}

func TestReviewReadErrorReturnedEvenAtPacketLimit(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	failure := errors.New("synthetic disk read error")
	for _, opts := range []ScanOptions{
		{MaxBytes: 188},
		{MaxBytes: 188, MaxPackets: 1},
		{MaxBytes: 188, MaxSections: 1},
		{MaxBytes: 1000},
		{MaxBytes: 1000, MaxPackets: 1},
		{MaxBytes: 1000, MaxSections: 1},
	} {
		r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: failure}
		stats, err := Scan(context.Background(), r, opts, func(Section) error { return nil })
		if !errors.Is(err, failure) || stats.Packets != 1 || stats.Sections != 1 || r.calls != 1 {
			t.Errorf("opts=%+v expected observed reader error, actual stats=%+v calls=%d err=%v", opts, stats, r.calls, err)
		}
		if (opts.MaxPackets > 0 || opts.MaxSections > 0) && !stats.LimitReached {
			t.Error("missing limit flag")
		}
	}
}

func TestReviewEOFWithDataStillEndsNormallyAtLimits(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	for _, opts := range []ScanOptions{{MaxBytes: 188, MaxPackets: 1}, {MaxBytes: 188, MaxSections: 1}} {
		r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: io.EOF}
		stats, err := Scan(context.Background(), r, opts, func(Section) error { return nil })
		if err != nil || !stats.LimitReached || stats.Sections != 1 {
			t.Fatalf("EOF/limit: %+v err=%v", stats, err)
		}
	}
}

func TestReviewVisitorErrorRetainsObservedReadError(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	readerError := errors.New("synthetic reader error")
	visitorError := errors.New("synthetic visitor error")
	r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: readerError}
	stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188, MaxSections: 1}, func(Section) error { return visitorError })
	if !errors.Is(err, visitorError) || !errors.Is(err, readerError) || stats.Sections != 0 || stats.LimitReached {
		t.Fatalf("expected visitor priority plus observed reader error: %+v err=%v", stats, err)
	}
}

func TestReviewContextCancellationRetainsObservedReadErrorAtLimit(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	readerError := errors.New("synthetic reader error")
	for _, opts := range []ScanOptions{{MaxBytes: 188, MaxSections: 1}, {MaxBytes: 188, MaxPackets: 1}, {MaxBytes: 188}} {
		ctx, cancel := context.WithCancel(context.Background())
		r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: readerError}
		stats, err := Scan(ctx, r, opts, func(Section) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) || !errors.Is(err, readerError) || stats.Sections != 1 {
			t.Errorf("expected cancellation plus observed read error at limit: opts=%+v stats=%+v err=%v", opts, stats, err)
		}
		cancel()
	}
}

func TestReviewVisitorFailureHasPriorityWhenItAlsoCancels(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	readError := errors.New("synthetic reader failure")
	visitorError := errors.New("synthetic visitor failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: readError}
	_, err := Scan(ctx, r, ScanOptions{MaxBytes: 188, MaxSections: 1}, func(Section) error { cancel(); return visitorError })
	if !errors.Is(err, visitorError) || !errors.Is(err, readError) || errors.Is(err, context.Canceled) {
		t.Fatalf("visitor failure not primary over callback cancellation: %v", err)
	}
}

func TestReviewReadErrorJoinedWithEOFMustNotBeDiscarded(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	failure := errors.New("synthetic error joined with EOF")
	observed := errors.Join(io.EOF, failure)
	r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: observed}
	stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188, MaxSections: 1}, func(Section) error { return nil })
	if !errors.Is(err, failure) || !errors.Is(err, io.EOF) || !stats.LimitReached || stats.Sections != 1 {
		t.Fatalf("EOF hid simultaneous non-EOF failure: %+v err=%v", stats, err)
	}
}

func TestReviewEOFOnlyErrorTreesStillEndNormally(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	for _, observed := range []error{io.EOF, fmt.Errorf("wrapped: %w", io.EOF), errors.Join(io.EOF, io.EOF)} {
		r := &reviewReadErrorWithData{packet: payloadFixture(0, 0, true, append([]byte{0}, raw...)), err: observed}
		stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188, MaxSections: 1}, func(Section) error { return nil })
		if err != nil || stats.Sections != 1 || !stats.LimitReached {
			t.Fatalf("EOF-only tree: %+v err=%v", stats, err)
		}
	}
}
