package psi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type countedReader struct {
	r     io.Reader
	n     int64
	chunk int
}

func (r *countedReader) Read(b []byte) (int, error) {
	if r.chunk > 0 && len(b) > r.chunk {
		b = b[:r.chunk]
	}
	n, e := r.r.Read(b)
	r.n += int64(n)
	return n, e
}
func TestScanReaderIsBounded(t *testing.T) {
	raw := syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0})
	data := append(payloadFixture(0, 0, true, append([]byte{0}, raw...)), payloadFixture(0, 1, true, append([]byte{0}, raw...))...)
	r := &countedReader{r: bytes.NewReader(data), chunk: 7}
	var got []Section
	stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188}, func(s Section) error { got = append(got, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if r.n != 188 || stats.BytesRead != 188 || stats.Packets != 1 || stats.Sections != 1 || !stats.LimitReached || len(got) != 1 || got[0].PacketOffset != 0 || !bytes.Equal(got[0].Raw, raw) {
		t.Fatalf("bounded scan %+v read=%d sections=%d", stats, r.n, len(got))
	}
	_ = errors.Is
}

func TestScanRecoversSync(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	first := payloadFixture(0, 0, true, append([]byte{0}, raw...))
	second := payloadFixture(0, 1, true, append([]byte{0}, raw...))
	data := []byte{0x47, 0, 0, 0, 0, 0, 0}
	data = append(data, first...)
	data = append(data, second...)
	data = append(data, 0, 0, 0)
	data = append(data, first...)
	data = append(data, second...)
	data = append(data, 0x47, 0, 0)
	r := &countedReader{r: bytes.NewReader(data), chunk: 13}
	var offsets []int64
	stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: int64(len(data) + 100)}, func(s Section) error { offsets = append(offsets, s.PacketOffset); return nil })
	if err != nil || len(offsets) != 4 || offsets[0] != 7 || offsets[1] != 195 || offsets[2] != 386 || offsets[3] != 574 || stats.DiscardedBytes != 13 {
		t.Fatalf("sync offsets=%v stats=%+v err=%v", offsets, stats, err)
	}
	stats, err = Scan(context.Background(), bytes.NewReader(first), ScanOptions{MaxBytes: 1000}, func(s Section) error { return nil })
	if err != nil || stats.Sections != 1 || stats.LimitReached {
		t.Fatalf("single EOF packet: %+v %v", stats, err)
	}
}

func TestScanCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &countedReader{r: bytes.NewReader(payloadFixture(0, 0, false, nil))}
	stats, err := Scan(ctx, r, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil })
	if !errors.Is(err, context.Canceled) || r.n != 0 || stats.Packets != 0 {
		t.Fatalf("pre-cancel: %+v %v read=%d", stats, err, r.n)
	}
	ctx, cancel = context.WithCancel(context.Background())
	raw := syntaxFixture(0, 1, nil)
	data := append(payloadFixture(0, 0, true, append([]byte{0}, raw...)), payloadFixture(0, 1, true, append([]byte{0}, raw...))...)
	stats, err = Scan(ctx, bytes.NewReader(data), ScanOptions{MaxBytes: 376}, func(s Section) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || stats.Sections != 1 {
		t.Fatalf("callback cancel: %+v %v", stats, err)
	}
}

type failReader struct{ err error }

func (r failReader) Read([]byte) (int, error) { return 0, r.err }
func TestScanOptionsLimitsAndErrors(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	packet := payloadFixture(0, 0, true, append(append([]byte{0}, raw...), raw...))
	for _, opts := range []ScanOptions{{}, {MaxBytes: -1}, {MaxBytes: 188, MaxPackets: -1}, {MaxBytes: 188, MaxSections: -1}} {
		r := &countedReader{r: bytes.NewReader(packet)}
		if _, err := Scan(context.Background(), r, opts, func(s Section) error { return nil }); !errors.Is(err, ErrOptions) || r.n != 0 {
			t.Fatalf("options: %v %d", err, r.n)
		}
	}
	stats, err := Scan(context.Background(), bytes.NewReader(append(packet, packet...)), ScanOptions{MaxBytes: 1000, MaxPackets: 1}, func(s Section) error { return nil })
	if err != nil || stats.Packets != 1 || stats.Sections != 2 || !stats.LimitReached {
		t.Fatalf("packet cap: %+v %v", stats, err)
	}
	stats, err = Scan(context.Background(), bytes.NewReader(packet), ScanOptions{MaxBytes: 1000, MaxSections: 1}, func(s Section) error { return nil })
	if err != nil || stats.Sections != 1 || !stats.LimitReached {
		t.Fatalf("section cap: %+v %v", stats, err)
	}
	expected := errors.New("reader failure")
	if _, err := Scan(context.Background(), failReader{expected}, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil }); !errors.Is(err, expected) {
		t.Fatalf("reader error %v", err)
	}
	if _, err := Scan(context.Background(), bytes.NewReader(packet), ScanOptions{MaxBytes: 188}, func(s Section) error { return expected }); !errors.Is(err, expected) {
		t.Fatalf("visitor error %v", err)
	}
	if _, err := Scan(context.Background(), nil, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil }); !errors.Is(err, ErrOptions) {
		t.Fatalf("nil reader %v", err)
	}
	if _, err := Scan(context.Background(), bytes.NewReader(packet), ScanOptions{MaxBytes: 188}, nil); !errors.Is(err, ErrOptions) {
		t.Fatalf("nil visitor %v", err)
	}
}

