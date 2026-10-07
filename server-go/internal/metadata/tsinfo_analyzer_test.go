package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/token"

	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// These fixtures are independently constructed, synthetic PSI, not broadcasts.
func tsinfoSyntax(id byte, extension uint16, number byte, body []byte) []byte {
	raw := []byte{id, 0xb0, 0, byte(extension >> 8), byte(extension), 0xc1, number, number}
	raw = append(raw, body...)
	length := len(raw) - 3 + 4
	raw[1] |= byte(length >> 8)
	raw[2] = byte(length)
	return binary.BigEndian.AppendUint32(raw, psi.CRC32MPEG2(raw))
}

func tsinfoDescriptor(tag byte, body []byte) []byte {
	return append([]byte{tag, byte(len(body))}, body...)
}

func tsinfoText(s string) []byte { return append([]byte{0x0e}, []byte(s)...) }

func tsinfoShort(title, text string) []byte {
	a, b := tsinfoText(title), tsinfoText(text)
	body := append([]byte{'j', 'p', 'n', byte(len(a))}, a...)
	body = append(body, byte(len(b)))
	return tsinfoDescriptor(0x4d, append(body, b...))
}

func tsinfoPAT(tsid uint16, services ...psi.PATProgram) []byte {
	var body []byte
	for _, s := range services {
		body = append(body, byte(s.ServiceID>>8), byte(s.ServiceID), 0xe0|byte(s.PMTPID>>8), byte(s.PMTPID))
	}
	return tsinfoSyntax(0, tsid, 0, body)
}

func tsinfoPMT(sid, pcrPID, videoPID, audioPID uint16) []byte {
	return tsinfoSyntax(2, sid, 0, []byte{
		0xe0 | byte(pcrPID>>8), byte(pcrPID), 0xf0, 0,
		2, 0xe0 | byte(videoPID>>8), byte(videoPID), 0xf0, 0,
		0x0f, 0xe0 | byte(audioPID>>8), byte(audioPID), 0xf0, 0,
	})
}

func tsinfoSDT(tsid, onid uint16, services ...psi.PATProgram) []byte {
	body := []byte{byte(onid >> 8), byte(onid), 0xff}
	for _, s := range services {
		name := tsinfoText("SYNTHETIC")
		desc := tsinfoDescriptor(0x48, append([]byte{1, 0, byte(len(name))}, name...))
		body = append(body, byte(s.ServiceID>>8), byte(s.ServiceID), 0xff, 0x80|byte(len(desc)>>8), byte(len(desc)))
		body = append(body, desc...)
	}
	return tsinfoSyntax(0x42, tsid, 0, body)
}

func tsinfoTime(clock time.Time) []byte {
	epoch := time.Date(1858, 11, 17, 0, 0, 0, 0, time.UTC)
	day := time.Date(clock.Year(), clock.Month(), clock.Day(), 0, 0, 0, 0, time.UTC)
	mjd := uint16(day.Sub(epoch) / (24 * time.Hour))
	bcd := func(n int) byte { return byte(n/10<<4 | n%10) }
	return []byte{byte(mjd >> 8), byte(mjd), bcd(clock.Hour()), bcd(clock.Minute()), bcd(clock.Second())}
}

func tsinfoEIT(tsid, onid, sid uint16, number byte, eventID uint16, start time.Time, descriptors []byte) []byte {
	body := []byte{byte(tsid >> 8), byte(tsid), byte(onid >> 8), byte(onid), number, 0x4e,
		byte(eventID >> 8), byte(eventID)}
	body = append(body, tsinfoTime(start)...)
	body = append(body, 0, 1, 0, 0x80|byte(len(descriptors)>>8), byte(len(descriptors)))
	body = append(body, descriptors...)
	return tsinfoSyntax(0x4e, sid, number, body)
}

func tsinfoPackets(pid uint16, section []byte, counters map[uint16]byte) []byte {
	payload := append([]byte{0}, section...)
	var packets []byte
	for len(payload) > 0 {
		p := make([]byte, psi.PacketSize)
		for i := range p {
			p[i] = 0xff
		}
		p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8), byte(pid), 0x10|counters[pid]
		if len(packets) == 0 {
			p[1] |= 0x40
		}
		counters[pid] = (counters[pid] + 1) & 15
		n := copy(p[4:], payload)
		payload = payload[n:]
		packets = append(packets, p...)
	}
	return packets
}

