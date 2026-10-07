package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

var errTSInfoComplete = errors.New("requested TS metadata collected")

// Some service-filtering recorders write zero PAT PID reserved bits, then
// correctly recompute CRC. Accept that field-level quirk only after the original
// section passes CRC/length validation; never repair transport corruption.
func tsinfoDecodePAT(s psi.Section) (*psi.PAT, error) {
	p, err := psi.DecodePAT(s)
	if err == nil {
		return p, nil
	}
	if psi.ValidateSection(s.Raw) != nil || len(s.Raw) < 12 || s.Raw[0] != 0 || (len(s.Raw)-12)%4 != 0 {
		return nil, err
	}
	b := bytes.Clone(s.Raw)
	for i := 8; i < len(b)-4; i += 4 {
		b[i+2] |= 0xe0
	}
	binary.BigEndian.PutUint32(b[len(b)-4:], psi.CRC32MPEG2(b[:len(b)-4]))
	s.Raw = b
	return psi.DecodePAT(s)
}

func tsinfoPMTs(ctx context.Context, r io.ReadSeeker, end int64, pat *psi.PAT) (map[uint16]*psi.PMT, error) {
	pids := make([]uint16, 0, len(pat.Programs))
	allowed := make(map[uint16]uint16, len(pat.Programs))
	for _, program := range pat.Programs {
		pids = append(pids, program.PMTPID)
		allowed[program.ServiceID] = program.PMTPID
	}
	found := make(map[uint16]*psi.PMT)
	checks := 0
	err := tsinfoScan(ctx, r, end, closestMultiple(end/5, psi.PacketSize), pids, func(s psi.Section) error {
		pmt, err := psi.DecodePMT(s)
		if err != nil || !pmt.Header.Current {
			return nil
		}
		checks++
		pid, ok := allowed[pmt.ServiceID]
		if ok && pid == s.PID && found[pmt.ServiceID] == nil && len(pmt.VideoPIDs)+len(pmt.AudioPIDs) > 0 {
			found[pmt.ServiceID] = pmt
		}
		if len(found) == len(allowed) || checks >= len(allowed)*3 {
			return errTSInfoComplete
		}
		return nil
	})
	if errors.Is(err, errTSInfoComplete) {
		err = nil
	}
	return found, err
}

func tsinfoSelectService(ctx context.Context, r io.ReadSeeker, end int64, pat *psi.PAT) (*uint16, error) {
	pmts, err := tsinfoPMTs(ctx, r, end, pat)
	if err != nil || len(pmts) == 0 || end < 100*psi.PacketSize {
		return nil, err
	}
	offset := closestMultiple(end/5, psi.PacketSize)
	n := min(closestMultiple(6<<20, psi.PacketSize), end-offset)
	if n < 100*psi.PacketSize {
		return nil, nil
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, n)
	used := 0
	for used < len(data) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := r.Read(data[used:min(used+64<<10, len(data))])
		used += m
		if err != nil {
			if tsinfoOnlyEOF(err) {
				break
			}
			return nil, err
		}
		if m == 0 {
			return nil, io.ErrNoProgress
		}
	}
	counts := map[uint16]int{}
	for i := 0; i+psi.PacketSize <= used; {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if data[i] != 0x47 {
			i++
			continue
		}
		p, err := psi.ParsePacket(data[i:i+psi.PacketSize], offset+int64(i))
		if err == nil {
			counts[p.PID]++
		}
		i += psi.PacketSize
	}
	bestScore, bestVideo, bestAudio := 0, -1, -1
	var best *uint16
	for _, service := range pat.Programs {
		pmt := pmts[service.ServiceID]
		if pmt == nil {
			continue
		}
		v, au := 0, 0
		for _, pid := range pmt.VideoPIDs {
			v += counts[pid]
		}
		for _, pid := range pmt.AudioPIDs {
			au += counts[pid]
		}
		score := 2*v + au
		if score > bestScore || (score == bestScore && score > 0 && (v > bestVideo || (v == bestVideo && au > bestAudio))) {
			sid := service.ServiceID
			best, bestScore, bestVideo, bestAudio = &sid, score, v, au
		}
	}
	return best, nil
}
