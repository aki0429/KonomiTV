package frameextract

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Request は一つの入力を一度開いて処理する抽出要求。出力先は親が作成した一時ディレクトリ。
type Request struct {
	LibraryDirectory string    `json:"library_directory"`
	FilePath         string    `json:"file_path"`
	ContainerFormat  string    `json:"container_format"`
	Offsets          []float64 `json:"offsets"`
	OutputDirectory  string    `json:"output_directory"`
}

// Result は抽出済み PNG の件数と、黒画像への置換件数。
type Result struct {
	Frames, BlackFrames int
	LibraryVersion      string
}

type container interface {
	Info() StreamInfo
	SeekTimestamp(int64) error
	FirstI(context.Context, int) (*RGB, int, error)
	Close()
}

// Extract は本家の通常抽出処理。候補ごとに 20000 パケット、連続10失敗で残りを黒画像にする。
// emit は候補順に呼ぶため、全フレームのメモリ保持は不要。
func (l *Library) Extract(ctx context.Context, request Request, emit func(int, *RGB) error) (Result, error) {
	result, err := extract(ctx, request, func() (container, error) { return l.Open(request.FilePath, request.ContainerFormat) }, emit)
	result.LibraryVersion = l.Version()
	return result, err
}

func extract(ctx context.Context, request Request, open func() (container, error), emit func(int, *RGB) error) (Result, error) {
	var result Result
	d, err := open()
	if err != nil {
		return result, err
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	failed := 0
	for index, offset := range request.Offsets {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var frame *RGB
		var frameErr error
		if failed < 10 {
			info := d.Info()
			timeBase := float64(info.TimeBaseNumerator) / float64(info.TimeBaseDenominator)
			if info.TimeBaseNumerator == 0 || info.TimeBaseDenominator == 0 {
				if request.ContainerFormat == "MPEG-TS" {
					timeBase = 1.0 / 90000
				} else {
					frameErr = &Error{Operation: "seek thumbnail input", Code: avInvalidData, Detail: "missing time base for non-TS container"}
				}
			}
			start := info.StartTime
			if start == avNoTimestamp {
				start = 0
			}
			if frameErr == nil {
				timestamp := float64(start) + offset/timeBase
				if math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp >= math.MaxInt64 || timestamp < math.MinInt64 {
					return result, fmt.Errorf("invalid thumbnail seek timestamp")
				}
				frameErr = d.SeekTimestamp(int64(timestamp))
			}
			if frameErr == nil {
				frame, _, frameErr = d.FirstI(ctx, 20000)
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if frameErr != nil {
			var native *Error
			if !errors.As(frameErr, &native) {
				return result, frameErr
			}
		}
		if frame == nil || frameErr != nil {
			frame = &RGB{Width: 480, Height: 270, Pixels: make([]byte, 480*270*3), PTS: avNoTimestamp}
			result.BlackFrames++
			failed++
			if frameErr != nil && failed < 10 {
				d.Close()
				d, err = open()
				if err != nil {
					return result, err
				}
			}
		} else {
			frame, err = ResizeArea(frame, 480, 270)
			if err != nil {
				return result, err
			}
			failed = 0
		}
		if err := emit(index, frame); err != nil {
			return result, err
		}
		result.Frames++
	}
	return result, nil
}