func tsinfoFixture(t *testing.T) (*RecordedVideo, []byte) {
	t.Helper()
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	cc := map[uint16]byte{}
	var data []byte
	for range 50 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{
			{0, tsinfoPAT(1, s)}, {0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)},
			{0x11, tsinfoSDT(1, 4, s)},
			{0x12, tsinfoEIT(1, 4, 101, 0, 10, start, tsinfoShort("PRESENT", "DESCRIPTION"))},
			{0x12, tsinfoEIT(1, 4, 101, 1, 11, start.Add(time.Minute), tsinfoShort("FOLLOWING", "NEXT"))},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	path := filepath.Join(t.TempDir(), "synthetic.ts")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return &RecordedVideo{FilePath: path, FileSize: int64(len(data)), ContainerFormat: "MPEG-TS", Duration: 60}, data
}

func tsinfoNIT(tsid, onid uint16, remocon byte) []byte {
	transport := []byte{byte(tsid >> 8), byte(tsid), byte(onid >> 8), byte(onid), 0xf0, 4, 0xcd, 2, remocon, 0}
	return tsinfoSyntax(0x40, onid, 0, append([]byte{0xf0, 0, 0xf0, byte(len(transport))}, transport...))
}

func TestTSInfoUsesMatchingNITAndReadOnlyChannelDB(t *testing.T) {
	if _, err := psi.DecodeNIT(psi.Section{Raw: tsinfoNIT(1, 0x7fe0, 1)}); err != nil {
		t.Fatalf("invalid synthetic NIT: %v", err)
	}
	store := openTestDB(t)
	for _, tc := range []struct {
		nid, sid     int
		number, kind string
	}{{1, 1, "011", "GR"}, {2, 2, "011-1", "GR"}, {3, 3, "011-3", "GR"}, {4, 4, "011-2", "BS"}, {0x7fe0, 1024, "011-8", "GR"}} {
		_, err := store.DB.Exec(`INSERT INTO channels(id,display_channel_id,network_id,service_id,remocon_id,channel_number,type,name,is_subchannel,is_radiochannel,is_watchable) VALUES(?,?,?,?,?,?,?,?,0,0,1)`, tsinfoChannelID(tc.nid, tc.sid), tc.kind+tc.number, tc.nid, tc.sid, 1, tc.number, tc.kind, "synthetic")
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 1024, PMTPID: 0x100}
	cc := map[uint16]byte{}
	var data []byte
	for range 100 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{
			{0, tsinfoPAT(1, s)}, {0x100, tsinfoPMT(1024, 0x200, 0x200, 0x201)}, {0x11, tsinfoSDT(1, 0x7fe0, s)},
			{0x10, tsinfoNIT(9, 0x7fe0, 9)}, {0x10, tsinfoNIT(1, 0x7fe0, 1)},
			{0x12, tsinfoEIT(1, 0x7fe0, 1024, 0, 10, start, tsinfoShort("GR", "DESCRIPTION"))},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	path := filepath.Join(t.TempDir(), "terrestrial.ts")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	video := &RecordedVideo{FilePath: path, FileSize: int64(len(data)), ContainerFormat: "MPEG-TS", Duration: 60}
	p, ok := NewTSInfoProgramAnalyzer(store.DB, testLogger(t)).AnalyzeProgram(video, nil)
	if !ok || p.Channel.RemoconID != 1 || p.Channel.ChannelNumber != "011-2" || p.Channel.DisplayChannelID != "gr011-2" {
		t.Fatalf("NIT/DB numbering mismatch: %#v", p)
	}
	var count int
	if err := store.DB.QueryRow("SELECT count(*) FROM channels").Scan(&count); err != nil || count != 5 {
		t.Fatalf("read-only DB changed: %d %v", count, err)
	}
}

func tsinfoExtended(number, last byte, items ...[2][]byte) []byte {
	var data []byte
	for _, item := range items {
		data = append(data, byte(len(item[0])))
		data = append(data, item[0]...)
		data = append(data, byte(len(item[1])))
		data = append(data, item[1]...)
	}
	body := append([]byte{number<<4 | last, 'j', 'p', 'n', byte(len(data))}, data...)
	return tsinfoDescriptor(0x4e, append(body, 0))
}

func TestTSInfoMergesExtendedFragmentsAndHeadingCollisions(t *testing.T) {
	video, _ := tsinfoFixture(t)
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	first := tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}})
	last := tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}}, [2][]byte{tsinfoText("HEAD"), tsinfoText("SECOND")})
	cc := map[uint16]byte{}
	var data []byte
	for range 50 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{
			{0, tsinfoPAT(1, s)}, {0x11, tsinfoSDT(1, 4, s)}, {0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)},
			{0x12, tsinfoEIT(1, 4, 101, 0, 10, start, append(tsinfoShort("DETAIL", ""), last...))},
			{0x12, tsinfoEIT(1, 4, 101, 0, 10, start, first)},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(data))
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	want := []DetailEntry{{Key: "HEAD", Value: "AB"}, {Key: "HEAD\t", Value: "SECOND"}}
	if !ok || !reflect.DeepEqual(p.Detail, want) || p.Description != "AB" {
		t.Fatalf("extended merge mismatch: %#v", p)
	}
}

