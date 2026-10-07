package psi

const PCRModulus uint64 = (1 << 33) * 300

type PCRSnapshot struct {
	PID            uint16
	PacketOffset   int64
	Value          PCR
	UnwrappedTicks uint64
}

type PCR struct {
	Base      uint64
	Extension uint16
}

func (p PCR) Ticks() uint64        { return p.Base*300 + uint64(p.Extension) }
func PCRDelta(from, to PCR) uint64 { return (to.Ticks() + PCRModulus - from.Ticks()) % PCRModulus }

func parsePCR(b []byte) (PCR, error) {
	p := PCR{Base: uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 | uint64(b[3])<<1 | uint64(b[4]>>7), Extension: uint16(b[4]&1)<<8 | uint16(b[5])}
	if b[4]&0x7e != 0x7e || p.Extension > 299 {
		return PCR{}, ErrPacket
	}
	return p, nil
}
