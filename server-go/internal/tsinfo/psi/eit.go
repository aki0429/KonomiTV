package psi

import "time"

type Event struct {
	EventID        uint16
	StartTime      time.Time
	StartTimeValid bool
	Duration       time.Duration
	DurationValid  bool
	RunningStatus  uint8
	FreeCA         bool
	Descriptors    []Descriptor
}
type EIT struct {
	Section
	Header                   TableHeader
	ServiceID                uint16
	TransportStreamID        uint16
	OriginalNetworkID        uint16
	SegmentLastSectionNumber uint8
	LastTableID              uint8
	Events                   []Event
}

func DecodeEIT(s Section) (*EIT, error) {
	h, b, err := tableHeader(s, 0x4e)
	if err != nil {
		return nil, err
	}
	if len(b) < 6 {
		return nil, ErrSection
	}
	t := &EIT{Section: s, Header: h, ServiceID: h.Extension, TransportStreamID: u16(b[:2]), OriginalNetworkID: u16(b[2:4]), SegmentLastSectionNumber: b[4], LastTableID: b[5]}
	b = b[6:]
	for len(b) > 0 {
		if len(b) < 12 {
			return nil, ErrSection
		}
		n := length12(b[10:12])
		if n > len(b)-12 {
			return nil, ErrSection
		}
		desc, err := DecodeDescriptors(b[12 : 12+n])
		if err != nil {
			return nil, err
		}
		start, startOK := DecodeJSTTime(b[2:7])
		duration, durationOK := DecodeDuration(b[7:10])
		t.Events = append(t.Events, Event{EventID: u16(b[:2]), StartTime: start, StartTimeValid: startOK, Duration: duration, DurationValid: durationOK, RunningStatus: b[10] >> 5, FreeCA: b[10]&0x10 != 0, Descriptors: desc})
		b = b[12+n:]
	}
	return t, nil
}
