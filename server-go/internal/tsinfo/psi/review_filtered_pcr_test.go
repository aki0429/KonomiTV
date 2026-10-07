package psi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
)

// Cycle 2 fixtures migrated from the frozen independent review. The CRC and
// transport writer do not call production CRC/packet builders.
func reviewFilterCRC(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			high := crc & 0x80000000
			crc <<= 1
			if high != 0 {
				crc ^= 0x04c11db7
			}
		}
	}
	return crc
}

func reviewFilterSyntax() []byte {
	b := []byte{0, 0xb0, 9, 0, 1, 0xc1, 0, 0}
	crc := reviewFilterCRC(b)
	return append(b, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func reviewFilterPacket(pid uint16, cc byte, pusi bool, payload []byte, pcr *PCR) []byte {
	b := bytes.Repeat([]byte{0xff}, PacketSize)
	b[0], b[1], b[2], b[3] = 0x47, byte(pid>>8), byte(pid), 0x30|(cc&15)
	if pusi {
		b[1] |= 0x40
	}
	b[4] = byte(183 - len(payload))
	if b[4] > 0 {
		b[5] = 0
	}
	if pcr != nil {
		if b[4] < 7 {
			panic("fixture PCR needs adaptation space")
		}
		b[5] = 0x10
		x := pcr.Base
		b[6], b[7], b[8], b[9] = byte(x>>25), byte(x>>17), byte(x>>9), byte(x>>1)
		b[10], b[11] = byte(x&1)<<7|0x7e|byte(pcr.Extension>>8), byte(pcr.Extension)
	}
	copy(b[5+int(b[4]):], payload)
	return b
}

func reviewFilterPush(t *testing.T, a *Assembler, raw []byte, offset int64) []Section {
	t.Helper()
	out, err := a.Push(Packet{Raw: raw, Offset: offset})
	if err != nil {
		t.Fatalf("direct Push offset=%d err=%v", offset, err)
	}
	return out
}

func TestReviewPCRDuplicateOutsideSectionFilter(t *testing.T) {
	for _, disc := range []bool{false, true} {
		t.Run(fmt.Sprint(disc), func(t *testing.T) {
			clockPID := uint16(0x100)
			value := PCR{Base: 100, Extension: 1}
			raw := reviewFilterSyntax()
			a := NewAssembler(AssemblerOptions{PIDs: []uint16{0x14}, PCRPID: &clockPID})
			first := reviewFilterPacket(clockPID, 4, false, []byte{0x11, 0x22}, &value)
			if disc {
				first[5] |= 0x80
			}
			reviewFilterPush(t, a, first, 0)
			reviewFilterPush(t, a, first, 188)
			one := reviewFilterPush(t, a, reviewFilterPacket(0x14, 0, true, append([]byte{0}, raw...), nil), 376)
			if len(one) != 1 || one[0].PCRAtPUSI == nil {
				t.Fatal("missing filtered PCR")
			}
			if one[0].PCRAtPUSI.PacketOffset != 0 {
				t.Errorf("byte-identical filtered PCR retransmit created new observation: got offset=%d want=0", one[0].PCRAtPUSI.PacketOffset)
			}
			changed := reviewFilterPacket(clockPID, 4, false, []byte{0x11, 0x22}, &PCR{Base: 101, Extension: 2})
			if disc {
				changed[5] |= 0x80
			}
			reviewFilterPush(t, a, changed, 564)
			want := *a.current
			reviewFilterPush(t, a, changed, 752)
			if *a.current != want {
				t.Errorf("identical filtered duplicate changed clock: got=%+v want=%+v", *a.current, want)
			}
		})
	}
}

func TestReviewFilteredPCRDuplicateEpoch(t *testing.T) {
	for _, tc := range []struct {
		name string
		pids []uint16
	}{{"unfiltered", nil}, {"filtered", []uint16{0x14}}, {"empty", []uint16{}}} {
		t.Run(tc.name, func(t *testing.T) {
			clockPID := uint16(0x100)
			a := NewAssembler(AssemblerOptions{PIDs: tc.pids, PCRPID: &clockPID})
			raw := reviewFilterSyntax()
			first := reviewFilterPacket(clockPID, 7, false, []byte{0xff}, &PCR{Base: (1 << 33) - 1, Extension: 299})
			first[5] |= 0x80
			reviewFilterPush(t, a, first, 0)
			// Only six PCR bytes change; CC/payload/discontinuity stay identical.
			duplicate := reviewFilterPacket(clockPID, 7, false, []byte{0xff}, &PCR{Base: 1, Extension: 2})
			duplicate[5] |= 0x80
			reviewFilterPush(t, a, duplicate, 188)
			want := PCRModulus + 302
			if a.current == nil || a.current.UnwrappedTicks != want || a.current.PacketOffset != 188 {
				t.Fatalf("PCR-only same-CC duplicate reset clock epoch: got=%+v want ticks=%d offset=188", a.current, want)
			}
			before := *a.current
			reviewFilterPush(t, a, duplicate, 376)
			if *a.current != before {
				t.Fatalf("identical duplicate changed snapshot: got=%+v want=%+v", a.current, before)
			}
			got := reviewFilterPush(t, a, reviewFilterPacket(0x14, 0, true, append([]byte{0}, raw...), nil), 564)
			if tc.pids != nil && len(tc.pids) == 0 {
				if len(got) != 0 {
					t.Fatal("empty filter emitted section")
				}
				return
			}
			if len(got) != 1 || got[0].PCRAtPUSI == nil || *got[0].PCRAtPUSI != before {
				t.Fatalf("missing updated PUSI snapshot: %+v", got)
			}
		})
	}
}

type reviewFilterReader struct {
	data  []byte
	chunk int
}

func (r *reviewFilterReader) Read(b []byte) (int, error) {
	if r.chunk > 0 && len(b) > r.chunk {
		b = b[:r.chunk]
	}
	n := copy(b, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestReviewFilteredPCRClockThroughScan(t *testing.T) {
	raw := reviewFilterSyntax()
	pid := uint16(0x100)
	first := reviewFilterPacket(pid, 5, false, []byte{0xff}, &PCR{Base: (1 << 33) - 1, Extension: 299})
	first[5] |= 0x80
	duplicate := reviewFilterPacket(pid, 5, false, []byte{0xff}, &PCR{Base: 1, Extension: 2})
	duplicate[5] |= 0x80
	stream := append(bytes.Clone(first), duplicate...)
	stream = append(stream, duplicate...)
	stream = append(stream, reviewFilterPacket(0x14, 0, true, append([]byte{0}, raw...), nil)...)
	for _, chunk := range []int{1, 7, 187, 188, 376, 0} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			r := &reviewFilterReader{data: bytes.Clone(stream), chunk: chunk}
			var snapshot *PCRSnapshot
			stats, err := Scan(context.Background(), r, ScanOptions{MaxBytes: int64(len(stream)), PIDs: []uint16{0x14}, PCRPID: &pid}, func(s Section) error {
				snapshot = s.PCRAtPUSI
				return nil
			})
			if err != nil || stats.Sections != 1 || stats.Packets != 4 || stats.InvalidSections != 0 || stats.ContinuityErrors != 0 || snapshot == nil {
				t.Fatalf("scan=%+v err=%v", stats, err)
			}
			if snapshot.UnwrappedTicks != PCRModulus+302 || snapshot.PacketOffset != 188 {
				t.Fatalf("filtered PCR epoch via Scan: got=%+v want ticks=%d offset=188", snapshot, PCRModulus+302)
			}
		})
	}
}

func TestReviewFilteredPCRSkipsSectionParsingAndCCGapErrors(t *testing.T) {
	pid := uint16(0x100)
	first := reviewFilterPacket(pid, 1, true, []byte{0, 0, 0xb0, 9}, &PCR{Base: 10})
	gap := reviewFilterPacket(pid, 9, true, []byte{255}, &PCR{Base: 11}) // invalid pointer, legal packet
	badCRC := reviewFilterSyntax()
	badCRC[len(badCRC)-1] ^= 1
	bad := reviewFilterPacket(pid, 10, true, append([]byte{0}, badCRC...), nil)
	for _, pids := range [][]uint16{{0x14}, {}} {
		a := NewAssembler(AssemblerOptions{PIDs: pids, PCRPID: &pid})
		for i, packet := range [][]byte{first, gap, bad} {
			if got := reviewFilterPush(t, a, packet, int64(i*PacketSize)); len(got) != 0 || len(a.states[pid].pending) != 0 {
				t.Fatal("filtered PID parsed or retained section")
			}
		}
		data := append(append(bytes.Clone(first), gap...), bad...)
		stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: int64(len(data)), PIDs: pids, PCRPID: &pid}, func(Section) error {
			t.Fatal("filtered PID reached visitor")
			return nil
		})
		if err != nil || stats.Packets != 3 || stats.Sections != 0 || stats.InvalidSections != 0 || stats.ContinuityErrors != 0 {
			t.Fatalf("filtered gap/CRC/pointer counted: %+v err=%v", stats, err)
		}
	}
}

