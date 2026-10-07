package arib

import (
	"bytes"
	"math/rand"
	"testing"
	"unicode/utf8"
)

func TestDecodeSafeForEveryBytePair(t *testing.T) {
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			data := []byte{byte(a), byte(b)}
			got := Decode(data)
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 for %x: %q", data, got)
			}
			if len(got) > 8*len(data)+3 {
				t.Fatalf("unbounded output for %x", data)
			}
		}
	}
}

func TestDecodeSafeForSyntheticRandomAndTruncatedInputs(t *testing.T) {
	random := rand.New(rand.NewSource(24))
	for iteration := 0; iteration < 5000; iteration++ {
		data := make([]byte, random.Intn(257))
		random.Read(data)
		before := append([]byte(nil), data...)
		for length := 0; length <= len(data); length += 7 {
			got := Decode(data[:length])
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 for %x", data[:length])
			}
			if len(got) > 8*length+3 {
				t.Fatalf("unbounded output for %x", data[:length])
			}
		}
		if !bytes.Equal(data, before) {
			t.Fatal("Decode modified input")
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, data := range [][]byte{
		nil, {0x46, 0x7c, 0x7a, 0x56}, {0x1b, 0x24, 0x28, 0x20, 0x40, 0x21},
		{0x1b, 0x28, 0x0e, 0x41}, {0x9b, 0x31, 0x3b}, {0x95, 0x40, 0x60},
	} {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		before := append([]byte(nil), data...)
		got := Decode(data)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}
		if len(got) > 8*len(data)+3 {
			t.Fatal("unbounded output")
		}
		if got != Decode(data) {
			t.Fatal("nondeterministic output")
		}
		if !bytes.Equal(data, before) {
			t.Fatal("Decode modified input")
		}
	})
}