func TestTSInfoDecodesGenreAndDualMonoAudio(t *testing.T) {
	video, _ := tsinfoFixture(t)
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	desc := tsinfoShort("AUDIO", "GENRE")
	desc = append(desc, tsinfoDescriptor(0x54, []byte{0x00, 0, 0xe0, 1, 0xe1, 0})...)
	desc = append(desc, tsinfoDescriptor(0xc4, []byte{0xf2, 2, 0x10, 0x0f, 0, 0xc0, 'j', 'p', 'n', 'e', 'n', 'g'})...)
	desc = append(desc, tsinfoDescriptor(0xc4, []byte{0xf2, 3, 0x11, 0x0f, 0, 0, 'e', 'n', 'g'})...)
	cc := map[uint16]byte{}
	var data []byte
	for range 50 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{{0, tsinfoPAT(1, s)}, {0x11, tsinfoSDT(1, 4, s)}, {0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)}, {0x12, tsinfoEIT(1, 4, 101, 0, 10, start, desc)}} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(data))
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	want := []Genre{{Major: "ニュース・報道", Middle: "定時・総合"}, {Major: "拡張", Middle: "延長の可能性あり"}}
	if !ok || !reflect.DeepEqual(p.Genres, want) {
		t.Fatalf("genre mismatch: %#v", p)
	}
	if p.PrimaryAudioType != "1/0+1/0モード(デュアルモノ)" || p.PrimaryAudioLanguage != "日本語+英語" || p.SecondaryAudioType == nil || *p.SecondaryAudioType != DefaultPrimaryAudioType || p.SecondaryAudioLanguage == nil || *p.SecondaryAudioLanguage != "英語" {
		t.Fatal("audio descriptor mismatch")
	}
}

func TestTSInfoUpdatesUnknownScheduleWithoutMixingEvents(t *testing.T) {
	video, _ := tsinfoFixture(t)
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	unknown := tsinfoEIT(1, 4, 101, 0, 10, start, tsinfoShort("SCHEDULE", "UNKNOWN"))
	unknown[21], unknown[22], unknown[23] = 0xff, 0xff, 0xff
	binary.BigEndian.PutUint32(unknown[len(unknown)-4:], psi.CRC32MPEG2(unknown[:len(unknown)-4]))
	cc := map[uint16]byte{}
	var data []byte
	for range 100 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{{0, tsinfoPAT(1, s)}, {0x11, tsinfoSDT(1, 4, s)}, {0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)},
			{0x12, unknown}, {0x12, tsinfoEIT(9, 4, 101, 0, 99, start, tsinfoShort("WRONG TSID", "WRONG"))},
			{0x12, tsinfoEIT(1, 4, 101, 0, 10, start, tsinfoShort("SCHEDULE", "CONFIRMED"))},
			{0x12, tsinfoEIT(1, 4, 101, 0, 99, start, tsinfoShort("DIFFERENT EVENT", "WRONG"))},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(data))
	video.Duration = 90
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	if !ok || p.Duration != 60 || p.Title != "SCHEDULE" || p.Description != "CONFIRMED" {
		t.Fatal("same event schedule must update, foreign event/TSID must not merge")
	}
}