func TestReviewFilteredPCRAdaptationOnlyIsNotPayloadRetransmit(t *testing.T) {
	for _, pids := range [][]uint16{nil, {0x14}, {}} {
		for _, disc := range []bool{false, true} {
			pid := uint16(0x100)
			a := NewAssembler(AssemblerOptions{PIDs: pids, PCRPID: &pid})
			first := reviewFilterPacket(pid, 7, false, []byte{0xff}, &PCR{Base: (1 << 33) - 1, Extension: 299})
			reviewFilterPush(t, a, first, 0)
			adaptation := reviewFilterPacket(pid, 7, false, nil, &PCR{Base: 1, Extension: 2})
			adaptation[3] = 0x27 // same CC, adaptation only
			if disc {
				adaptation[5] |= 0x80
			}
			wantTicks := PCRModulus + 302
			if disc {
				wantTicks = 302
			}
			for _, offset := range []int64{188, 376} {
				reviewFilterPush(t, a, adaptation, offset)
				if a.current == nil || a.current.PacketOffset != offset || a.current.UnwrappedTicks != wantTicks {
					t.Fatalf("adaptation-only update incorrectly deduplicated: disc=%v got=%+v want ticks=%d offset=%d", disc, a.current, wantTicks, offset)
				}
			}
		}
	}
}

