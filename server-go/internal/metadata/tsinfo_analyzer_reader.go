package metadata

import (
	"context"
	"errors"
	"io"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// Cache each bounded source window once. PAT/SDT/NIT/TOT share the beginning;
// PMT/PID frequency/EIT share the 20% window. Source I/O is at most 64 MiB for
// normal analysis, regardless of table absence or number of PAT services.
type tsinfoReadWindow struct {
	offset int64
	data   []byte
}
type tsinfoWindowReader struct {
	windows  []tsinfoReadWindow
	position int64
}

func tsinfoReadWindows(ctx context.Context, source io.ReadSeeker, end int64) (*tsinfoWindowReader, error) {
	r := &tsinfoWindowReader{}
	for _, offset := range []int64{0, closestMultiple(end/5, psi.PacketSize)} {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n := min(tsinfoWindowBytes, end-offset)
		if n <= 0 {
			continue
		}
		if len(r.windows) > 0 && offset == 0 {
			continue
		}
		w, err := tsinfoLoadWindow(ctx, source, offset, n)
		if err != nil {
			return nil, err
		}
		r.windows = append(r.windows, w)
	}
	return r, nil
}

// EOF is normal only when every leaf in the error tree is EOF. An Is match
// alone would erase storage errors returned alongside EOF by errors.Join.
func tsinfoOnlyEOF(err error) bool {
	if err == nil {
		return false
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !tsinfoOnlyEOF(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return tsinfoOnlyEOF(e.Unwrap())
	default:
		return errors.Is(err, io.EOF)
	}
}

func tsinfoLoadWindow(ctx context.Context, source io.ReadSeeker, offset, n int64) (tsinfoReadWindow, error) {
	if ctx.Err() != nil {
		return tsinfoReadWindow{}, ctx.Err()
	}
	pos, err := source.Seek(offset, io.SeekStart)
	if err != nil {
		return tsinfoReadWindow{}, err
	}
	if pos != offset {
		return tsinfoReadWindow{}, psi.ErrOptions
	}
	data := make([]byte, n)
	used := 0
	for used < len(data) {
		if ctx.Err() != nil {
			return tsinfoReadWindow{}, ctx.Err()
		}
		count, err := source.Read(data[used:min(used+(64<<10), len(data))])
		used += count
		if err != nil {
			if tsinfoOnlyEOF(err) {
				break
			}
			return tsinfoReadWindow{}, err
		}
		if count == 0 {
			return tsinfoReadWindow{}, io.ErrNoProgress
		}
	}
	return tsinfoReadWindow{offset: offset, data: data[:used]}, nil
}

func (r *tsinfoWindowReader) Read(b []byte) (int, error) {
	// Prefer the closest window origin if the beginning and 20% overlap.
	for i := len(r.windows) - 1; i >= 0; i-- {
		w := r.windows[i]
		if r.position >= w.offset && r.position < w.offset+int64(len(w.data)) {
			n := copy(b, w.data[r.position-w.offset:])
			r.position += int64(n)
			return n, nil
		}
	}
	return 0, io.EOF
}
func (r *tsinfoWindowReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekCurrent {
		offset += r.position
	} else if whence != io.SeekStart {
		return 0, psi.ErrOptions
	}
	if offset < 0 {
		return 0, psi.ErrOptions
	}
	r.position = offset
	return offset, nil
}
