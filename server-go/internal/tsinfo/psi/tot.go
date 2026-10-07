package psi

import "time"

type TOT struct {
	Section
	Clock       time.Time
	ClockValid  bool
	Descriptors []Descriptor
}

func DecodeTOT(s Section) (*TOT, error) {
	if err := ValidateSection(s.Raw); err != nil {
		return nil, err
	}
	b := s.Raw
	if b[0] != 0x73 || b[1]&0x80 != 0 || len(b) < 14 || b[8]&0xf0 != 0xf0 {
		return nil, ErrSection
	}
	n := length12(b[8:10])
	if n != len(b)-14 {
		return nil, ErrSection
	}
	desc, err := DecodeDescriptors(b[10 : 10+n])
	if err != nil {
		return nil, err
	}
	clock, ok := DecodeJSTTime(b[3:8])
	return &TOT{Section: s, Clock: clock, ClockValid: ok, Descriptors: desc}, nil
}