func FuzzFilteredPCRDuplicateSequence(f *testing.F) {
	f.Add(uint64((1<<33)-1), uint16(299), uint16(2), byte(15), byte(0), true, false)
	f.Add(uint64(100), uint16(255), uint16(256), byte(0), byte(2), false, true)
	f.Fuzz(func(t *testing.T, base uint64, ext1, ext2 uint16, cc, step byte, disc, pusi bool) {
		base %= 1 << 33
		ext1 %= 300
		ext2 %= 300
		cc &= 15
		deltaBase := uint64(step%4) + 1
		original := PCR{Base: base, Extension: ext1}
		updated := PCR{Base: (base + deltaBase) % (1 << 33), Extension: ext2}
		wantTicks := base*300 + deltaBase*300 + uint64(ext2)
		payload := []byte{0xff}
		if pusi {
			payload = []byte{0, 0xff}
		}
		pid := uint16(0x100)
		first := reviewFilterPacket(pid, cc, pusi, payload, &original)
		duplicate := reviewFilterPacket(pid, cc, pusi, payload, &updated)
		if disc {
			first[5] |= 0x80
			duplicate[5] |= 0x80
		}
		raw := reviewFilterSyntax()
		for _, pids := range [][]uint16{nil, {0x14}, {}} {
			a := NewAssembler(AssemblerOptions{PIDs: pids, PCRPID: &pid})
			reviewFilterPush(t, a, first, 0)
			reviewFilterPush(t, a, first, 188)
			if a.current == nil || a.current.PacketOffset != 0 || a.current.Value != original || a.current.UnwrappedTicks != base*300+uint64(ext1) {
				t.Fatalf("identical packet changed first observation: %+v", a.current)
			}
			reviewFilterPush(t, a, reviewFilterPacket(0x14, 15, true, append([]byte{0}, raw[:2]...), nil), 376)
			reviewFilterPush(t, a, duplicate, 564)
			if a.current == nil || a.current.PacketOffset != 564 || a.current.Value != updated || a.current.UnwrappedTicks != wantTicks {
				t.Fatalf("updated duplicate changed epoch: got=%+v want ticks=%d", a.current, wantTicks)
			}
			latest := *a.current
			reviewFilterPush(t, a, duplicate, 752)
			if *a.current != latest {
				t.Fatal("identical updated duplicate changed latest observation")
			}
			done := reviewFilterPush(t, a, reviewFilterPacket(0x14, 0, false, raw[2:], nil), 940)
			next := reviewFilterPush(t, a, reviewFilterPacket(0x14, 1, true, append([]byte{0}, raw...), nil), 1128)
			if pids != nil && len(pids) == 0 {
				if len(done) != 0 || len(next) != 0 {
					t.Fatal("empty filter emitted sections")
				}
				continue
			}
			if len(done) != 1 || !bytes.Equal(done[0].Raw, raw) || done[0].PacketOffset != 376 || done[0].PCRAtPUSI == nil || done[0].PCRAtPUSI.Value != original || done[0].PCRAtPUSI.PacketOffset != 0 {
				t.Fatalf("updated duplicate destroyed pending/PUSI snapshot: %+v", done)
			}
			if len(next) != 1 || next[0].PCRAtPUSI == nil || *next[0].PCRAtPUSI != latest {
				t.Fatalf("new PUSI missing latest observation: %+v", next)
			}
		}
	})
}
