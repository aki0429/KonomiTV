package psi

import (
	"context"
	"errors"
	"io"
)

type ScanOptions struct {
	MaxBytes    int64
	MaxPackets  int64
	MaxSections int64
	PIDs        []uint16
	PCRPID      *uint16
}
type ScanStats struct {
	BytesRead        int64
	Packets          int64
	Sections         int64
	InvalidPackets   int64
	InvalidSections  int64
	ContinuityErrors int64
	DiscardedBytes   int64
	LimitReached     bool
}

// isOnlyEOF accepts wrapped EOF without swallowing non-EOF siblings in Join.
func isOnlyEOF(err error) bool {
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
			if !isOnlyEOF(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isOnlyEOF(e.Unwrap())
	default:
		return errors.Is(err, io.EOF)
	}
}

func Scan(ctx context.Context, r io.Reader, opts ScanOptions, visit func(Section) error) (ScanStats, error) {
	return scan(ctx, r, 0, opts, visit)
}

func ScanWindow(ctx context.Context, r io.ReadSeeker, offset int64, opts ScanOptions, visit func(Section) error) (ScanStats, error) {
	if ctx == nil || r == nil || visit == nil || offset < 0 || opts.MaxBytes <= 0 || opts.MaxBytes > int64(^uint64(0)>>1)-offset || opts.MaxPackets < 0 || opts.MaxSections < 0 {
		return ScanStats{}, ErrOptions
	}
	if err := ctx.Err(); err != nil {
		return ScanStats{}, err
	}
	pos, err := r.Seek(offset, io.SeekStart)
	if err != nil {
		return ScanStats{}, err
	}
	if pos != offset {
		return ScanStats{}, ErrOptions
	}
	return scan(ctx, r, offset, opts, visit)
}

func scan(ctx context.Context, r io.Reader, baseOffset int64, opts ScanOptions, visit func(Section) error) (ScanStats, error) {
	var stats ScanStats
	if ctx == nil || r == nil || visit == nil || opts.MaxBytes <= 0 || opts.MaxPackets < 0 || opts.MaxSections < 0 {
		return stats, ErrOptions
	}
	a := NewAssembler(AssemblerOptions{PIDs: opts.PIDs, PCRPID: opts.PCRPID})
	var buf []byte
	scratch := make([]byte, PacketSize*128)
	offset := baseOffset
	var readError error
	// An observed reader failure is never hidden by buffered packet/section caps.
	// Context or visitor failures remain primary; Join preserves both for Is.
	finish := func(primary error) (ScanStats, error) {
		if primary == nil {
			primary = ctx.Err()
		}
		if primary == nil {
			return stats, readError
		}
		if readError == nil {
			return stats, primary
		}
		return stats, errors.Join(primary, readError)
	}
	eof, locked := false, false
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		if len(buf) >= PacketSize {
			if locked && buf[0] != 0x47 {
				locked = false
				a.Reset()
			}
			if !locked {
				for len(buf) >= PacketSize {
					candidate := buf[0] == 0x47
					if candidate && len(buf) < 2*PacketSize && !eof {
						break
					}
					if candidate && len(buf) > PacketSize && !(eof && len(buf) < 2*PacketSize) {
						candidate = buf[PacketSize] == 0x47
					}
					if candidate {
						if eof && len(buf) < 2*PacketSize {
							_, err := ParsePacket(buf[:PacketSize], offset)
							candidate = err == nil
						}
						if candidate {
							locked = true
							break
						}
					}
					buf = buf[1:]
					offset++
					stats.DiscardedBytes++
				}
			}
			if locked && len(buf) >= PacketSize {
				p, err := ParsePacket(buf[:PacketSize], offset)
				stats.Packets++
				if err != nil {
					stats.InvalidPackets++
					a.ResetPID(p.PID)
				} else {
					sections, err := a.Push(p)
					invalid, continuity := assemblyProblemCounts(err)
					stats.InvalidSections += invalid
					stats.ContinuityErrors += continuity
					for _, s := range sections {
						if err := ctx.Err(); err != nil {
							return finish(err)
						}
						if err := visit(s); err != nil {
							return finish(err)
						}
						stats.Sections++
						if opts.MaxSections > 0 && stats.Sections >= opts.MaxSections {
							stats.LimitReached = true
							return finish(nil)
						}
					}
				}
				buf = buf[PacketSize:]
				offset += PacketSize
				if opts.MaxPackets > 0 && stats.Packets >= opts.MaxPackets {
					stats.LimitReached = true
					return finish(nil)
				}
				continue
			}
		}
		if eof {
			stats.DiscardedBytes += int64(len(buf))
			return finish(nil)
		}
		if stats.BytesRead == opts.MaxBytes {
			eof = true
			stats.LimitReached = true
			continue
		}
		n := int64(len(scratch))
		if n > opts.MaxBytes-stats.BytesRead {
			n = opts.MaxBytes - stats.BytesRead
		}
		count, err := r.Read(scratch[:n])
		if count == 0 && err == nil {
			emptyReads++
		} else {
			emptyReads = 0
		}
		if emptyReads >= 100 {
			if err := ctx.Err(); err != nil {
				return finish(err)
			}
			return stats, io.ErrNoProgress
		}
		stats.BytesRead += int64(count)
		buf = append(buf, scratch[:count]...)
		if err != nil {
			eof = true
			if !isOnlyEOF(err) {
				readError = err
			}
		}
	}
}