func TestTSInfoAcceptsRecorderPATReservedBitsOnlyAfterCRC(t *testing.T) {
	video, data := tsinfoFixture(t)
	for i := 0; i+188 <= len(data); i += 188 {
		if data[i+1]&31 != 0 || data[i+2] != 0 {
			continue
		}
		s := data[i+5:]
		length := 3 + (int(s[1]&15)<<8 | int(s[2]))
		s = s[:length]
		for j := 8; j < len(s)-4; j += 4 {
			s[j+2] &= 31
		}
		binary.BigEndian.PutUint32(s[len(s)-4:], psi.CRC32MPEG2(s[:len(s)-4]))
	}
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	if !ok || p.Title != "PRESENT" {
		t.Fatal("CRC-valid recorder PAT with zero reserved bits must be usable")
	}
	for i := 0; i+188 <= len(data); i += 188 {
		if data[i+1]&31 == 0 && data[i+2] == 0 {
			data[i+8] ^= 1
		}
	}
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil); ok || p != nil {
		t.Fatal("CRC-corrupt PAT must never be repaired")
	}
}

type tsinfoBudgetReader struct {
	*bytes.Reader
	read   int64
	ranges [][2]int64
}

func (r *tsinfoBudgetReader) Read(b []byte) (int, error) {
	pos, _ := r.Reader.Seek(0, io.SeekCurrent)
	n, err := r.Reader.Read(b)
	r.read += int64(n)
	r.ranges = append(r.ranges, [2]int64{pos, pos + int64(n)})
	return n, err
}

func TestTSInfoSharesBeginningAndTwentyPercentWindowBudget(t *testing.T) {
	video, data := tsinfoFixture(t)
	r := &tsinfoBudgetReader{Reader: bytes.NewReader(data)}
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).analyzeTSInfo(context.Background(), r, video, int64(len(data)), nil)
	if !ok || p == nil {
		t.Fatal("analysis unavailable")
	}
	end := int64(len(data))
	offset := closestMultiple(end/5, 188)
	if r.read > end+(end-offset) {
		t.Fatalf("duplicate window reads violate budget: %d", r.read)
	}
	for _, window := range r.ranges {
		if window[1] > end {
			t.Fatal("read beyond effective zero-padding boundary")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.read = 0
	if p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(ctx, r, video, end, nil); ok || p != nil || r.read != 0 {
		t.Fatal("cancelled context must not read")
	}

}

type tsinfoSparseReader struct {
	blocks    []tsinfoReadWindow
	position  int64
	bytesRead int64
}

func (r *tsinfoSparseReader) Read(b []byte) (int, error) {
	for _, w := range r.blocks {
		if r.position >= w.offset && r.position < w.offset+int64(len(w.data)) {
			n := copy(b, w.data[r.position-w.offset:])
			r.position += int64(n)
			r.bytesRead += int64(n)
			return n, nil
		}
	}
	return 0, io.EOF
}
func (r *tsinfoSparseReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekCurrent {
		offset += r.position
	} else if whence != io.SeekStart {
		return 0, psi.ErrOptions
	}
	r.position = offset
	return offset, nil
}

func TestTSInfoResolvesUnknownDurationInLimitedAdditionalWindow(t *testing.T) {
	video, data := tsinfoFixture(t)
	unknown := append([]byte(nil), data...)
	for i := 0; i+188 <= len(unknown); i += 188 {
		if (uint16(unknown[i+1]&31)<<8 | uint16(unknown[i+2])) != 0x12 {
			continue
		}
		s := unknown[i+5:]
		n := 3 + (int(s[1]&15)<<8 | int(s[2]))
		s = s[:n]
		s[21], s[22], s[23] = 0xff, 0xff, 0xff
		binary.BigEndian.PutUint32(s[len(s)-4:], psi.CRC32MPEG2(s[:len(s)-4]))
	}
	end := int64(1 << 30)
	offset := closestMultiple(end/5, 188)
	r := &tsinfoSparseReader{blocks: []tsinfoReadWindow{{offset: 0, data: data}, {offset: offset, data: unknown}, {offset: offset + tsinfoWindowBytes + 188*1000000, data: data}}}
	video.Duration = 90
	video.FileSize = end
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).analyzeTSInfo(context.Background(), r, video, end, nil)
	if !ok || p.Duration != 60 {
		t.Fatal("limited additional EIT window did not resolve duration")
	}
	if r.bytesRead > 2*tsinfoWindowBytes+3*(8<<20) {
		t.Fatal("finite source I/O budget exceeded")
	}
}

