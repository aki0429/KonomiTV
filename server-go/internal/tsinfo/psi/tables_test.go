package psi

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestDescriptorsRawAndBounds(t *testing.T) {
	raw := []byte{0x48, 3, 0x1b, 0xff, 0, 0x99, 0, 0x48, 1, 0xfe}
	got, err := DecodeDescriptors(raw)
	if err != nil || len(got) != 3 {
		t.Fatalf("descriptors %v %v", got, err)
	}
	if got[0].Tag != 0x48 || !bytes.Equal(got[0].Data, []byte{0x1b, 0xff, 0}) || got[1].Tag != 0x99 || len(got[1].Data) != 0 || got[2].Tag != 0x48 {
		t.Fatal("raw descriptors altered")
	}
	raw[2] = 0
	if got[0].Data[0] != 0x1b {
		t.Fatal("data aliases input")
	}
	for _, bad := range [][]byte{{1}, {1, 2, 0}} {
		if _, err := DecodeDescriptors(bad); !errors.Is(err, ErrSection) {
			t.Fatalf("truncation accepted %v", err)
		}
	}
	_ = time.Second
}

func TestDecodeJSTTimeAndDuration(t *testing.T) {
	got, ok := DecodeJSTTime([]byte{0xc9, 0x58, 0x12, 0x34, 0x56})
	want := time.Date(2000, 1, 1, 12, 34, 56, 0, time.FixedZone("JST", 9*60*60))
	if !ok || !got.Equal(want) || got.Hour() != 12 {
		t.Fatalf("JST %v %v", got, ok)
	}
	_, offset := got.Zone()
	if offset != 9*60*60 {
		t.Fatalf("timezone offset %d", offset)
	}
	for _, bad := range [][]byte{nil, {0xc9, 0x58, 0x24, 0, 0}, {0xc9, 0x58, 0x1a, 0, 0}, {0xc9, 0x58, 0, 0x60, 0}, {0xc9, 0x58, 0, 0, 0x60}, {0xff, 0xff, 0xff, 0xff, 0xff}, {0, 0, 0, 0}} {
		v, ok := DecodeJSTTime(bad)
		if ok || !v.IsZero() {
			t.Fatalf("bad time %x: %v %v", bad, v, ok)
		}
	}
	for _, tt := range []struct {
		raw  []byte
		want time.Duration
	}{{[]byte{0x99, 0x59, 0x59}, 99*time.Hour + 59*time.Minute + 59*time.Second}, {[]byte{0, 0, 0}, 0}} {
		v, ok := DecodeDuration(tt.raw)
		if !ok || v != tt.want {
			t.Fatalf("duration %v %v", v, ok)
		}
	}
	for _, bad := range [][]byte{nil, {0xff, 0xff, 0xff}, {0x1a, 0, 0}, {0, 0x60, 0}, {0, 0, 0x60}, {0, 0}} {
		v, ok := DecodeDuration(bad)
		if ok || v != 0 {
			t.Fatalf("bad duration %x", bad)
		}
	}
}

