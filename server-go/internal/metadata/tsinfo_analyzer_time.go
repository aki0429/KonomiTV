package metadata

import (
	"bufio"
	"context"
	"io"
	"math"
	"math/big"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// tsinfoRecordingTime never mixes another program's PCR with the selected PMT.
// A discontinuity before the first usable TOT leaves the file-origin clock
// unknown; retain caller timestamps rather than infer a time across that epoch.
func tsinfoRecordingTime(ctx context.Context, r io.ReadSeeker, end int64, pcrPID uint16, duration float64) (*time.Time, *time.Time) {
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration >= float64(math.MaxInt64)/float64(time.Second) {
		return nil, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, nil
	}
	reader := bufio.NewReaderSize(io.LimitReader(r, min(end, tsinfoWindowBytes)), 32<<10)
	assembler := psi.NewAssembler(psi.AssemblerOptions{PIDs: []uint16{0x14}, PCRPID: &pcrPID})
	var firstTicks *uint64
	var offset int64
	for ctx.Err() == nil {
		raw, err := reader.Peek(psi.PacketSize)
		if err != nil {
			return nil, nil
		}
		if raw[0] != 0x47 {
			_, _ = reader.Discard(1)
			offset++
			assembler.Reset()
			if firstTicks != nil {
				return nil, nil
			}
			continue
		}
		packet, err := psi.ParsePacket(raw, offset)
		if err != nil {
			assembler.ResetPID(packet.PID)
			if packet.PID == pcrPID {
				return nil, nil
			}
		} else {
			if packet.PID == pcrPID {
				if packet.Discontinuity && firstTicks != nil {
					return nil, nil
				}
				if packet.PCR != nil && firstTicks == nil {
					ticks := packet.PCR.Ticks()
					firstTicks = &ticks
				}
			}
			sections, _ := assembler.Push(packet)
			for _, section := range sections {
				tot, err := psi.DecodeTOT(section)
				if err != nil || !tot.ClockValid || firstTicks == nil || section.PCRAtPUSI == nil || section.PCRAtPUSI.PID != pcrPID {
					continue
				}
				snapshot := section.PCRAtPUSI.UnwrappedTicks
				if snapshot < *firstTicks {
					return nil, nil
				}
				elapsedTicks := snapshot - *firstTicks
				// A TOT inside the recording cannot be farther from its first
				// PCR than the recording duration. Compare exact tick seconds
				// to the supplied binary float, without nanosecond truncation
				// or slack that could turn an unannounced reset into a wrap.
				seconds := new(big.Rat).SetFrac(new(big.Int).SetUint64(elapsedTicks), big.NewInt(27000000))
				if seconds.Cmp(new(big.Rat).SetFloat64(duration)) > 0 {
					return nil, nil
				}
				// floor(ticks * 1e9 / 27e6), splitting first so neither
				// intermediate multiplication nor time.Duration can overflow.
				whole, remainder := elapsedTicks/27000000, elapsedTicks%27000000
				fraction := remainder * uint64(time.Second) / 27000000
				if whole > uint64(math.MaxInt64)/uint64(time.Second) {
					return nil, nil
				}
				nanoseconds := whole*uint64(time.Second) + fraction
				if nanoseconds > uint64(math.MaxInt64) {
					return nil, nil
				}
				elapsed := time.Duration(nanoseconds)
				start := tot.Clock.Add(-elapsed)
				finish := start.Add(time.Duration(duration * float64(time.Second)))
				return &start, &finish
			}
		}
		_, _ = reader.Discard(psi.PacketSize)
		offset += psi.PacketSize
	}
	return nil, nil
}
