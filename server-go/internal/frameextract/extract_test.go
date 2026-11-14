package frameextract

import (
	"context"
	"errors"
	"testing"
)

type policyContainer struct {
	info      StreamInfo
	seeks     *[]int64
	first     func() (*RGB, int, error)
	seekError error
	closes    *int
}

func (d policyContainer) Info() StreamInfo { return d.info }
func (d policyContainer) SeekTimestamp(v int64) error {
	*d.seeks = append(*d.seeks, v)
	return d.seekError
}
func (d policyContainer) FirstI(_ context.Context, limit int) (*RGB, int, error) {
	if limit != 20000 {
		panic("wrong packet limit")
	}
	return d.first()
}
func (d policyContainer) Close() { *d.closes++ }

func TestExtractionFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nativeError bool
		wantOpens   int
	}{
		{"no-frame-does-not-reopen", false, 1}, {"native-error-reopens-until-ten", true, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seeks, opens, closes := []int64{}, 0, 0
			open := func() (container, error) {
				opens++
				d := policyContainer{info: StreamInfo{TimeBaseNumerator: 1, TimeBaseDenominator: 90000, StartTime: 900}, seeks: &seeks, closes: &closes, first: func() (*RGB, int, error) { return nil, 20000, nil }}
				if tc.nativeError {
					d.seekError = &Error{Code: avInvalidData}
				}
				return d, nil
			}
			offsets := make([]float64, 14)
			for i := range offsets {
				offsets[i] = float64(i)
			}
			result, err := extract(context.Background(), Request{ContainerFormat: "MPEG-TS", Offsets: offsets}, open, func(_ int, frame *RGB) error {
				if frame.Pixels[0] != 0 || frame.Width != 480 || frame.Height != 270 {
					t.Error("invalid black frame")
				}
				return nil
			})
			if err != nil || result.Frames != 14 || result.BlackFrames != 14 || len(seeks) != 10 || opens != tc.wantOpens || closes != opens {
				t.Fatalf("result=%+v seeks=%v opens=%d closes=%d error=%v", result, seeks, opens, closes, err)
			}
			if seeks[1] != 90900 {
				t.Errorf("stream start time ignored: %v", seeks)
			}
		})
	}
}

func TestExtractionReopenFailureAndTimeBase(t *testing.T) {
	for _, format := range []string{"MPEG-TS", "MPEG-4"} {
		t.Run(format, func(t *testing.T) {
			seeks, closes, opens := []int64{}, 0, 0
			failure := errors.New("reopen input failed")
			open := func() (container, error) {
				opens++
				if opens > 1 {
					return nil, failure
				}
				return policyContainer{info: StreamInfo{StartTime: avNoTimestamp}, seeks: &seeks, closes: &closes, first: func() (*RGB, int, error) { return &RGB{Width: 1, Height: 1, Pixels: []byte{1, 2, 3}}, 1, nil }}, nil
			}
			result, err := extract(context.Background(), Request{ContainerFormat: format, Offsets: []float64{1.5}}, open, func(int, *RGB) error { return nil })
			if format == "MPEG-TS" {
				if err != nil || result.Frames != 1 || len(seeks) != 1 || seeks[0] != 135000 || opens != 1 || closes != 1 {
					t.Fatalf("TS fallback=%+v %v seeks=%v", result, err, seeks)
				}
			} else if !errors.Is(err, failure) || opens != 2 || closes != 1 || len(seeks) != 0 {
				t.Fatalf("MP4 reopen=%+v %v opens=%d closes=%d seeks=%v", result, err, opens, closes, seeks)
			}
		})
	}
}