func TestTSInfoMetadataAPIWiresProductionAnalyzer(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../api/metadata.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(file, func(node ast.Node) bool {
		kv, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "ProgramAnalyzer" {
			return true
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return true
		}
		f, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || f.Sel.Name != "NewTSInfoProgramAnalyzer" || len(call.Args) != 2 {
			return true
		}
		db, ok1 := call.Args[0].(*ast.SelectorExpr)
		logger, ok2 := call.Args[1].(*ast.SelectorExpr)
		wired = ok1 && ok2 && db.Sel.Name == "db" && logger.Sel.Name == "logger"
		return true
	})
	if !wired {
		t.Fatal("metadata API must inject production TS analyzer with read DB/logger")
	}
}

func TestTSInfoReadsActualPresentFollowingOnAdditionalEITPIDs(t *testing.T) {
	for _, pid := range []uint16{0x26, 0x27} {
		video, data := tsinfoFixture(t)
		for i := 0; i+188 <= len(data); i += 188 {
			if (uint16(data[i+1]&31)<<8 | uint16(data[i+2])) == 0x12 {
				data[i+1] = data[i+1]&0xe0 | byte(pid>>8)
				data[i+2] = byte(pid)
			}
		}
		if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
			t.Fatal(err)
		}
		p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
		if !ok || p.Title != "PRESENT" {
			t.Fatalf("EIT PID %#x missing", pid)
		}
	}
}

func TestTSInfoPreservesDescriptorStateUntilUnknownFollowingStarts(t *testing.T) {
	video, _ := tsinfoFixture(t)
	c := &Channel{NetworkID: 4, ServiceID: 101}
	state := tsinfoEventState{}
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	last, _ := psi.DecodeDescriptors(tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}}))
	short, _ := psi.DecodeDescriptors(tsinfoShort("FOLLOWING", ""))
	state.merge(video, c, psi.Event{EventID: 11, Descriptors: append(short, last...)})
	first, _ := psi.DecodeDescriptors(tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}}))
	state.merge(video, c, psi.Event{EventID: 11, StartTime: start, StartTimeValid: true, Duration: time.Minute, DurationValid: true, Descriptors: first})
	if state.program == nil || state.program.Title != "FOLLOWING" || state.program.Description != "AB" || len(state.program.Detail) != 1 || state.program.Detail[0].Value != "AB" {
		t.Fatal("unknown start must retain title/fragments for same event")
	}
}

func TestTSInfoPreservesConfirmedDurationBeforeStartIsKnown(t *testing.T) {
	video, _ := tsinfoFixture(t)
	state := tsinfoEventState{}
	channel := &Channel{NetworkID: 4, ServiceID: 101}
	state.merge(video, channel, psi.Event{EventID: 11, Duration: time.Minute, DurationValid: true})
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	state.merge(video, channel, psi.Event{EventID: 11, StartTime: start, StartTimeValid: true})
	if state.program == nil || state.program.Duration != 60 || !state.program.EndTime.Equal(start.Add(time.Minute)) {
		t.Fatal("confirmed duration lost while waiting for start")
	}
}

func TestTSInfoUsesRecordingEndForUnconfirmedDuration(t *testing.T) {
	video, _ := tsinfoFixture(t)
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	end := start.Add(75 * time.Second)
	video.RecordingEndTime = &end
	video.Duration = 90
	state := tsinfoEventState{}
	state.merge(video, &Channel{NetworkID: 4, ServiceID: 101}, psi.Event{EventID: 10, StartTime: start, StartTimeValid: true})
	if state.program == nil || !state.program.EndTime.Equal(end) || state.program.Duration != 75 {
		t.Fatal("unknown duration must use recording end before media duration")
	}
}

func TestTSInfoFollowingWinsOnlyWithinRecordingStartMargin(t *testing.T) {
	for _, tc := range []struct {
		delta time.Duration
		title string
	}{{0, "FOLLOWING"}, {60 * time.Second, "FOLLOWING"}, {61 * time.Second, "PRESENT"}, {-time.Second, "PRESENT"}} {
		video, _ := tsinfoFixture(t)
		start := time.Date(2026, 10, 7, 10, 1, 0, 0, constants.JST).Add(-tc.delta)
		video.RecordingStartTime = &start
		p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
		if !ok || p.Title != tc.title {
			t.Fatalf("following margin %s: wrong selection", tc.delta)
		}
	}
}

func TestTSInfoRejectsNonFiniteDuration(t *testing.T) {
	for _, duration := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0} {
		video, _ := tsinfoFixture(t)
		video.Duration = duration
		if p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil); ok || p != nil {
			t.Fatal("invalid media duration must fall back")
		}
	}
}

