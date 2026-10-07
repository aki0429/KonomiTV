package psi

type ElementaryStream struct {
	StreamType  uint8
	PID         uint16
	Descriptors []Descriptor
}
type PMT struct {
	Section
	Header      TableHeader
	ServiceID   uint16
	PCRPID      uint16
	Descriptors []Descriptor
	Streams     []ElementaryStream
	VideoPIDs   []uint16
	AudioPIDs   []uint16
}

func DecodePMT(s Section) (*PMT, error) {
	h, b, err := tableHeader(s, 2)
	if err != nil {
		return nil, err
	}
	if len(s.Raw)-3 > 1021 || h.SectionNumber != 0 || h.LastSectionNumber != 0 || len(b) < 4 || b[0]&0xe0 != 0xe0 || b[2]&0xf0 != 0xf0 {
		return nil, ErrSection
	}
	n := length12(b[2:4])
	if n > len(b)-4 {
		return nil, ErrSection
	}
	desc, err := DecodeDescriptors(b[4 : 4+n])
	if err != nil {
		return nil, err
	}
	t := &PMT{Section: s, Header: h, ServiceID: h.Extension, PCRPID: u16(b[:2]) & 0x1fff, Descriptors: desc}
	b = b[4+n:]
	for len(b) > 0 {
		if len(b) < 5 || b[1]&0xe0 != 0xe0 || b[3]&0xf0 != 0xf0 {
			return nil, ErrSection
		}
		n = length12(b[3:5])
		if n > len(b)-5 {
			return nil, ErrSection
		}
		desc, err = DecodeDescriptors(b[5 : 5+n])
		if err != nil {
			return nil, err
		}
		es := ElementaryStream{StreamType: b[0], PID: u16(b[1:3]) & 0x1fff, Descriptors: desc}
		t.Streams = append(t.Streams, es)
		switch es.StreamType {
		case 0x01, 0x02, 0x10, 0x1b, 0x24, 0x42:
			t.VideoPIDs = append(t.VideoPIDs, es.PID)
		case 0x03, 0x04, 0x0f, 0x11, 0x81, 0x87:
			t.AudioPIDs = append(t.AudioPIDs, es.PID)
		case 0x06:
			audio := false
			for _, d := range desc {
				switch d.Tag {
				case 0x6a, 0x7a, 0x7b, 0x7c:
					audio = true
				case 5:
					if len(d.Data) >= 4 {
						switch string(d.Data[:4]) {
						case "AC-3", "EAC3", "DTS1", "DTS2", "DTS3":
							audio = true
						}
					}
				}
			}
			if audio {
				t.AudioPIDs = append(t.AudioPIDs, es.PID)
			}
		}
		b = b[5+n:]
	}
	return t, nil
}
