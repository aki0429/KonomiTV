package psi

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestReviewMalformedPacketReturnsReadablePID(t *testing.T) {
	packet := payloadFixture(0x123, 0, false, nil)
	for _, size := range []int{3, 4, 187, 189} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := append(bytes.Clone(packet), 0)
			p, err := ParsePacket(raw[:size], 188)
			if !errors.Is(err, ErrPacket) || p.PID != 0x123 || p.Offset != 188 || !bytes.Equal(p.Raw, raw[:size]) {
				t.Fatalf("expected readable PID/Offset retained: PID=0x%x offset=%d err=%v", p.PID, p.Offset, err)
			}
		})
	}
}

func TestReviewInvalidShortPacketMustDiscardThatPIDPending(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	a := NewAssembler(AssemblerOptions{})
	mustPush(t, a, exactPayloadFixture(0x123, 0, true, append([]byte{0}, raw[:8]...)), 0)
	mustPush(t, a, exactPayloadFixture(0x124, 0, true, append([]byte{0}, raw[:8]...)), 188)
	bad := payloadFixture(0x123, 1, false, raw[8:])[:187]
	_, err := a.Push(Packet{PID: 0x124, Raw: bad, Offset: 376}) // Raw, not caller's PID, is authoritative.
	if !errors.Is(err, ErrPacket) {
		t.Fatal(err)
	}
	p, _ := ParsePacket(payloadFixture(0x123, 1, false, raw[8:]), 564)
	got, err := a.Push(p)
	if err != nil || len(got) != 0 {
		t.Fatalf("damaged PID pending survived: sections=%d err=%v", len(got), err)
	}
	other := mustPush(t, a, payloadFixture(0x124, 1, false, raw[8:]), 752)
	if len(other) != 1 {
		t.Fatal("readable damaged PID discarded another PID")
	}
	next := mustPush(t, a, payloadFixture(0x123, 2, true, append([]byte{0}, raw...)), 940)
	if len(next) != 1 {
		t.Fatal("failed to recover at new PUSI")
	}
}

func TestReviewUnidentifiablePacketSafelyResetsAllState(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	damagedSync := payloadFixture(0x123, 1, false, raw[8:])
	damagedSync[0] = 0
	for _, bad := range [][]byte{nil, {0x47}, {0x47, 1}, damagedSync} {
		a := NewAssembler(AssemblerOptions{})
		mustPush(t, a, pcrFixture(0x100, 100, 0), 0)
		mustPush(t, a, exactPayloadFixture(0x123, 0, true, append([]byte{0}, raw[:8]...)), 188)
		_, err := a.Push(Packet{PID: 0x123, Raw: bad, Offset: 376})
		if !errors.Is(err, ErrPacket) {
			t.Fatal(err)
		}
		if len(a.states) != 0 || len(a.clocks) != 0 || a.current != nil {
			t.Errorf("unreadable header retained state: input=%x states=%d clocks=%d", bad, len(a.states), len(a.clocks))
		}
		got := mustPush(t, a, payloadFixture(0x123, 1, false, raw[8:]), 564)
		if len(got) != 0 {
			t.Fatal("emitted continuation after unidentifiable packet")
		}
	}
}

func TestReviewEmittedSectionsHaveIndependentPCRSnapshots(t *testing.T) {
	raw := syntaxFixture(0, 1, nil)
	a := NewAssembler(AssemblerOptions{})
	mustPush(t, a, pcrFixture(0x100, 100, 0), 0)
	sections := mustPush(t, a, payloadFixture(0, 0, true, append(append([]byte{0}, raw...), raw...)), 188)
	if len(sections) != 2 {
		t.Fatal(len(sections))
	}
	sections[0].PCRAtPUSI.Value.Base = 999
	if sections[1].PCRAtPUSI.Value.Base != 100 {
		t.Fatal("PCR snapshots alias")
	}
}

func TestReviewCRCErrorDoesNotLoseNextValidSection(t *testing.T) {
	good := syntaxFixture(0, 1, nil)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	a := NewAssembler(AssemblerOptions{})
	p, _ := ParsePacket(payloadFixture(0, 0, true, append(append([]byte{0}, bad...), good...)), 0)
	got, err := a.Push(p)
	if !errors.Is(err, ErrCRC) || len(got) != 1 {
		t.Fatalf("got=%d err=%v", len(got), err)
	}
}
