package psi

import (
	"bytes"
	"errors"
	"testing"
)

func mustPush(t *testing.T, a *Assembler, b []byte, offset int64) []Section {
	t.Helper()
	p, err := ParsePacket(b, offset)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Push(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAssemblerEmitsOwnedSection(t *testing.T) {
	raw := syntaxFixture(0, 0x123, []byte{0, 1, 0xe1, 0})
	packet := payloadFixture(0, 0, true, append([]byte{0}, raw...))
	a := NewAssembler(AssemblerOptions{})
	got := mustPush(t, a, packet, 188)
	if len(got) != 1 || got[0].PID != 0 || got[0].PacketOffset != 188 || !bytes.Equal(got[0].Raw, raw) {
		t.Fatalf("sections: %+v", got)
	}
	packet[5] = 0xff
	if !bytes.Equal(got[0].Raw, raw) {
		t.Fatal("section aliases packet")
	}
	filtered := NewAssembler(AssemblerOptions{PIDs: []uint16{0x14}})
	if got := mustPush(t, filtered, payloadFixture(0, 0, true, append([]byte{0}, raw...)), 0); len(got) != 0 {
		t.Fatal("PID filter ignored")
	}
	if got := mustPush(t, NewAssembler(AssemblerOptions{PIDs: []uint16{}}), payloadFixture(0, 0, true, append([]byte{0}, raw...)), 0); len(got) != 0 {
		t.Fatal("empty PID filter ignored")
	}
	_ = errors.Is
}

func exactPayloadFixture(pid uint16, cc byte, pusi bool, payload []byte) []byte {
	b := payloadFixture(pid, cc, pusi, nil)
	b[3] = 0x30 | cc
	b[4] = byte(183 - len(payload))
	if b[4] > 0 {
		b[5] = 0
	}
	copy(b[5+int(b[4]):], payload)
	return b
}

func TestAssemblerPointerAndMultipleSections(t *testing.T) {
	a := NewAssembler(AssemblerOptions{})
	first := syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0})
	second := syntaxFixture(0, 2, []byte{0, 2, 0xe1, 1})
	third := syntaxFixture(0, 3, []byte{0, 3, 0xe1, 2})
	if got := mustPush(t, a, exactPayloadFixture(0, 0, true, append([]byte{0}, first[:2]...)), 0); len(got) != 0 {
		t.Fatal("partial emitted")
	}
	payload := append([]byte{byte(len(first) - 2)}, first[2:]...)
	payload = append(payload, second...)
	payload = append(payload, third...)
	got := mustPush(t, a, payloadFixture(0, 1, true, payload), 188)
	if len(got) != 3 {
		t.Fatalf("sections %d", len(got))
	}
	for i, want := range [][]byte{first, second, third} {
		if !bytes.Equal(got[i].Raw, want) {
			t.Fatalf("section %d corrupt", i)
		}
	}
	if got[0].PacketOffset != 0 || got[1].PacketOffset != 188 || got[2].PacketOffset != 188 {
		t.Fatal("pointer changed origin offsets")
	}
	other := NewAssembler(AssemblerOptions{})
	if got := mustPush(t, other, payloadFixture(0, 0, false, second), 0); len(got) != 0 {
		t.Fatal("continuation started section")
	}
	mustPush(t, other, exactPayloadFixture(0, 1, true, append([]byte{0}, second[:7]...)), 188)
	got = mustPush(t, other, payloadFixture(0, 2, false, second[7:]), 376)
	if len(got) != 1 || got[0].PacketOffset != 188 {
		t.Fatal("continuation failed")
	}
}

