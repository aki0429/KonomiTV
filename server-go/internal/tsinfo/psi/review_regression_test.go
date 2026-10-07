package psi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// Synthetic regression migrated from the independent scratch review.
func reviewPCRPayload(pid uint16, cc byte, pusi bool, payload []byte, base uint64) []byte {
	b := exactPayloadFixture(pid, cc, pusi, payload)
	b[5] = 0x10
	b[6], b[7], b[8], b[9] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
	b[10], b[11] = byte(base&1)<<7|0x7e, 0
	return b
}

func TestReviewDuplicateWithUpdatedPCRDoesNotDiscardPending(t *testing.T) {
	raw := syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0})
	start := exactPayloadFixture(0, 0, true, append([]byte{0}, raw[:4]...))
	middle := reviewPCRPayload(0, 1, false, raw[4:8], 100)
	duplicate := bytes.Clone(middle)
	duplicate[9]++ // PCR base 100 -> 102; all non-PCR bytes are identical.
	a := NewAssembler(AssemblerOptions{})
	mustPush(t, a, start, 0)
	mustPush(t, a, middle, 188)
	p, err := ParsePacket(duplicate, 376)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Push(p)
	if err != nil || len(got) != 0 {
		t.Errorf("updated-PCR duplicate: err=%v sections=%d", err, len(got))
	}
	got = mustPush(t, a, payloadFixture(0, 2, false, raw[8:]), 564)
	if len(got) != 1 || !bytes.Equal(got[0].Raw, raw) || got[0].PacketOffset != 0 {
		t.Errorf("continuation: expected owned original section, sections=%+v", got)
	}
}

func TestReviewUpdatedPCRDuplicateRefreshesClockWithoutReemitting(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	a := NewAssembler(AssemblerOptions{})
	packet := reviewPCRPayload(0, 0, true, append([]byte{0}, raw...), 100)
	first := mustPush(t, a, packet, 0)
	updated := bytes.Clone(packet)
	updated[9]++
	if got := mustPush(t, a, updated, 188); len(got) != 0 {
		t.Fatalf("duplicate emitted %d sections", len(got))
	}
	next := mustPush(t, a, payloadFixture(0, 1, true, append([]byte{0}, raw...)), 376)
	if len(next) != 1 || next[0].PCRAtPUSI == nil {
		t.Fatal("missing latest PCR snapshot")
	}
	snap := next[0].PCRAtPUSI
	if snap.Value.Base != 102 || snap.PacketOffset != 188 || snap.UnwrappedTicks != 102*300 {
		t.Fatalf("updated duplicate not used as latest clock: %+v", snap)
	}
	if first[0].PCRAtPUSI.Value.Base != 100 || first[0].PCRAtPUSI.PacketOffset != 0 {
		t.Fatal("duplicate modified previously emitted snapshot")
	}
}

func TestReviewPCRDuplicateRequiresAllNonPCRBytesIdentical(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"payload", func(b []byte) { b[len(b)-1] ^= 1 }},
		{"adaptation stuffing", func(b []byte) { b[12] ^= 1 }},
		{"OPCR", func(b []byte) { b[5] |= 8 }},
		{"PCR flag removed", func(b []byte) { b[5] &^= 0x10 }},
		{"PUSI", func(b []byte) { b[1] ^= 0x40 }},
		{"adaptation length", func(b []byte) { b[4]-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAssembler(AssemblerOptions{})
			packet := reviewPCRPayload(0, 0, true, append([]byte{0}, raw...), 100)
			mustPush(t, a, packet, 0)
			conflicting := bytes.Clone(packet)
			conflicting[9]++
			tc.mutate(conflicting)
			p, parseErr := ParsePacket(conflicting, 188)
			if parseErr != nil {
				t.Fatalf("fixture not structurally valid: %v", parseErr)
			}
			_, err := a.Push(p)
			if !errors.Is(err, ErrContinuity) {
				t.Fatalf("non-PCR difference accepted: %v", err)
			}
		})
	}
}

func TestReviewScanCountsEveryInvalidSection(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	raw[len(raw)-1] ^= 1
	packet := payloadFixture(0, 0, true, append(append([]byte{0}, raw...), raw...))
	stats, err := Scan(context.Background(), bytes.NewReader(packet), ScanOptions{MaxBytes: 188}, func(Section) error { return nil })
	if err != nil || stats.InvalidSections != 2 || stats.ContinuityErrors != 0 || stats.Sections != 0 {
		t.Fatalf("expected two invalid sections: %+v err=%v", stats, err)
	}
}

func TestReviewScanPreservesContinuityAndCRCErrorCounts(t *testing.T) {
	good := syntaxFixture(0, 1, nil)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	data := payloadFixture(0, 0, true, append([]byte{0}, good...))
	data = append(data, payloadFixture(0, 2, true, append([]byte{0}, bad...))...)
	stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: int64(len(data))}, func(Section) error { return nil })
	if err != nil || stats.ContinuityErrors != 1 || stats.InvalidSections != 1 || stats.Sections != 1 {
		t.Fatalf("expected continuity=1 invalid=1: %+v err=%v", stats, err)
	}
}

