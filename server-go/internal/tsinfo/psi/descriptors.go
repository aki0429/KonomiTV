package psi

import "bytes"

type Descriptor struct {
	Tag  uint8
	Data []byte
}

func DecodeDescriptors(raw []byte) ([]Descriptor, error) {
	var out []Descriptor
	for len(raw) > 0 {
		if len(raw) < 2 || int(raw[1])+2 > len(raw) {
			return nil, ErrSection
		}
		n := int(raw[1]) + 2
		out = append(out, Descriptor{Tag: raw[0], Data: bytes.Clone(raw[2:n])})
		raw = raw[n:]
	}
	return out, nil
}