func TestAssemblerContinuity(t *testing.T) {
	a := NewAssembler(AssemblerOptions{})
	raw := syntaxFixture(0, 1, []byte{0, 1, 0xe1, 0})
	start := exactPayloadFixture(0, 15, true, append([]byte{0}, raw[:8]...))
	mustPush(t, a, start, 0)
	if got := mustPush(t, a, start, 188); len(got) != 0 {
		t.Fatal("duplicate emitted")
	}
	adaptation := pcrFixture(0, 1, 0)
	adaptation[3] |= 15
	mustPush(t, a, adaptation, 376)
	got := mustPush(t, a, payloadFixture(0, 0, false, raw[8:]), 564)
	if len(got) != 1 {
		t.Fatal("adaptation only consumed CC or wrap failed")
	}
	full := payloadFixture(0, 1, true, append([]byte{0}, raw...))
	if got := mustPush(t, a, full, 752); len(got) != 1 {
		t.Fatal("full section missing")
	}
	if got := mustPush(t, a, full, 940); len(got) != 0 {
		t.Fatal("duplicate full section emitted")
	}
	different := payloadFixture(0, 1, true, append([]byte{0}, syntaxFixture(0, 2, nil)...))
	p, _ := ParsePacket(different, 1128)
	got, err := a.Push(p)
	if !errors.Is(err, ErrContinuity) || len(got) != 1 {
		t.Fatalf("same CC nonduplicate: %v %d", err, len(got))
	}
	mustPush(t, a, exactPayloadFixture(0, 2, true, append([]byte{0}, raw[:8]...)), 1316)
	p, _ = ParsePacket(payloadFixture(0, 4, false, raw[8:]), 1504)
	got, err = a.Push(p)
	if !errors.Is(err, ErrContinuity) || len(got) != 0 {
		t.Fatalf("gap continuation: %v %d", err, len(got))
	}
	discontinuous := exactPayloadFixture(0, 9, true, append([]byte{0}, raw...))
	discontinuous[5] = 0x80
	if got := mustPush(t, a, discontinuous, 1692); len(got) != 1 {
		t.Fatal("discontinuity didn't recover")
	}
	a.ResetPID(0)
	if got := mustPush(t, a, payloadFixture(0, 3, true, append([]byte{0}, raw...)), 1880); len(got) != 1 {
		t.Fatal("ResetPID failed")
	}
	a.Reset()
	if got := mustPush(t, a, payloadFixture(0, 7, true, append([]byte{0}, raw...)), 2068); len(got) != 1 {
		t.Fatal("Reset failed")
	}
}

func TestAssemblerPCRAtPUSI(t *testing.T) {
	target := uint16(0x100)
	a := NewAssembler(AssemblerOptions{PIDs: []uint16{0x14}, PCRPID: &target})
	raw := syntaxFixture(0, 1, nil)
	mustPush(t, a, pcrFixture(target, (1<<33)-1, 299), 0)
	mustPush(t, a, exactPayloadFixture(0x14, 0, true, append([]byte{0}, raw[:6]...)), 188)
	mustPush(t, a, pcrFixture(target, 0, 2), 376)
	first := mustPush(t, a, payloadFixture(0x14, 1, false, raw[6:]), 564)
	if len(first) != 1 || first[0].PCRAtPUSI == nil {
		t.Fatal("missing PCR snapshot")
	}
	snap := first[0].PCRAtPUSI
	if snap.PID != target || snap.PacketOffset != 0 || snap.Value.Extension != 299 || snap.UnwrappedTicks != PCRModulus-1 {
		t.Fatalf("snapshot: %+v", snap)
	}
	second := mustPush(t, a, payloadFixture(0x14, 2, true, append([]byte{0}, raw...)), 752)
	if second[0].PCRAtPUSI.UnwrappedTicks != PCRModulus+2 || snap.UnwrappedTicks != PCRModulus-1 {
		t.Fatal("wrap or snapshot ownership failed")
	}
	mustPush(t, a, pcrFixture(0x200, 100, 0), 940)
	third := mustPush(t, a, payloadFixture(0x14, 3, true, append([]byte{0}, raw...)), 1128)
	if third[0].PCRAtPUSI.PID != target {
		t.Fatal("PCRPID filter ignored")
	}
	disc := pcrFixture(target, 5, 0)
	disc[5] |= 0x80
	mustPush(t, a, disc, 1316)
	fourth := mustPush(t, a, payloadFixture(0x14, 4, true, append([]byte{0}, raw...)), 1504)
	if fourth[0].PCRAtPUSI.UnwrappedTicks != 1500 {
		t.Fatal("discontinuity epoch not reset")
	}
	a.ResetPID(target)
	fifth := mustPush(t, a, payloadFixture(0x14, 5, true, append([]byte{0}, raw...)), 1692)
	if fifth[0].PCRAtPUSI != nil {
		t.Fatal("reset retained PCR")
	}
	unfiltered := NewAssembler(AssemblerOptions{})
	mustPush(t, unfiltered, pcrFixture(0x200, 7, 5), 0)
	got := mustPush(t, unfiltered, payloadFixture(0, 0, true, append([]byte{0}, raw...)), 188)
	if got[0].PCRAtPUSI == nil || got[0].PCRAtPUSI.Value.Ticks() != 2105 {
		t.Fatal("latest PCR missing")
	}
}