type failingSeeker struct{ err error }

func (r failingSeeker) Read([]byte) (int, error)       { return 0, r.err }
func (r failingSeeker) Seek(int64, int) (int64, error) { return 0, r.err }
func TestScanWindowTwentyPercent(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	packet := payloadFixture(0, 0, true, append([]byte{0}, raw...))
	data := bytes.Repeat(payloadFixture(0x1fff, 0, false, nil), 20)
	copy(data[188*4:], packet)
	r := bytes.NewReader(data)
	var got []Section
	stats, err := ScanWindow(context.Background(), r, int64(len(data)/5), ScanOptions{MaxBytes: 188}, func(s Section) error { got = append(got, s); return nil })
	if err != nil || stats.Sections != 1 || len(got) != 1 || got[0].PacketOffset != 752 || stats.BytesRead != 188 {
		t.Fatalf("20%% window: %+v %v %+v", stats, err, got)
	}
	got = nil
	data = append([]byte{0, 0, 0}, data...)
	_, err = ScanWindow(context.Background(), bytes.NewReader(data), 752, ScanOptions{MaxBytes: 376}, func(s Section) error { got = append(got, s); return nil })
	if err != nil || len(got) != 1 || got[0].PacketOffset != 755 {
		t.Fatalf("unaligned window: %+v %v", got, err)
	}
	if _, err := ScanWindow(context.Background(), r, -1, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil }); !errors.Is(err, ErrOptions) {
		t.Fatalf("negative offset %v", err)
	}
	expected := errors.New("seek error")
	if _, err := ScanWindow(context.Background(), failingSeeker{expected}, 0, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil }); !errors.Is(err, expected) {
		t.Fatalf("seek error %v", err)
	}
}

func TestScanDamagedPacketResetsAssembly(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	data := exactPayloadFixture(0, 0, true, append([]byte{0}, raw[:8]...))
	damaged := payloadFixture(0, 1, false, raw[8:])
	damaged[1] |= 0x80
	data = append(data, damaged...)
	data = append(data, payloadFixture(0, 2, false, raw[8:])...)
	data = append(data, payloadFixture(0, 3, true, append([]byte{0}, raw...))...)
	scrambled := payloadFixture(0, 4, false, nil)
	scrambled[3] |= 0x80
	data = append(data, scrambled...)
	data = append(data, payloadFixture(0, 5, true, append([]byte{0}, raw...))...)
	data = append(data, payloadFixture(0, 7, true, append([]byte{0}, raw...))...)
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 1
	data = append(data, payloadFixture(0, 8, true, append([]byte{0}, bad...))...)
	stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: int64(len(data))}, func(s Section) error { return nil })
	if err != nil || stats.Sections != 3 || stats.InvalidPackets != 2 || stats.ContinuityErrors != 1 || stats.InvalidSections != 1 {
		t.Fatalf("damaged stats: %+v %v", stats, err)
	}
}

type noProgressReader struct{ calls int }

func (r *noProgressReader) Read([]byte) (int, error) {
	r.calls++
	if r.calls > 101 {
		return 0, io.EOF
	}
	return 0, nil
}
func TestScanNoProgress(t *testing.T) {
	r := &noProgressReader{}
	_, err := Scan(context.Background(), r, ScanOptions{MaxBytes: 188}, func(s Section) error { return nil })
	if !errors.Is(err, io.ErrNoProgress) || r.calls > 100 {
		t.Fatalf("no-progress: %v calls=%d", err, r.calls)
	}
}

func TestScanSinglePacketWithTail(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	data := append([]byte{0, 0, 0}, payloadFixture(0, 0, true, append([]byte{0}, raw...))...)
	data = append(data, 1, 2, 3)
	stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: 1000}, func(s Section) error { return nil })
	if err != nil || stats.Sections != 1 || stats.DiscardedBytes != 6 {
		t.Fatalf("single tail: %+v %v", stats, err)
	}
}

// TDD: scan_test.go additions.
