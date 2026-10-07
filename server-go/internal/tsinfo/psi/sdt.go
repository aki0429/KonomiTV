package psi

import "bytes"

type Service struct {
	ServiceID           uint16
	EITSchedule         bool
	EITPresentFollowing bool
	RunningStatus       uint8
	FreeCA              bool
	Descriptors         []Descriptor
	ServiceType         uint8
	ProviderName        []byte
	ServiceName         []byte
}
type SDT struct {
	Section
	Header            TableHeader
	TransportStreamID uint16
	OriginalNetworkID uint16
	Services          []Service
}

func DecodeSDT(s Section) (*SDT, error) {
	h, b, err := tableHeader(s, 0x42)
	if err != nil {
		return nil, err
	}
	if len(b) < 3 {
		return nil, ErrSection
	}
	t := &SDT{Section: s, Header: h, TransportStreamID: h.Extension, OriginalNetworkID: u16(b[:2])}
	b = b[3:]
	for len(b) > 0 {
		if len(b) < 5 {
			return nil, ErrSection
		}
		n := length12(b[3:5])
		if n > len(b)-5 {
			return nil, ErrSection
		}
		desc, err := DecodeDescriptors(b[5 : 5+n])
		if err != nil {
			return nil, err
		}
		svc := Service{ServiceID: u16(b[:2]), EITSchedule: b[2]&2 != 0, EITPresentFollowing: b[2]&1 != 0, RunningStatus: b[3] >> 5, FreeCA: b[3]&0x10 != 0, Descriptors: desc}
		for _, d := range desc {
			if d.Tag != 0x48 {
				continue
			}
			v := d.Data
			if len(v) < 3 {
				return nil, ErrSection
			}
			p := int(v[1])
			if p+3 > len(v) {
				return nil, ErrSection
			}
			n := int(v[2+p])
			if p+3+n != len(v) {
				return nil, ErrSection
			}
			svc.ServiceType = v[0]
			svc.ProviderName = bytes.Clone(v[2 : 2+p])
			svc.ServiceName = bytes.Clone(v[3+p:])
		}
		t.Services = append(t.Services, svc)
		b = b[5+n:]
	}
	return t, nil
}
