package psi

import "encoding/binary"

type TableHeader struct {
	TableID           uint8
	Extension         uint16
	Version           uint8
	Current           bool
	SectionNumber     uint8
	LastSectionNumber uint8
}

func tableHeader(s Section, id byte) (TableHeader, []byte, error) {
	if err := ValidateSection(s.Raw); err != nil {
		return TableHeader{}, nil, err
	}
	b := s.Raw
	if b[0] != id || b[1]&0x80 == 0 {
		return TableHeader{}, nil, ErrSection
	}
	h := TableHeader{TableID: id, Extension: binary.BigEndian.Uint16(b[3:5]), Version: (b[5] >> 1) & 31, Current: b[5]&1 != 0, SectionNumber: b[6], LastSectionNumber: b[7]}
	return h, b[8 : len(b)-4], nil
}

func DecodeSection(s Section) (any, error) {
	if err := ValidateSection(s.Raw); err != nil {
		return nil, err
	}
	switch s.Raw[0] {
	case 0:
		return DecodePAT(s)
	case 2:
		return DecodePMT(s)
	case 0x42:
		return DecodeSDT(s)
	case 0x40:
		return DecodeNIT(s)
	case 0x4e:
		return DecodeEIT(s)
	case 0x73:
		return DecodeTOT(s)
	default:
		return nil, ErrUnsupportedTable
	}
}

func u16(b []byte) uint16   { return binary.BigEndian.Uint16(b) }
func length12(b []byte) int { return int(b[0]&15)<<8 | int(b[1]) }

type PATProgram struct {
	ServiceID uint16
	PMTPID    uint16
}
type PAT struct {
	Section
	Header            TableHeader
	TransportStreamID uint16
	NetworkPID        *uint16
	Programs          []PATProgram
}

func DecodePAT(s Section) (*PAT, error) {
	h, b, err := tableHeader(s, 0)
	if err != nil {
		return nil, err
	}
	if len(s.Raw)-3 > 1021 || len(b)%4 != 0 {
		return nil, ErrSection
	}
	t := &PAT{Section: s, Header: h, TransportStreamID: h.Extension}
	seen := make(map[uint16]bool)
	for len(b) > 0 {
		if b[2]&0xe0 != 0xe0 {
			return nil, ErrSection
		}
		sid := u16(b[:2])
		if seen[sid] {
			return nil, ErrSection
		}
		seen[sid] = true
		pid := u16(b[2:4]) & 0x1fff
		if sid == 0 {
			t.NetworkPID = &pid
		} else {
			t.Programs = append(t.Programs, PATProgram{ServiceID: sid, PMTPID: pid})
		}
		b = b[4:]
	}
	return t, nil
}
