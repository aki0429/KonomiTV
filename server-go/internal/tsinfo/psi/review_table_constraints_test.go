package psi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestReviewMalformedPATDuplicateProgramRejected(t *testing.T) {
	for _, body := range [][]byte{
		{0, 1, 0xe1, 0, 0, 1, 0xe1, 1},
		{0, 1, 0xe1, 0, 0, 1, 0xe1, 0},
		{0, 0, 0xe0, 0x10, 0, 0, 0xe0, 0x11},
	} {
		raw := syntaxFixture(0, 1, body)
		got, err := DecodePAT(Section{Raw: raw})
		if !errors.Is(err, ErrSection) {
			t.Errorf("duplicate program_number accepted: table=%+v err=%v", got, err)
		}
	}
}

func reviewPATBody(programs int) []byte {
	body := make([]byte, 4*programs)
	for i := 0; i < programs; i++ {
		sid := uint16(i + 1)
		body[4*i], body[4*i+1], body[4*i+2], body[4*i+3] = byte(sid>>8), byte(sid), 0xe1, 0
	}
	return body
}

func TestReviewMalformedPATSpecificLengthRejected(t *testing.T) {
	for _, programs := range []int{253, 254} {
		raw := syntaxFixture(0, 1, reviewPATBody(programs))
		if err := ValidateSection(raw); err != nil {
			t.Fatal("generic section bound must remain 4096:", err)
		}
		got, err := DecodePAT(Section{Raw: raw})
		if programs == 253 {
			if err != nil || len(got.Programs) != 253 || len(raw)-3 != 1021 {
				t.Fatalf("valid PAT boundary: table=%+v err=%v", got, err)
			}
		} else if !errors.Is(err, ErrSection) {
			t.Fatalf("expected ErrSection for PAT section_length>1021: total_bytes=%d err=%v", len(raw), err)
		}
	}
}

func TestReviewMalformedPMTSectionNumberRejected(t *testing.T) {
	for _, numbers := range [][2]byte{{0, 0}, {0, 1}, {1, 1}} {
		raw := syntaxFixture(2, 1, []byte{0xe1, 0, 0xf0, 0})
		raw[6], raw[7] = numbers[0], numbers[1]
		binary.BigEndian.PutUint32(raw[len(raw)-4:], fixtureCRC(raw[:len(raw)-4]))
		got, err := DecodePMT(Section{Raw: raw})
		if numbers == [2]byte{0, 0} {
			if err != nil || got.Header.SectionNumber != 0 || got.Header.LastSectionNumber != 0 {
				t.Fatalf("valid single-section PMT: %+v err=%v", got, err)
			}
		} else if !errors.Is(err, ErrSection) {
			t.Errorf("expected ErrSection for PMT section numbers %v: table=%+v err=%v", numbers, got, err)
		}
	}
}

func reviewPMTBody(descriptorBytes int) []byte {
	// Unknown descriptors keep the loop structurally valid at exact boundaries.
	desc := []byte{}
	for remaining := descriptorBytes; remaining > 0; {
		n := remaining
		if n > 257 {
			n = 257
		}
		desc = append(desc, 0xee, byte(n-2))
		desc = append(desc, make([]byte, n-2)...)
		remaining -= n
	}
	return append([]byte{0xe1, 0, 0xf0 | byte(descriptorBytes>>8), byte(descriptorBytes)}, desc...)
}

func TestReviewMalformedPMTSpecificLengthRejected(t *testing.T) {
	for _, length := range []int{1021, 1022} {
		raw := syntaxFixture(2, 1, reviewPMTBody(length-13))
		if len(raw)-3 != length || ValidateSection(raw) != nil {
			t.Fatal("invalid boundary fixture")
		}
		got, err := DecodePMT(Section{Raw: raw})
		if length == 1021 {
			if err != nil || got.PCRPID != 0x100 {
				t.Fatalf("valid max PMT rejected: err=%v", err)
			}
		} else if !errors.Is(err, ErrSection) {
			t.Fatalf("PMT section_length=%d accepted: err=%v", length, err)
		}
	}
}

func TestReviewLargeEITStillAccepts4096ByteSection(t *testing.T) {
	// 4066 descriptor bytes + event/header/CRC reach the generic 4096 ceiling.
	desc := reviewPMTBody(4066)[4:]
	body := []byte{0x12, 0x34, 0x78, 0x90, 0, 0x4e}
	body = append(body, eventFixture(1, bytes.Repeat([]byte{0xff}, 5), bytes.Repeat([]byte{0xff}, 3), desc, 0)...)
	raw := syntaxFixture(0x4e, 1, body)
	if len(raw) != MaxSectionSize {
		t.Fatalf("fixture length %d", len(raw))
	}
	got, err := DecodeEIT(Section{Raw: raw})
	if err != nil || len(got.Events) != 1 {
		t.Fatalf("PAT/PMT constraints narrowed EIT: err=%v", err)
	}
	a := NewAssembler(AssemblerOptions{})
	var sections []Section
	for i, cc := 0, byte(0); i < len(raw); cc = (cc + 1) & 15 {
		cap := 184
		payload := []byte{}
		if i == 0 {
			payload = append(payload, 0)
			cap--
		}
		n := cap
		if n > len(raw)-i {
			n = len(raw) - i
		}
		payload = append(payload, raw[i:i+n]...)
		sections = append(sections, mustPush(t, a, payloadFixture(0x12, cc, i == 0, payload), int64(i))...)
		i += n
	}
	if len(sections) != 1 || !bytes.Equal(sections[0].Raw, raw) {
		t.Fatal("large EIT assembly failed")
	}
}

func TestReviewPIDAndAssemblyMemoryBound(t *testing.T) {
	a := NewAssembler(AssemblerOptions{})
	raw := syntaxFixture(0x4e, 1, bytes.Repeat([]byte{0}, 4084))
	for pid := uint16(0); pid < 8192; pid++ {
		p, err := ParsePacket(payloadFixture(pid, 0, true, append([]byte{0}, raw[:183]...)), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.Push(p); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.states) != 8192 {
		t.Fatal(len(a.states))
	}
	for _, state := range a.states {
		if len(state.pending) > MaxSectionSize {
			t.Fatal("unbounded pending")
		}
	}
}
