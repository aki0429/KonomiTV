package videostream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

type offlineOracle struct {
	segment OfflineSegment
	err     error
	calls   int
}

func (e *offlineOracle) Next(context.Context) (OfflineSegment, error) {
	e.calls++
	if e.calls == 1 {
		return e.segment, e.err
	}
	return OfflineSegment{}, io.EOF
}
func (e *offlineOracle) Close() error { return nil }

// TestOfflineRejectInvalid はクライアント上限と uint32 変換前の境界を検証する。
func TestOfflineRejectInvalid(t *testing.T) {
	cases := []struct {
		name     string
		metadata OfflineMetadata
		segment  OfflineSegment
		err      error
		zero     bool
	}{
		{"metadata overflow", OfflineMetadata{FileHash: strings.Repeat("a", 1<<20)}, OfflineSegment{DurationSeconds: 1, Data: []byte{1}}, nil, true},
		{"metadata NaN", OfflineMetadata{DurationSeconds: math.NaN()}, OfflineSegment{}, nil, true},
		{"empty", OfflineMetadata{}, OfflineSegment{DurationSeconds: 1}, nil, false},
		{"duration overflow", OfflineMetadata{}, OfflineSegment{DurationSeconds: float64(math.MaxUint32)/1000 + 1, Data: []byte{1}}, nil, false},
		{"duration NaN", OfflineMetadata{}, OfflineSegment{DurationSeconds: math.NaN(), Data: []byte{1}}, nil, false},
		{"joined EOF failure", OfflineMetadata{}, OfflineSegment{}, errors.Join(io.EOF, errors.New("encoder failed")), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			e := &offlineOracle{segment: c.segment, err: c.err}
			err := WriteOffline(context.Background(), &out, c.metadata, e)
			if err == nil {
				t.Fatalf("accepted invalid record, bytes=%d", out.Len())
			}
			if c.zero && out.Len() != 0 {
				t.Fatal("metadata error started response")
			}
			if bytes.HasSuffix(out.Bytes(), []byte{255, 255, 255, 255, 0, 0, 0, 1}) {
				t.Fatal("failure wrote success terminator")
			}
		})
	}
}
