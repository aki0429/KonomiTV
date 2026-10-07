package psi

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// Each seed contains a stateful sequence, not one fresh assembler per packet.
func FuzzPacketSequence(f *testing.F) {
	raw := syntaxFixture(0, 1, nil)
	start := exactPayloadFixture(0x123, 0, true, append([]byte{0}, raw[:4]...))
	middle := reviewPCRPayload(0x123, 1, false, raw[4:8], 100)
	duplicate := bytes.Clone(middle)
	duplicate[9]++
	data := append(bytes.Clone(start), middle...)
	data = append(data, duplicate...)
	data = append(data, payloadFixture(0x123, 2, false, raw[8:])...)
	f.Add(data)
	damaged := payloadFixture(0x123, 1, false, raw[4:])
	damaged[1] |= 0x80
	f.Add(append(append(bytes.Clone(start), damaged...), payloadFixture(0x123, 2, false, raw[4:])...))
	bad := bytes.Clone(raw)
	bad[len(bad)-1] ^= 1
	f.Add(append(payloadFixture(0, 0, true, append([]byte{0}, raw...)), payloadFixture(0, 2, true, append(append([]byte{0}, bad...), bad...))...))
	f.Add(append(bytes.Clone(start), []byte{0x47, 1, 0x23}...))
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > 64*PacketSize {
			stream = stream[:64*PacketSize]
		}
		a := NewAssembler(AssemblerOptions{})
		for offset := 0; offset < len(stream); offset += PacketSize {
			end := offset + PacketSize
			if end > len(stream) {
				end = len(stream)
			}
			packet := stream[offset:end]
			_, parseErr := ParsePacket(packet, int64(offset))
			sections, pushErr := a.Push(Packet{Raw: packet, Offset: int64(offset)})
			if parseErr != nil && (!errors.Is(pushErr, parseErr) || len(sections) != 0) {
				t.Fatal("invalid packet bypassed validation")
			}
			for _, s := range sections {
				if ValidateSection(s.Raw) != nil || len(s.Raw) > MaxSectionSize || s.PacketOffset < 0 || s.PacketOffset > int64(offset) {
					t.Fatal("invalid section emitted by stateful assembler")
				}
			}
			for _, state := range a.states {
				if len(state.pending) > MaxSectionSize {
					t.Fatal("unbounded pending")
				}
			}
		}
		stats, err := Scan(context.Background(), bytes.NewReader(stream), ScanOptions{MaxBytes: int64(len(stream) + 1)}, func(s Section) error { return ValidateSection(s.Raw) })
		if err != nil || stats.BytesRead > int64(len(stream)) || stats.Packets > int64(len(stream)/PacketSize) {
			t.Fatalf("bounded sequence scan: %+v err=%v", stats, err)
		}
	})
}

func FuzzPCRDuplicateSequence(f *testing.F) {
	f.Add([]byte{0, 1, 0xe1, 0}, byte(15), uint64(100), uint64(102))
	f.Add([]byte{}, byte(0), uint64((1<<33)-1), uint64(1))
	f.Fuzz(func(t *testing.T, body []byte, cc byte, original, updated uint64) {
		if len(body) > 100 {
			body = body[:100]
		}
		cc &= 15
		original %= 1 << 33
		updated %= 1 << 33
		// Compare against a known assembled Section, not decoder-specific fields.
		raw := syntaxFixture(0x4e, 1, body)
		a := NewAssembler(AssemblerOptions{})
		mustPush(t, a, reviewPCRPayload(0x12, cc, true, append([]byte{0}, raw[:4]...), original), 0)
		middle := reviewPCRPayload(0x12, (cc+1)&15, false, raw[4:8], original)
		mustPush(t, a, middle, 188)
		duplicate := reviewPCRPayload(0x12, (cc+1)&15, false, raw[4:8], updated)
		if got := mustPush(t, a, duplicate, 376); len(got) != 0 {
			t.Fatal("PCR duplicate reemitted payload")
		}
		done := mustPush(t, a, payloadFixture(0x12, (cc+2)&15, false, raw[8:]), 564)
		if len(done) != 1 || !bytes.Equal(done[0].Raw, raw) || done[0].PCRAtPUSI.Value.Base != original || done[0].PacketOffset != 0 {
			t.Fatal("duplicate destroyed pending or original snapshot")
		}
		next := mustPush(t, a, payloadFixture(0x14, 0, true, append([]byte{0}, raw...)), 752)
		expectedOffset := int64(376)
		if original == updated {
			expectedOffset = 188
		} // Byte-identical retransmit does not create a new clock observation.
		if len(next) != 1 || next[0].PCRAtPUSI.Value.Base != updated || next[0].PCRAtPUSI.PacketOffset != expectedOffset {
			t.Fatal("PCR duplicate failed to refresh latest clock")
		}
	})
}

func FuzzPacketRecoverySequence(f *testing.F) {
	f.Add(uint16(0x123), uint16(187))
	f.Add(uint16(0), uint16(2))
	f.Add(uint16(0x1ffe), uint16(189))
	f.Fuzz(func(t *testing.T, pid, damagedLength uint16) {
		pid &= 0x1fff
		otherPID := (pid + 1) & 0x1fff
		raw := syntaxFixture(0, 1, nil)
		a := NewAssembler(AssemblerOptions{})
		mustPush(t, a, exactPayloadFixture(pid, 0, true, append([]byte{0}, raw[:8]...)), 0)
		mustPush(t, a, exactPayloadFixture(otherPID, 0, true, append([]byte{0}, raw[:8]...)), 188)
		size := int(damagedLength % 190)
		bad := append(payloadFixture(pid, 1, false, raw[8:]), 0)
		if size == PacketSize {
			bad[1] |= 0x80
		}
		_, err := a.Push(Packet{PID: otherPID, Raw: bad[:size], Offset: 376})
		if err == nil {
			t.Fatal("damaged packet accepted")
		}
		if got := mustPush(t, a, payloadFixture(pid, 1, false, raw[8:]), 564); len(got) != 0 {
			t.Fatal("damaged PID pending emitted")
		}
		other := mustPush(t, a, payloadFixture(otherPID, 1, false, raw[8:]), 752)
		expected := 0
		if size >= 3 {
			expected = 1
		}
		if len(other) != expected {
			t.Fatal("damaged header reset wrong scope")
		}
		if got := mustPush(t, a, payloadFixture(pid, 2, true, append([]byte{0}, raw...)), 940); len(got) != 1 {
			t.Fatal("new PUSI recovery failed")
		}
	})
}
