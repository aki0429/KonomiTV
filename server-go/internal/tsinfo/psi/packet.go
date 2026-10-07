package psi

const PacketSize = 188

type Packet struct {
	PID               uint16
	Offset            int64
	PUSI              bool
	ContinuityCounter uint8
	HasPayload        bool
	Discontinuity     bool
	PCR               *PCR
	Payload           []byte
	Raw               []byte
}

// Without sync and both PID bytes, even a caller-supplied PID cannot be trusted.
func packetPIDReadable(raw []byte) bool {
	return len(raw) >= 3 && raw[0] == 0x47
}

func ParsePacket(raw []byte, offset int64) (Packet, error) {
	p := Packet{Offset: offset, Raw: raw}
	if !packetPIDReadable(raw) {
		return p, ErrPacket
	}
	p.PID = uint16(raw[1]&0x1f)<<8 | uint16(raw[2])
	if len(raw) != PacketSize {
		return p, ErrPacket
	}
	p.PUSI = raw[1]&0x40 != 0
	p.ContinuityCounter = raw[3] & 0x0f
	p.HasPayload = raw[3]&0x10 != 0
	if raw[1]&0x80 != 0 {
		return p, ErrTransport
	}
	if raw[3]&0xc0 != 0 {
		return p, ErrScrambled
	}
	control := (raw[3] >> 4) & 3
	if control == 0 {
		return p, ErrPacket
	}
	start := 4
	if control&2 != 0 {
		n := int(raw[4])
		if n > 183 || (control == 2 && n != 183) || (control == 3 && n > 182) {
			return p, ErrPacket
		}
		start = 5 + n
		if n > 0 {
			a := raw[5:start]
			flags := a[0]
			p.Discontinuity = flags&0x80 != 0
			i := 1
			if flags&0x10 != 0 {
				if i+6 > len(a) {
					return p, ErrPacket
				}
				value, err := parsePCR(a[i : i+6])
				if err != nil {
					return p, err
				}
				p.PCR = &value
				i += 6
			}
			if flags&0x08 != 0 {
				i += 6
			}
			if flags&0x04 != 0 {
				i++
			}
			if i > len(a) {
				return p, ErrPacket
			}
			if flags&0x02 != 0 {
				if i >= len(a) {
					return p, ErrPacket
				}
				i += 1 + int(a[i])
				if i > len(a) {
					return p, ErrPacket
				}
			}
			if flags&0x01 != 0 {
				if i >= len(a) {
					return p, ErrPacket
				}
				end := i + 1 + int(a[i])
				if end > len(a) {
					return p, ErrPacket
				}
				i++
				if i < end {
					ef := a[i]
					i++
					if ef&0x80 != 0 {
						i += 2
					}
					if ef&0x40 != 0 {
						i += 3
					}
					if ef&0x20 != 0 {
						i += 5
					}
					if i > end {
						return p, ErrPacket
					}
				}
			}
		}
	}
	if p.HasPayload {
		p.Payload = raw[start:]
	}
	return p, nil
}
