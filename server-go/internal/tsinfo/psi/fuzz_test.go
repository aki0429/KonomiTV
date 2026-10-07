package psi

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"
)

func FuzzPacketAndAssembly(f *testing.F) {
	raw := syntaxFixture(0, 1, nil)
	f.Add(payloadFixture(0, 0, true, append([]byte{0}, raw...)))
	damaged := payloadFixture(0, 0, true, append([]byte{0}, raw...))
	damaged[1] |= 0x80
	f.Add(damaged)
	f.Add([]byte{})
	f.Add([]byte{0x47, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, parseErr := ParsePacket(b, 188)
		a := NewAssembler(AssemblerOptions{})
		// Public Push must not permit a hand-constructed Packet to bypass validation.
		got, pushErr := a.Push(Packet{PID: p.PID, Offset: 188, PUSI: true, HasPayload: true, Payload: b, Raw: b})
		if parseErr != nil {
			if pushErr == nil || len(got) != 0 {
				t.Fatal("invalid raw packet accepted by assembler")
			}
			return
		}
		for _, s := range got {
			if len(s.Raw) > MaxSectionSize || s.PacketOffset != 188 || ValidateSection(s.Raw) != nil {
				t.Fatal("invalid emitted section")
			}
		}
	})
}

func FuzzSectionAndDescriptors(f *testing.F) {
	f.Add(syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0}))
	f.Add(totFixture([]byte{0xc9, 0x58, 0, 0, 0}, nil))
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { exerciseMalformed(t, b) })
}

func exerciseMalformed(t *testing.T, b []byte) {
	t.Helper()
	_, _ = DecodeDescriptors(b)
	_, _ = DecodeJSTTime(b)
	_, _ = DecodeDuration(b)
	s := Section{Raw: b}
	_, _ = DecodeSection(s)
	_, _ = DecodePAT(s)
	_, _ = DecodePMT(s)
	_, _ = DecodeSDT(s)
	_, _ = DecodeNIT(s)
	_, _ = DecodeEIT(s)
	_, _ = DecodeTOT(s)
	if p, err := ParsePacket(b, 0); err == nil {
		sections, _ := NewAssembler(AssemblerOptions{}).Push(p)
		for _, s := range sections {
			if err := ValidateSection(s.Raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	stats, err := Scan(context.Background(), bytes.NewReader(b), ScanOptions{MaxBytes: int64(len(b) + 1)}, func(s Section) error {
		return ValidateSection(s.Raw)
	})
	if err != nil && !errors.Is(err, ErrOptions) {
		t.Fatal(err)
	}
	if stats.BytesRead > int64(len(b)) {
		t.Fatal("reader exceeded actual data")
	}
}

func TestMalformedDeterministicFuzzCorpus(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for range 512 {
		b := make([]byte, rng.Intn(1024))
		_, _ = rng.Read(b)
		exerciseMalformed(t, b)
	}
	for _, base := range [][]byte{syntaxFixture(0, 1, nil), syntaxFixture(2, 1, []byte{0xe1, 0, 0xf0, 0}), syntaxFixture(0x42, 1, []byte{0, 1, 0xff}), syntaxFixture(0x40, 1, []byte{0xf0, 0, 0xf0, 0}), syntaxFixture(0x4e, 1, []byte{0, 1, 0, 1, 0, 0x4e}), totFixture([]byte{0xc9, 0x58, 0, 0, 0}, nil)} {
		for n := 0; n < len(base); n++ {
			exerciseMalformed(t, base[:n])
		}
		for i := range base {
			b := bytes.Clone(base)
			b[i] ^= 0xff
			if b[1]&0x80 != 0 || b[0] == 0x73 {
				crc := fixtureCRC(b[:len(b)-4])
				b[len(b)-4], b[len(b)-3], b[len(b)-2], b[len(b)-1] = byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)
			}
			exerciseMalformed(t, b)
		}
	}
}
