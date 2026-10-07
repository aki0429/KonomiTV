// Package psi implements bounded MPEG-TS packet and PSI/SI parsing.
package psi

// ValidateSection checks framing and CRC for syntax sections and TOT.
func ValidateSection(raw []byte) error {
	if len(raw) < 3 || len(raw) > MaxSectionSize || raw[0] == 0xff {
		return ErrSection
	}
	n := int(raw[1]&0x0f)<<8 | int(raw[2])
	if n+3 != len(raw) || raw[1]&0x30 != 0x30 {
		return ErrSection
	}
	if raw[1]&0x80 != 0 {
		if n < 9 || raw[5]&0xc0 != 0xc0 || raw[6] > raw[7] {
			return ErrSection
		}
	}
	if raw[1]&0x80 != 0 || raw[0] == 0x73 {
		if len(raw) < 7 {
			return ErrSection
		}
		if CRC32MPEG2(raw) != 0 {
			return ErrCRC
		}
	}
	return nil
}

// CRC32MPEG2 computes the non-reflected MPEG-2 CRC (no final XOR).
func CRC32MPEG2(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