func TestTSInfoNilFallbackAndZeroPaddingBoundary(t *testing.T) {
	video, data := tsinfoFixture(t)
	end := int64(len(data))
	data = append(data, make([]byte, len(data)*8)...)
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(data))
	a := NewTSInfoProgramAnalyzer(nil, testLogger(t))
	p, ok := a.AnalyzeProgramContext(context.Background(), video, &end, nil)
	if !ok || p.Title != "PRESENT" {
		t.Fatal("effective endoffset must choose EIT before zero padding")
	}
	for _, boundary := range []int64{-1, 0, 187} {
		if p, ok := a.AnalyzeProgramContext(context.Background(), video, &boundary, nil); ok || p != nil {
			t.Fatal("invalid boundary must return nil")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, ok := a.AnalyzeProgramContext(ctx, video, &end, nil); ok || p != nil {
		t.Fatal("cancelled public API must return nil")
	}
	video.ContainerFormat = "MPEG-4"
	if p, ok := a.AnalyzeProgram(video, nil); ok || p != nil {
		t.Fatal(".psc/non TS is explicitly unsupported")
	}
	video.ContainerFormat = "MPEG-TS"
	for _, bad := range [][]byte{make([]byte, 8192), {0x47, 0, 0, 0}, bytes.Repeat([]byte{0x47, 0xff, 0xff, 0xff}, 400)} {
		if err := os.WriteFile(video.FilePath, bad, 0600); err != nil {
			t.Fatal(err)
		}
		video.FileSize = int64(len(bad))
		if p, ok := a.AnalyzeProgram(video, nil); ok || p != nil {
			t.Fatal("malformed recording must return nil")
		}
	}
}

func TestTSInfoTOTDoesNotCrossDiscontinuityOrInventAnotherPIDClock(t *testing.T) {
	video, data := tsinfoFixture(t)
	clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
	prefix := tsinfoPCRPacket(0x200, 100000, 0)
	broken := tsinfoPCRPacket(0x200, 300000, 0)
	broken[5] |= 0x80
	prefix = append(prefix, broken...)
	prefix = append(prefix, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
	if err := os.WriteFile(video.FilePath, append(prefix, data...), 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(prefix) + len(data))
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	if !ok || p == nil || video.RecordingStartTime != nil || video.RecordingEndTime != nil {
		t.Fatal("discontinuity must leave timestamps unknown")
	}
	prefix = append(tsinfoPCRPacket(0x999, 100000, 0), tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
	if err := os.WriteFile(video.FilePath, append(prefix, data...), 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(prefix) + len(data))
	if p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil); !ok || p == nil || video.RecordingStartTime != nil || video.RecordingEndTime != nil {
		t.Fatal("foreign PCR must leave timestamps unknown")
	}
}

func TestTSInfoParallelCallsHaveNoSharedResult(t *testing.T) {
	video, _ := tsinfoFixture(t)
	a := NewTSInfoProgramAnalyzer(nil, testLogger(t))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := *video
			p, ok := a.AnalyzeProgramContext(context.Background(), &local, nil, nil)
			if !ok || p.Title != "PRESENT" || local.RecordingStartTime != nil {
				t.Error("independent call result mismatch")
			}
		}()
	}
	wg.Wait()
}

func tsinfoPCRPacket(pid uint16, base uint64, extension uint16) []byte {
	p := make([]byte, psi.PacketSize)
	for i := range p {
		p[i] = 0xff
	}
	p[0], p[1], p[2], p[3], p[4], p[5] = 0x47, byte(pid>>8), byte(pid), 0x20, 183, 0x10
	p[6], p[7], p[8], p[9], p[10], p[11] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1), byte((base&1)<<7)|0x7e|byte(extension>>8), byte(extension)
	return p
}

func tsinfoTOT(clock time.Time) []byte {
	raw := append([]byte{0x73, 0x70, 11}, tsinfoTime(clock)...)
	raw = append(raw, 0xf0, 0)
	return binary.BigEndian.AppendUint32(raw, psi.CRC32MPEG2(raw))
}