func TestAssemblerMalformedRecoveryAndMaximum(t *testing.T) {
	raw := syntaxFixture(0, 1, bytes.Repeat([]byte{0}, 4084))
	if len(raw) != MaxSectionSize {
		t.Fatalf("fixture length %d", len(raw))
	}
	a := NewAssembler(AssemblerOptions{})
	var got []Section
	offset := int64(0)
	cc := byte(0)
	for i := 0; i < len(raw); {
		cap := 184
		payload := []byte{}
		if i == 0 {
			cap = 183
			payload = append(payload, 0)
		}
		n := cap
		if n > len(raw)-i {
			n = len(raw) - i
		}
		payload = append(payload, raw[i:i+n]...)
		got = append(got, mustPush(t, a, payloadFixture(0, cc, i == 0, payload), offset)...)
		i += n
		cc = (cc + 1) & 15
		offset += 188
	}
	if len(got) != 1 || len(got[0].Raw) != 4096 {
		t.Fatal("maximum length section lost")
	}
	bad := syntaxFixture(0, 1, nil)
	bad[3] ^= 1
	good := syntaxFixture(0, 2, nil)
	p, _ := ParsePacket(payloadFixture(0, cc, true, append(append([]byte{0}, bad...), good...)), offset)
	got, err := a.Push(p)
	if !errors.Is(err, ErrCRC) || len(got) != 1 || !bytes.Equal(got[0].Raw, good) {
		t.Fatalf("CRC recovery %v %d", err, len(got))
	}
	cc = (cc + 1) & 15
	p, _ = ParsePacket(payloadFixture(0, cc, true, []byte{0, 0, 0xbf, 0xfe}), offset+188)
	if _, err := a.Push(p); !errors.Is(err, ErrSection) {
		t.Fatalf("overlong header: %v", err)
	}
	cc = (cc + 1) & 15
	p, _ = ParsePacket(payloadFixture(0, cc, true, []byte{184}), offset+376)
	if _, err := a.Push(p); !errors.Is(err, ErrSection) {
		t.Fatalf("pointer overflow: %v", err)
	}
	cc = (cc + 1) & 15
	if got := mustPush(t, a, payloadFixture(0, cc, true, append([]byte{0}, good...)), offset+564); len(got) != 1 {
		t.Fatal("recovery failed")
	}
	a.Reset()
	mustPush(t, a, exactPayloadFixture(0, 0, true, append([]byte{0}, good[:6]...)), 0)
	p, _ = ParsePacket(payloadFixture(0, 1, true, append([]byte{0}, good...)), 188)
	got, err = a.Push(p)
	if !errors.Is(err, ErrSection) || len(got) != 1 {
		t.Fatalf("unfinished pointer discarded current %v %d", err, len(got))
	}
}

// TDD: assembler_test.go additions.
