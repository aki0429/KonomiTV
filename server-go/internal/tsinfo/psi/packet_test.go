package psi

import (
	"bytes"
	"testing"
)

func payloadFixture(pid uint16, cc byte, pusi bool, payload []byte) []byte {
	b := bytes.Repeat([]byte{0xff}, 188)
	b[0], b[1], b[2], b[3] = 0x47, byte(pid>>8), byte(pid), 0x10|cc
	if pusi {
		b[1] |= 0x40
	}
	copy(b[4:], payload)
	return b
}

func TestParsePayloadPacket(t *testing.T) {
	raw := payloadFixture(0x123, 15, true, []byte{0, 1, 2})
	p, err := ParsePacket(raw, 376)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 0x123 || p.Offset != 376 || !p.PUSI || !p.HasPayload || p.ContinuityCounter != 15 || !bytes.Equal(p.Payload, raw[4:]) || !bytes.Equal(p.Raw, raw) {
		t.Fatalf("packet: %+v", p)
	}
}

func TestPacketValidation(t *testing.T) {
	valid := payloadFixture(1, 0, false, nil)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short", func(b []byte) []byte { return b[:187] }},
		{"sync", func(b []byte) []byte { b[0] = 0; return b }},
		{"TEI", func(b []byte) []byte { b[1] |= 0x80; return b }},
		{"scrambled", func(b []byte) []byte { b[3] |= 0x80; return b }},
		{"reserved control", func(b []byte) []byte { b[3] = 0; return b }},
		{"adaptation overflow", func(b []byte) []byte { b[3] = 0x30; b[4] = 184; return b }},
		{"adaptation only short", func(b []byte) []byte { b[3] = 0x20; b[4] = 1; return b }},
		{"payload missing", func(b []byte) []byte { b[3] = 0x30; b[4] = 183; return b }},
		{"PCR truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5] = 1, 0x10; return b }},
		{"OPCR truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5] = 1, 0x08; return b }},
		{"splice truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5] = 1, 0x04; return b }},
		{"private truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5], b[6] = 2, 0x02, 1; return b }},
		{"extension truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5], b[6] = 2, 0x01, 1; return b }},
		{"LTW truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5], b[6], b[7] = 3, 1, 1, 0x80; return b }},
		{"piecewise truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5], b[6], b[7] = 3, 1, 1, 0x40; return b }},
		{"seamless truncated", func(b []byte) []byte { b[3] = 0x30; b[4], b[5], b[6], b[7] = 3, 1, 1, 0x20; return b }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.mutate(append([]byte(nil), valid...))
			if _, err := ParsePacket(b, 0); err == nil {
				t.Fatal("accepted invalid packet")
			}
		})
	}
	b := append([]byte(nil), valid...)
	b[3], b[4], b[5] = 0x30, 1, 0x80
	p, err := ParsePacket(b, 1)
	if err != nil || !p.Discontinuity || !bytes.Equal(p.Payload, b[6:]) {
		t.Fatalf("valid adaptation: %+v %v", p, err)
	}
	b[4] = 0
	p, err = ParsePacket(b, 0)
	if err != nil || !bytes.Equal(p.Payload, b[5:]) {
		t.Fatalf("zero adaptation: %v", err)
	}
	b[3], b[4], b[5] = 0x20, 183, 0
	p, err = ParsePacket(b, 0)
	if err != nil || p.HasPayload || len(p.Payload) != 0 {
		t.Fatalf("adaptation only: %+v %v", p, err)
	}
}

func pcrFixture(pid uint16, base uint64, ext uint16) []byte {
	b := payloadFixture(pid, 0, false, nil)
	b[3], b[4], b[5] = 0x20, 183, 0x10
	b[6], b[7], b[8], b[9] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
	b[10], b[11] = byte(base&1)<<7|0x7e|byte(ext>>8), byte(ext)
	return b
}

func TestPCRBaseExtensionWrap(t *testing.T) {
	b := pcrFixture(0x100, (1<<33)-1, 299)
	p, err := ParsePacket(b, 188)
	if err != nil || p.PCR == nil {
		t.Fatalf("PCR missing: %+v %v", p, err)
	}
	if p.PCR.Base != (1<<33)-1 || p.PCR.Extension != 299 || p.PCR.Ticks() != PCRModulus-1 {
		t.Fatalf("PCR: %+v", p.PCR)
	}
	if got := PCRDelta(*p.PCR, PCR{Base: 0, Extension: 2}); got != 3 {
		t.Fatalf("wrap delta %d", got)
	}
	b[10] &^= 0x7e
	if _, err := ParsePacket(b, 0); err == nil {
		t.Fatal("PCR reserved bits accepted")
	}
	b = pcrFixture(0x100, 0, 300)
	if _, err := ParsePacket(b, 0); err == nil {
		t.Fatal("PCR extension 300 accepted")
	}
}

// TDD: packet_test.go additions.