func TestTSInfoRecordsTOTWithSelectedPMTPCRAndWrap(t *testing.T) {
	video, original := tsinfoFixture(t)
	clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
	data := tsinfoPCRPacket(0x200, (1<<33)-90000, 0)
	data = append(data, tsinfoPCRPacket(0x999, 2000000, 0)...)
	data = append(data, tsinfoPCRPacket(0x200, 90000, 0)...)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
	data = append(data, original...)
	if err := os.WriteFile(video.FilePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	video.FileSize = int64(len(data))
	p, ok := NewTSInfoProgramAnalyzer(nil, testLogger(t)).AnalyzeProgram(video, nil)
	want := clock.Add(-2 * time.Second)
	if !ok || video.RecordingStartTime == nil || !video.RecordingStartTime.Equal(want) || video.RecordingEndTime == nil || !video.RecordingEndTime.Equal(want.Add(time.Minute)) {
		t.Fatal("TOT/selected PCR timestamp mismatch")
	}
	if p.Video.RecordingStartTime == nil || !p.Video.RecordingStartTime.Equal(want) {
		t.Fatal("caller timestamps not propagated to result")
	}
}

func TestTSInfoSelectsServiceByTwentyPercentPIDFrequency(t *testing.T) {
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	services := []psi.PATProgram{{ServiceID: 101, PMTPID: 0x100}, {ServiceID: 102, PMTPID: 0x101}}
	cc := map[uint16]byte{}
	var data []byte
	for range 300 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{
			{0, tsinfoPAT(1, services...)}, {0x11, tsinfoSDT(1, 4, services...)},
			{0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)}, {0x101, tsinfoPMT(102, 0x300, 0x300, 0x301)},
			{0x12, tsinfoEIT(1, 4, 101, 0, 10, start, tsinfoShort("FIRST", "ONE"))},
			{0x12, tsinfoEIT(1, 4, 102, 0, 20, start, tsinfoShort("SECOND", "TWO"))},
			{0x300, nil}, {0x300, nil}, {0x301, nil},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
		}
	}
	path := filepath.Join(t.TempDir(), "multi.ts")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	video := &RecordedVideo{FilePath: path, FileSize: int64(len(data)), ContainerFormat: "MPEG-TS", Duration: 60}
	a := NewTSInfoProgramAnalyzer(nil, testLogger(t))
	for _, tc := range []struct {
		preferred *int
		want      string
	}{{nil, "SECOND"}, {ptr(101), "FIRST"}, {ptr(999), "SECOND"}} {
		p, ok := a.AnalyzeProgramContext(context.Background(), video, nil, tc.preferred)
		if !ok || p.Title != tc.want {
			t.Fatalf("service selection got %#v, want %s", p, tc.want)
		}
	}
}

func TestTSInfoAnalyzesSyntheticPATSDTEIT(t *testing.T) {
	video, _ := tsinfoFixture(t)
	analyzer := NewTSInfoProgramAnalyzer(nil, testLogger(t))
	var _ ProgramAnalyzer = analyzer
	var _ ContextProgramAnalyzer = analyzer
	p, ok := analyzer.AnalyzeProgramContext(context.Background(), video, nil, nil)
	if !ok || p == nil {
		t.Fatal("synthetic SDT/EIT must produce a program")
	}
	if p.Title != "PRESENT" || p.Description != "DESCRIPTION" || p.EventID == nil || *p.EventID != 10 {
		t.Fatalf("wrong program: %#v", p)
	}
	if p.Channel == nil || p.Channel.ID != "NID4-SID101" || p.Channel.DisplayChannelID != "bs101" || p.Channel.Name != "SYNTHETIC" || p.Channel.RemoconID != 1 {
		t.Fatalf("wrong channel: %#v", p.Channel)
	}
	if p.Duration != 60 || !p.EndTime.Equal(p.StartTime.Add(time.Minute)) || !p.IsFree {
		t.Fatal("schedule/free flag mismatch")
	}
	if p.PrimaryAudioType != DefaultPrimaryAudioType || p.PrimaryAudioLanguage != DefaultPrimaryAudioLanguage {
		t.Fatal("default audio mismatch")
	}
	if video.RecordingStartTime != nil || video.RecordingEndTime != nil {
		t.Fatal("missing TOT must not invent timestamps")
	}
	if _, ok := any(analyzer).(RecordingTimeAnalyzer); ok {
		t.Fatal("stateless analyzer must not expose shared last-result timestamps")
	}
	legacy, ok := analyzer.AnalyzeProgram(video, nil)
	if !ok || legacy.Title != p.Title {
		t.Fatal("legacy interface mismatch")
	}
}