func TestDecodePAT(t *testing.T) {
	raw := syntaxFixture(0, 0x1234, []byte{0, 0, 0xe0, 0x10, 0x10, 1, 0xe1, 0, 0x10, 2, 0xe1, 1})
	s := Section{PID: 0, PacketOffset: 188, Raw: raw}
	got, err := DecodePAT(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.TransportStreamID != 0x1234 || got.NetworkPID == nil || *got.NetworkPID != 0x10 || len(got.Programs) != 2 || got.Programs[0].ServiceID != 0x1001 || got.Programs[0].PMTPID != 0x100 || got.Programs[1].PMTPID != 0x101 || !got.Header.Current || got.Header.SectionNumber != 0 || got.PacketOffset != 188 || !bytes.Equal(got.Raw, raw) {
		t.Fatalf("PAT %+v", got)
	}
	for _, bad := range [][]byte{syntaxFixture(0, 1, []byte{0}), syntaxFixture(0, 1, []byte{0, 1, 0, 1}), syntaxFixture(2, 1, nil)} {
		if _, err := DecodePAT(Section{Raw: bad}); !errors.Is(err, ErrSection) {
			t.Fatalf("bad PAT: %v", err)
		}
	}
	raw[len(raw)-1] ^= 1
	if _, err := DecodePAT(s); !errors.Is(err, ErrCRC) {
		t.Fatalf("CRC unchecked: %v", err)
	}
}

func descFixture(tag byte, data []byte) []byte { return append([]byte{tag, byte(len(data))}, data...) }
func esFixture(typ byte, pid uint16, desc []byte) []byte {
	return append([]byte{typ, 0xe0 | byte(pid>>8), byte(pid), 0xf0 | byte(len(desc)>>8), byte(len(desc))}, desc...)
}
func TestDecodePMT(t *testing.T) {
	pd := descFixture(0x99, []byte{0xff, 0x1b})
	body := append([]byte{0xe1, 0, 0xf0, byte(len(pd))}, pd...)
	body = append(body, esFixture(0x1b, 0x101, descFixture(0x52, []byte{0x10}))...)
	body = append(body, esFixture(0x0f, 0x102, nil)...)
	body = append(body, esFixture(0x06, 0x103, descFixture(0x6a, []byte{0}))...)
	body = append(body, esFixture(0x06, 0x104, descFixture(5, []byte("AC-3")))...)
	body = append(body, esFixture(0x06, 0x105, nil)...)
	got, err := DecodePMT(Section{PID: 0x100, Raw: syntaxFixture(2, 0x1001, body)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ServiceID != 0x1001 || got.PCRPID != 0x100 || len(got.Streams) != 5 || len(got.VideoPIDs) != 1 || got.VideoPIDs[0] != 0x101 || len(got.AudioPIDs) != 3 || got.AudioPIDs[2] != 0x104 || len(got.Descriptors) != 1 || got.Streams[0].Descriptors[0].Tag != 0x52 {
		t.Fatalf("PMT %+v", got)
	}
	for _, bad := range [][]byte{{}, {0xe1, 0, 0xf0, 1}, {0xe1, 0, 0xf0, 0, 0x1b}, {0xe1, 0, 0xf0, 0, 0x1b, 0xe1, 1, 0xf0, 2, 0x52, 1}} {
		if _, err := DecodePMT(Section{Raw: syntaxFixture(2, 1, bad)}); !errors.Is(err, ErrSection) {
			t.Fatalf("bad PMT accepted %x: %v", bad, err)
		}
	}
}

func TestDecodeSDT(t *testing.T) {
	provider := []byte{0x1b, 0x24, 0x42}
	name := []byte{0x0e, 0xff, 0x21}
	d := descFixture(0x48, append(append([]byte{1, byte(len(provider))}, provider...), append([]byte{byte(len(name))}, name...)...))
	d = append(d, descFixture(0xee, []byte{1, 2})...)
	body := append([]byte{0x78, 0x90, 0xff, 0x10, 0x01, 0xff, 0x90, byte(len(d))}, d...)
	got, err := DecodeSDT(Section{PID: 0x11, Raw: syntaxFixture(0x42, 0x1234, body)})
	if err != nil {
		t.Fatal(err)
	}
	if got.TransportStreamID != 0x1234 || got.OriginalNetworkID != 0x7890 || len(got.Services) != 1 {
		t.Fatalf("SDT %+v", got)
	}
	svc := got.Services[0]
	if svc.ServiceID != 0x1001 || !svc.EITSchedule || !svc.EITPresentFollowing || svc.RunningStatus != 4 || !svc.FreeCA || svc.ServiceType != 1 || !bytes.Equal(svc.ProviderName, provider) || !bytes.Equal(svc.ServiceName, name) || len(svc.Descriptors) != 2 {
		t.Fatalf("service %+v", svc)
	}
	for _, bad := range [][]byte{{}, {1, 2, 0xff, 0}, {1, 2, 0xff, 0, 1, 0xff, 0, 2, 0x48, 2}, {1, 2, 0xff, 0, 1, 0xff, 0, 4, 0x48, 2, 1, 2}} {
		if _, err := DecodeSDT(Section{Raw: syntaxFixture(0x42, 1, bad)}); !errors.Is(err, ErrSection) {
			t.Fatalf("bad SDT %x: %v", bad, err)
		}
	}
}

func TestDecodeNIT(t *testing.T) {
	nd := descFixture(0x40, []byte{0x0e, 0x4e})
	td := descFixture(0xcd, []byte{7, 0x0d, 0x0e, 0x54, 0x53, 2, 2, 0x10, 1, 0x10, 2})
	ts := append([]byte{0x12, 0x34, 0x78, 0x90, 0xf0, byte(len(td))}, td...)
	body := append([]byte{0xf0, byte(len(nd))}, nd...)
	body = append(body, 0xf0, byte(len(ts)))
	body = append(body, ts...)
	got, err := DecodeNIT(Section{PID: 0x10, Raw: syntaxFixture(0x40, 0xabcd, body)})
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkID != 0xabcd || len(got.Descriptors) != 1 || len(got.TransportStreams) != 1 {
		t.Fatalf("NIT %+v", got)
	}
	v := got.TransportStreams[0]
	if v.TransportStreamID != 0x1234 || v.OriginalNetworkID != 0x7890 || !v.HasRemoteControlKeyID || v.RemoteControlKeyID != 7 || !bytes.Equal(v.Name, []byte{0x0e, 0x54, 0x53}) || len(v.Descriptors) != 1 {
		t.Fatalf("transport %+v", v)
	}
	for _, bad := range [][]byte{{}, {0xf0, 1}, {0xf0, 0, 0xf0, 1}, {0xf0, 0, 0xf0, 6, 0, 1, 0, 1, 0xf0, 1}, {0xf0, 0, 0xf0, 10, 0, 1, 0, 1, 0xf0, 4, 0xcd, 2, 7, 1}} {
		if _, err := DecodeNIT(Section{Raw: syntaxFixture(0x40, 1, bad)}); !errors.Is(err, ErrSection) {
			t.Fatalf("bad NIT %x: %v", bad, err)
		}
	}
}

func eventFixture(id uint16, start, duration, desc []byte, flags byte) []byte {
	b := []byte{byte(id >> 8), byte(id)}
	b = append(b, start...)
	b = append(b, duration...)
	b = append(b, flags|byte(len(desc)>>8), byte(len(desc)))
	return append(b, desc...)
}
func TestDecodeEIT(t *testing.T) {
	d := descFixture(0x4d, []byte{0x6a, 0x70, 0x6e, 1, 0xff, 0})
	d = append(d, descFixture(0x4e, []byte{0x00, 0x6a, 0x70, 0x6e, 0, 1, 0x1b})...)
	d = append(d, descFixture(0xaa, []byte{0xfe})...)
	body := []byte{0x12, 0x34, 0x78, 0x90, 1, 0x4e}
	body = append(body, eventFixture(0xabcd, []byte{0xc9, 0x58, 0x12, 0x34, 0x56}, []byte{1, 2, 3}, d, 0x90)...)
	body = append(body, eventFixture(0xabce, bytes.Repeat([]byte{0xff}, 5), bytes.Repeat([]byte{0xff}, 3), nil, 0)...)
	raw := syntaxFixture(0x4e, 0x1001, body)
	raw[6], raw[7] = 1, 1
	raw = append(raw[:len(raw)-4], 0, 0, 0, 0)
	sum := fixtureCRC(raw[:len(raw)-4])
	raw[len(raw)-4], raw[len(raw)-3], raw[len(raw)-2], raw[len(raw)-1] = byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum)
	got, err := DecodeEIT(Section{PID: 0x12, Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got.ServiceID != 0x1001 || got.TransportStreamID != 0x1234 || got.OriginalNetworkID != 0x7890 || got.Header.SectionNumber != 1 || got.SegmentLastSectionNumber != 1 || got.LastTableID != 0x4e || len(got.Events) != 2 {
		t.Fatalf("EIT %+v", got)
	}
	e := got.Events[0]
	if e.EventID != 0xabcd || !e.StartTimeValid || e.StartTime.Hour() != 12 || !e.DurationValid || e.Duration != time.Hour+2*time.Minute+3*time.Second || !e.FreeCA || e.RunningStatus != 4 || len(e.Descriptors) != 3 || e.Descriptors[2].Tag != 0xaa {
		t.Fatalf("event %+v", e)
	}
	if got.Events[1].StartTimeValid || got.Events[1].DurationValid || got.Events[1].FreeCA {
		t.Fatal("undefined fields not retained")
	}
	for _, bad := range [][]byte{{}, {0, 1, 0, 1, 0, 0x4e, 0}, {0, 1, 0, 1, 0, 0x4e, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}} {
		if _, err := DecodeEIT(Section{Raw: syntaxFixture(0x4e, 1, bad)}); !errors.Is(err, ErrSection) {
			t.Fatalf("bad EIT accepted %x: %v", bad, err)
		}
	}
}

func totFixture(clock, desc []byte) []byte {
	b := append([]byte{0x73, 0x70, 0}, clock...)
	b = append(b, 0xf0|byte(len(desc)>>8), byte(len(desc)))
	b = append(b, desc...)
	n := len(b) + 1
	b[1] |= byte(n >> 8)
	b[2] = byte(n)
	sum := fixtureCRC(b)
	return append(b, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
}
func TestDecodeTOT(t *testing.T) {
	d := descFixture(0x58, []byte{0x4a, 0x50, 0x4e, 0x02, 0x09, 0, 0xc9, 0x58, 0, 0, 0, 0x09, 0})
	raw := totFixture([]byte{0xc9, 0x58, 0x12, 0x34, 0x56}, d)
	got, err := DecodeTOT(Section{PID: 0x14, Raw: raw})
	if err != nil || !got.ClockValid || got.Clock.Hour() != 12 || len(got.Descriptors) != 1 || got.Descriptors[0].Tag != 0x58 {
		t.Fatalf("TOT %+v %v", got, err)
	}
	unknown, err := DecodeTOT(Section{Raw: totFixture(bytes.Repeat([]byte{0xff}, 5), nil)})
	if err != nil || unknown.ClockValid {
		t.Fatal("TOT undefined clock")
	}
	bad := totFixture([]byte{0xc9, 0x58, 0, 0, 0}, []byte{1})
	if _, err := DecodeTOT(Section{Raw: bad}); !errors.Is(err, ErrSection) {
		t.Fatalf("descriptor truncation %v", err)
	}
	raw[len(raw)-1] ^= 1
	if _, err := DecodeTOT(Section{Raw: raw}); !errors.Is(err, ErrCRC) {
		t.Fatalf("TOT CRC %v", err)
	}
	if _, err := DecodeTOT(Section{Raw: syntaxFixture(0x73, 1, nil)}); !errors.Is(err, ErrSection) {
		t.Fatalf("TOT syntax %v", err)
	}
}

func TestDecodeSectionDispatch(t *testing.T) {
	fixtures := [][]byte{
		syntaxFixture(0, 1, nil),
		syntaxFixture(2, 1, []byte{0xff, 0xff, 0xf0, 0}),
		syntaxFixture(0x42, 1, []byte{0, 1, 0xff}),
		syntaxFixture(0x40, 1, []byte{0xf0, 0, 0xf0, 0}),
		syntaxFixture(0x4e, 1, []byte{0, 1, 0, 1, 0, 0x4e}),
		totFixture([]byte{0xc9, 0x58, 0, 0, 0}, nil),
	}
	for _, raw := range fixtures {
		got, err := DecodeSection(Section{Raw: raw})
		if err != nil {
			t.Fatalf("table %02x: %v", raw[0], err)
		}
		var id byte
		switch got.(type) {
		case *PAT:
			id = 0
		case *PMT:
			id = 2
		case *SDT:
			id = 0x42
		case *NIT:
			id = 0x40
		case *EIT:
			id = 0x4e
		case *TOT:
			id = 0x73
		default:
			t.Fatalf("unexpected type %T", got)
		}
		if id != raw[0] {
			t.Fatalf("wrong type %T", got)
		}
	}
	if _, err := DecodeSection(Section{Raw: syntaxFixture(0x46, 1, nil)}); !errors.Is(err, ErrUnsupportedTable) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := DecodeSection(Section{}); !errors.Is(err, ErrSection) {
		t.Fatalf("empty: %v", err)
	}
}

// TDD: tables_test.go additions.