func TestReviewAssemblerPreservesEveryRecoverableProblem(t *testing.T) {
	good := syntaxFixture(0, 1, nil)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	for _, tc := range []struct {
		name         string
		payload      []byte
		sectionError error
	}{
		{"CRC", append([]byte{0}, bad...), ErrCRC},
		{"invalid section header", []byte{0, 0, 0xbf, 0xfe}, ErrSection},
		{"pointer overflow", []byte{184}, ErrSection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAssembler(AssemblerOptions{})
			mustPush(t, a, payloadFixture(0, 0, true, append([]byte{0}, good...)), 0)
			p, _ := ParsePacket(payloadFixture(0, 2, true, tc.payload), 188)
			got, err := a.Push(p)
			if !errors.Is(err, ErrContinuity) || !errors.Is(err, tc.sectionError) || len(got) != 0 {
				t.Fatalf("continuity overwritten: sections=%d err=%v", len(got), err)
			}
		})
	}
}

func TestReviewScanCountsProblemsAcrossPointerAndNewSections(t *testing.T) {
	good := syntaxFixture(0, 1, nil)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	for _, tc := range []struct {
		name    string
		pointer []byte
	}{
		{"prior CRC", append([]byte{byte(len(bad) - 8)}, bad[8:]...)},
		{"prior incomplete", []byte{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := exactPayloadFixture(0, 0, true, append([]byte{0}, bad[:8]...))
			payload := append(bytes.Clone(tc.pointer), bad...)
			payload = append(payload, good...)
			data = append(data, payloadFixture(0, 1, true, payload)...)
			stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: int64(len(data))}, func(Section) error { return nil })
			if err != nil || stats.InvalidSections != 2 || stats.Sections != 1 || stats.ContinuityErrors != 0 {
				t.Fatalf("expected two invalid and next valid section: %+v err=%v", stats, err)
			}
		})
	}
}

func TestReviewScanRetainsCRCBeforeOverlongNextHeader(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	raw[len(raw)-1] ^= 1
	payload := append([]byte{0}, raw...)
	payload = append(payload, 0, 0xbf, 0xfe)
	stats, err := Scan(context.Background(), bytes.NewReader(payloadFixture(0, 0, true, payload)), ScanOptions{MaxBytes: 188}, func(Section) error { return nil })
	if err != nil || stats.InvalidSections != 2 {
		t.Fatalf("invalid section before bad header lost: %+v err=%v", stats, err)
	}
}

func TestReviewScanConflictingSameCCAndTwoCRCsCountsEveryCause(t *testing.T) {
	good := syntaxFixture(0, 1, nil)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	data := payloadFixture(0, 0, true, append([]byte{0}, good...))
	data = append(data, payloadFixture(0, 0, true, append(append([]byte{0}, bad...), bad...))...)
	stats, err := Scan(context.Background(), bytes.NewReader(data), ScanOptions{MaxBytes: int64(len(data))}, func(Section) error { return nil })
	if err != nil || stats.ContinuityErrors != 1 || stats.InvalidSections != 2 || stats.Sections != 1 {
		t.Fatalf("same CC plus two CRCs: %+v err=%v", stats, err)
	}
}

func TestReviewAssemblyProblemCountsTreeCountsLeavesOnly(t *testing.T) {
	for _, tc := range []struct {
		err                 error
		invalid, continuity int64
	}{
		{nil, 0, 0},
		{ErrCRC, 1, 0},
		{ErrContinuity, 0, 1},
		{errors.Join(ErrCRC, errors.Join(ErrSection, ErrCRC), ErrContinuity), 3, 1},
		{fmt.Errorf("wrapped: %w", errors.Join(ErrCRC, ErrContinuity)), 1, 1},
		{ErrPacket, 0, 0},
	} {
		invalid, continuity := assemblyProblemCounts(tc.err)
		if invalid != tc.invalid || continuity != tc.continuity {
			t.Fatalf("count tree err=%v invalid=%d continuity=%d", tc.err, invalid, continuity)
		}
	}
}

func TestReviewPCRDuplicateRequiresValidAdaptationFields(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	for _, mutate := range []func([]byte){
		func(b []byte) { b[4] = 1 },
		func(b []byte) { b[10] &^= 0x7e },
		func(b []byte) { b[10], b[11] = 0x7f, 44 }, // extension 300
	} {
		a := NewAssembler(AssemblerOptions{})
		packet := reviewPCRPayload(0, 0, true, append([]byte{0}, raw[:8]...), 100)
		mustPush(t, a, packet, 0)
		bad := bytes.Clone(packet)
		mutate(bad)
		_, err := a.Push(Packet{Raw: bad, Offset: 188})
		if !errors.Is(err, ErrPacket) {
			t.Fatalf("invalid PCR retransmit: %v", err)
		}
		if got := mustPush(t, a, payloadFixture(0, 1, false, raw[8:]), 376); len(got) != 0 {
			t.Fatal("malformed PCR duplicate retained pending")
		}
	}
}
