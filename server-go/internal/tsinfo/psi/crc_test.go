package psi

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestCRC32MPEG2KnownVector(t *testing.T) {
	if got := CRC32MPEG2([]byte("123456789")); got != 0x0376e6e7 {
		t.Fatalf("CRC = %08x, want 0376e6e7", got)
	}
}

// Independently generated bit-serial fixture CRC, never a broadcast capture.
func fixtureCRC(b []byte) uint32 {
	var v uint32 = 0xffffffff
	for _, x := range b {
		for bit := 7; bit >= 0; bit-- {
			top := (v >> 31) ^ uint32((x>>bit)&1)
			v <<= 1
			if top != 0 {
				v ^= 0x04c11db7
			}
		}
	}
	return v
}

func syntaxFixture(id byte, ext uint16, body []byte) []byte {
	b := []byte{id, 0xb0, 0, byte(ext >> 8), byte(ext), 0xc1, 0, 0}
	b = append(b, body...)
	n := len(b) + 4 - 3
	b[1] |= byte(n >> 8)
	b[2] = byte(n)
	return binary.BigEndian.AppendUint32(b, fixtureCRC(b))
}

func TestValidateSectionRejectsCorruptionAndOversize(t *testing.T) {
	good := syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0})
	if err := ValidateSection(good); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), good...)
	bad[8] ^= 1
	if err := ValidateSection(bad); !errors.Is(err, ErrCRC) {
		t.Fatalf("bad CRC: %v", err)
	}
	if err := ValidateSection(good[:len(good)-1]); !errors.Is(err, ErrSection) {
		t.Fatalf("truncated: %v", err)
	}
	large := make([]byte, 4097)
	large[0], large[1], large[2] = 0, 0xbf, 0xfe
	if err := ValidateSection(large); !errors.Is(err, ErrSection) {
		t.Fatalf("oversize: %v", err)
	}
}

// TDD: CRC test additions.
