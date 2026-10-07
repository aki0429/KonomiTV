package psi

import "bytes"

type TransportStream struct {
	TransportStreamID     uint16
	OriginalNetworkID     uint16
	Descriptors           []Descriptor
	RemoteControlKeyID    uint8
	HasRemoteControlKeyID bool
	Name                  []byte
}
type NIT struct {
	Section
	Header           TableHeader
	NetworkID        uint16
	Descriptors      []Descriptor
	TransportStreams []TransportStream
}

func DecodeNIT(s Section) (*NIT, error) {
	h, b, err := tableHeader(s, 0x40)
	if err != nil {
		return nil, err
	}
	if len(b) < 2 || b[0]&0xf0 != 0xf0 {
		return nil, ErrSection
	}
	n := length12(b[:2])
	if n > len(b)-2 {
		return nil, ErrSection
	}
	desc, err := DecodeDescriptors(b[2 : 2+n])
	if err != nil {
		return nil, err
	}
	t := &NIT{Section: s, Header: h, NetworkID: h.Extension, Descriptors: desc}
	b = b[2+n:]
	if len(b) < 2 || b[0]&0xf0 != 0xf0 || length12(b[:2]) != len(b)-2 {
		return nil, ErrSection
	}
	b = b[2:]
	for len(b) > 0 {
		if len(b) < 6 || b[4]&0xf0 != 0xf0 {
			return nil, ErrSection
		}
		n = length12(b[4:6])
		if n > len(b)-6 {
			return nil, ErrSection
		}
		desc, err = DecodeDescriptors(b[6 : 6+n])
		if err != nil {
			return nil, err
		}
		ts := TransportStream{TransportStreamID: u16(b[:2]), OriginalNetworkID: u16(b[2:4]), Descriptors: desc}
		for _, d := range desc {
			if d.Tag != 0xcd {
				continue
			}
			v := d.Data
			if len(v) < 2 {
				return nil, ErrSection
			}
			namelen := int(v[1] >> 2)
			if namelen > len(v)-2 {
				return nil, ErrSection
			}
			rest := v[2+namelen:]
			for range int(v[1] & 3) {
				if len(rest) < 2 {
					return nil, ErrSection
				}
				size := 2 + int(rest[1])*2
				if size > len(rest) {
					return nil, ErrSection
				}
				rest = rest[size:]
			}
			if len(rest) != 0 {
				return nil, ErrSection
			}
			ts.RemoteControlKeyID = v[0]
			ts.HasRemoteControlKeyID = true
			ts.Name = bytes.Clone(v[2 : 2+namelen])
		}
		t.TransportStreams = append(t.TransportStreams, ts)
		b = b[6+n:]
	}
	return t, nil
}
