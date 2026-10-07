package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// EIT failure must not leak a newly inferred clock to filename fallback.
func TestIndependentReviewFailedProgramDoesNotPublishClock(t *testing.T) {
	clock := time.Date(2020, 1, 2, 3, 4, 5, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	cc := map[uint16]byte{}
	data := tsinfoPCRPacket(0x200, 90000, 0)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), cc)...)
	data = append(data, tsinfoPackets(0, tsinfoPAT(1, s), cc)...)
	data = append(data, tsinfoPackets(0x11, tsinfoSDT(1, 4, s), cc)...)
	data = append(data, tsinfoPackets(0x100, tsinfoPMT(101, 0x200, 0x200, 0x201), cc)...)
	video := &RecordedVideo{FilePath: "synthetic.ts", FileSize: int64(len(data)), Duration: 60, ContainerFormat: "MPEG-TS"}
	p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), bytes.NewReader(data), video, int64(len(data)), nil)
	if p != nil || ok {
		t.Fatal("fixture unexpectedly contained a valid EIT")
	}
	if video.RecordingStartTime != nil || video.RecordingEndTime != nil {
		t.Fatalf("5 packets (%d bytes), no EIT: got (nil,false), but leaked clock %s into caller-owned video", len(data), video.RecordingStartTime.Format(time.RFC3339Nano))
	}
}

type independentTSProbeRunner struct{ recordingTimeProbeRunner }

func (r independentTSProbeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error) {
	result, err := r.recordingTimeProbeRunner.Run(ctx, name, args, stdin)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(result.Stdout, &data); err != nil {
		return nil, err
	}
	data["format"] = map[string]any{"format_name": "mpegts", "duration": "60"}
	result.Stdout, err = json.Marshal(data)
	return result, err
}
func TestIndependentReviewFilenameFallbackDoesNotAdoptOldTOT(t *testing.T) {
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	cc := map[uint16]byte{}
	oldClock := time.Date(2020, 1, 2, 3, 4, 5, 0, constants.JST)
	data := tsinfoPCRPacket(0x200, 90000, 0)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(oldClock), cc)...)
	// Repeat just PAT/SDT/PMT so both bounded windows contain service data,
	// while no EIT is present. No broadcast or user data is involved.
	for len(data) < 4<<20 {
		data = append(data, tsinfoPackets(0, tsinfoPAT(1, s), cc)...)
		data = append(data, tsinfoPackets(0x11, tsinfoSDT(1, 4, s), cc)...)
		data = append(data, tsinfoPackets(0x100, tsinfoPMT(101, 0x200, 0x200, 0x201), cc)...)
	}
	path := filepath.Join(t.TempDir(), "synthetic-fallback.ts")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2026, 10, 7, 11, 0, 0, 0, constants.JST)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	analyzer := &Analyzer{Runner: independentTSProbeRunner{}, ProgramAnalyzer: NewTSInfoProgramAnalyzer(nil, nil), Logger: testLogger(t)}
	p, err := analyzer.Analyze(context.Background(), path)
	if err != nil || p == nil {
		t.Fatalf("fixture failed before fallback: %v", err)
	}
	want := modified.Add(-time.Minute)
	if !p.StartTime.Equal(want) || p.Video.RecordingStartTime != nil {
		t.Fatalf("no EIT, filename fallback: expected mtime-duration=%s with unknown recording clock; got start=%s, recording_start=%v", want.Format(time.RFC3339Nano), p.StartTime.Format(time.RFC3339Nano), p.Video.RecordingStartTime)
	}
}

// Keep a previously known caller clock when a failed call produced a new TOT.
func TestIndependentReviewFailedCallPreservesExistingClock(t *testing.T) {
	old := time.Date(2026, 10, 7, 9, 0, 0, 0, constants.JST)
	oldEnd := old.Add(time.Minute)
	observed := time.Date(2020, 1, 2, 3, 4, 5, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	cc := map[uint16]byte{}
	data := tsinfoPCRPacket(0x200, 90000, 0)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(observed), cc)...)
	data = append(data, tsinfoPackets(0, tsinfoPAT(1, s), cc)...)
	data = append(data, tsinfoPackets(0x11, tsinfoSDT(1, 4, s), cc)...)
	data = append(data, tsinfoPackets(0x100, tsinfoPMT(101, 0x200, 0x200, 0x201), cc)...)
	video := &RecordedVideo{FilePath: "synthetic.ts", Duration: 60, ContainerFormat: "MPEG-TS", FileSize: int64(len(data)), RecordingStartTime: &old, RecordingEndTime: &oldEnd}
	p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), bytes.NewReader(data), video, int64(len(data)), nil)
	if p != nil || ok {
		t.Fatal("unexpected valid program")
	}
	if !video.RecordingStartTime.Equal(old) || !video.RecordingEndTime.Equal(oldEnd) {
		t.Fatalf("failed call changed caller clock: want=%s got=%s", old.Format(time.RFC3339Nano), video.RecordingStartTime.Format(time.RFC3339Nano))
	}
}

type tsinfoReviewScheduleReader struct {
	*tsinfoSparseReader
	failAt  int64
	failure error
	cancel  context.CancelFunc
}

func (r *tsinfoReviewScheduleReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart && offset == r.failAt {
		if r.cancel != nil {
			r.cancel()
		}
		return 0, r.failure
	}
	return r.tsinfoSparseReader.Seek(offset, whence)
}

func TestTSInfoReviewFailedTimingKeepsCallerClock(t *testing.T) {
	for _, mode := range []string{"invalid_start", "zero_duration", "schedule_read_error", "schedule_cancel"} {
		for _, known := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/unknown", true: "/known"}[known], func(t *testing.T) {
				video, data := tsinfoFixture(t)
				for i := 0; i+psi.PacketSize <= len(data); i += psi.PacketSize {
					if uint16(data[i+1]&31)<<8|uint16(data[i+2]) != 0x12 {
						continue
					}
					s := data[i+5:]
					s = s[:3+(int(s[1]&15)<<8|int(s[2]))]
					switch mode {
					case "invalid_start":
						for j := 16; j < 21; j++ {
							s[j] = 0xff
						}
					case "zero_duration":
						s[21], s[22], s[23] = 0, 0, 0
					default:
						s[21], s[22], s[23] = 0xff, 0xff, 0xff
					}
					binary.BigEndian.PutUint32(s[len(s)-4:], psi.CRC32MPEG2(s[:len(s)-4]))
				}
				clock := time.Date(2020, 1, 2, 3, 4, 5, 0, constants.JST)
				prefix := tsinfoPCRPacket(0x200, 90000, 0)
				prefix = append(prefix, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
				beginning := append(prefix, data...)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var r io.ReadSeeker = bytes.NewReader(beginning)
				end := int64(len(beginning))
				if mode == "schedule_read_error" || mode == "schedule_cancel" {
					end = 1 << 30
					offset := closestMultiple(end/5, psi.PacketSize)
					next := offset + tsinfoWindowBytes + 188*1000000
					reader := &tsinfoReviewScheduleReader{
						tsinfoSparseReader: &tsinfoSparseReader{blocks: []tsinfoReadWindow{{offset: 0, data: beginning}, {offset: offset, data: data}}},
						failAt:             next, failure: errors.New("synthetic schedule read error"),
					}
					if mode == "schedule_cancel" {
						reader.cancel = cancel
						reader.failure = context.Canceled
					}
					r = reader
				}
				video.FileSize = end
				old := time.Date(2026, 10, 7, 9, 0, 0, 123, constants.JST)
				oldEnd := old.Add(time.Minute)
				if known {
					video.RecordingStartTime, video.RecordingEndTime = &old, &oldEnd
				}
				before := *video
				p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(ctx, r, video, end, nil)
				if p != nil || ok {
					t.Fatal("invalid/cancelled synthetic schedule must fall back")
				}
				if video.RecordingStartTime != before.RecordingStartTime || video.RecordingEndTime != before.RecordingEndTime {
					t.Fatal("failed analysis replaced caller timestamp pointers")
				}
				if known && (!video.RecordingStartTime.Equal(old) || !video.RecordingEndTime.Equal(oldEnd)) {
					t.Fatal("failed analysis changed existing timestamp values")
				}
			})
		}
	}
}

// Missing middle descriptor is a gap, not adjacent ARIB bytes.
func TestIndependentReviewFragmentGapCannotJoinNonAdjacentBytes(t *testing.T) {
	video := &RecordedVideo{FilePath: "synthetic.ts", Duration: 60}
	state := tsinfoEventState{}
	c := &Channel{NetworkID: 4, ServiceID: 101}
	first, _ := psi.DecodeDescriptors(tsinfoExtended(0, 2, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}}))
	last, _ := psi.DecodeDescriptors(tsinfoExtended(2, 2, [2][]byte{nil, []byte{'C'}}))
	state.merge(video, c, psi.Event{EventID: 10, Descriptors: first})
	state.merge(video, c, psi.Event{EventID: 10, Descriptors: last})
	if len(state.program.Detail) > 0 && state.program.Detail[0].Value == "AC" {
		t.Fatalf("fragments 0/2='A', missing 1/2, 2/2 continuation='C': expected incomplete detail, got fabricated contiguous text %q", state.program.Detail[0].Value)
	}
}

// descriptor_number belongs to a fragment set ending at last_descriptor_number.
// A shorter replacement for the same event must not inherit old trailing bytes.
func TestIndependentReviewFragmentCountChangeDropsOldTail(t *testing.T) {
	video := &RecordedVideo{FilePath: "synthetic.ts", Duration: 60}
	state := tsinfoEventState{}
	c := &Channel{NetworkID: 4, ServiceID: 101}
	first, _ := psi.DecodeDescriptors(tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}}))
	last, _ := psi.DecodeDescriptors(tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}}))
	replacement, _ := psi.DecodeDescriptors(tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'X'}}))
	state.merge(video, c, psi.Event{EventID: 10, Descriptors: append(first, last...)})
	state.merge(video, c, psi.Event{EventID: 10, Descriptors: replacement})
	if len(state.program.Detail) != 1 || state.program.Detail[0].Value != "X" {
		t.Fatalf("same event fragment set 0/1='A'+1/1='B' -> replacement 0/0='X': expected X only, got %v", state.program.Detail)
	}
}

// A CRC-valid PAT on NIT PID is not a PAT. Accepting it defeats the
// stream identity check when no canonical PID 0 PAT exists at all.
func TestIndependentReviewMustNotAdoptPATFromNITPID(t *testing.T) {
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	s := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	cc := map[uint16]byte{}
	data := tsinfoPackets(0x10, tsinfoPAT(1, s), cc)
	data = append(data, tsinfoPackets(0x11, tsinfoSDT(1, 4, s), cc)...)
	data = append(data, tsinfoPackets(0x100, tsinfoPMT(101, 0x200, 0x200, 0x201), cc)...)
	data = append(data, tsinfoPackets(0x12, tsinfoEIT(1, 4, 101, 0, 10, start, tsinfoShort("PRESENT", "DESCRIPTION")), cc)...)
	video := &RecordedVideo{FilePath: "synthetic.ts", Duration: 60, ContainerFormat: "MPEG-TS", FileSize: int64(len(data))}
	p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), bytes.NewReader(data), video, int64(len(data)), nil)
	if ok || p != nil {
		t.Fatalf("4 packets (%d bytes), no PID 0 PAT, PAT carried on PID 0x10: expected fallback, got ok=%v sid=%d title=%q", len(data), ok, p.Channel.ServiceID, p.Title)
	}
}

func TestTSInfoReviewCanonicalTablePID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table byte
		pid   uint16
	}{
		{"PAT_on_SDT", 0, 0x11}, {"PAT_on_NIT", 0, 0x10},
		{"SDT_on_PAT", 0x42, 0}, {"SDT_on_NIT", 0x42, 0x10},
		{"NIT_on_PAT", 0x40, 0}, {"NIT_on_SDT", 0x40, 0x11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
			service := psi.PATProgram{ServiceID: 1024, PMTPID: 0x100}
			cc := map[uint16]byte{}
			var data []byte
			for range 20 {
				for _, table := range []struct {
					pid uint16
					raw []byte
				}{
					{0, tsinfoPAT(1, service)}, {0x11, tsinfoSDT(1, 0x7fe0, service)},
					{0x10, tsinfoNIT(1, 0x7fe0, 9)}, {0x100, tsinfoPMT(1024, 0x200, 0x200, 0x201)},
					{0x12, tsinfoEIT(1, 0x7fe0, 1024, 0, 10, start, tsinfoShort("SYNTHETIC", "DESCRIPTION"))},
				} {
					if err := psi.ValidateSection(table.raw); err != nil {
						t.Fatal("test section CRC invalid", err)
					}
					if table.raw[0] == tc.table {
						table.pid = tc.pid
					}
					data = append(data, tsinfoPackets(table.pid, table.raw, cc)...)
				}
			}
			video := &RecordedVideo{FilePath: "synthetic.ts", FileSize: int64(len(data)), Duration: 60, ContainerFormat: "MPEG-TS"}
			p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), bytes.NewReader(data), video, int64(len(data)), nil)
			if tc.table != 0x40 {
				if ok || p != nil {
					t.Fatal("CRC-valid table on wrong PID supplied canonical identity")
				}
			} else if !ok || p == nil || p.Channel.RemoconID != 0 {
				t.Fatal("wrong-PID NIT must not supply remote control key")
			}
		})
	}
}

type independentJoinedEOFReader struct{ *bytes.Reader }

var independentDiskFailure = errors.New("synthetic disk failure")

func (r independentJoinedEOFReader) Read(b []byte) (int, error) {
	n, _ := r.Reader.Read(b)
	return n, errors.Join(io.EOF, independentDiskFailure)
}
func TestIndependentReviewWindowMustPreserveJoinedReadFailure(t *testing.T) {
	data := []byte{1, 2, 3}
	_, err := tsinfoLoadWindow(context.Background(), independentJoinedEOFReader{bytes.NewReader(data)}, 0, 3)
	if !errors.Is(err, independentDiskFailure) {
		t.Fatalf("Read returned 3 bytes + Join(EOF,disk failure): expected disk failure propagated, got %v", err)
	}
}

// PMT scan succeeds; the following PID-frequency read must not hide a
// separately reported storage failure merely because EOF is another leaf.
type tsinfoReviewReadError struct {
	*bytes.Reader
	err            error
	errorAfterSeek int
	seeks          int
}

func (r *tsinfoReviewReadError) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	return r.Reader.Seek(offset, whence)
}
func (r *tsinfoReviewReadError) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if r.seeks >= r.errorAfterSeek {
		return n, r.err
	}
	return n, err
}

func TestTSInfoReviewEOFErrorTrees(t *testing.T) {
	disk := errors.New("synthetic disk failure")
	for _, tc := range []struct {
		name    string
		err     error
		failure bool
	}{
		{"EOF", io.EOF, false}, {"wrapped_EOF", fmt.Errorf("wrapped: %w", io.EOF), false},
		{"joined_EOF_only", errors.Join(io.EOF, fmt.Errorf("wrapped: %w", io.EOF)), false},
		{"disk", disk, true}, {"join", errors.Join(io.EOF, disk), true},
		{"wrapped_join", fmt.Errorf("wrapped: %w", errors.Join(io.EOF, disk)), true},
		{"nested_join", errors.Join(fmt.Errorf("wrapped: %w", io.EOF), errors.Join(io.EOF, disk)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tsinfoLoadWindow(context.Background(), &tsinfoReviewReadError{Reader: bytes.NewReader([]byte{1, 2, 3}), err: tc.err, errorAfterSeek: 1}, 0, 3)
			if tc.failure {
				if !errors.Is(err, disk) {
					t.Errorf("window lost non-EOF leaf: %v", err)
				}
			} else if err != nil {
				t.Errorf("EOF-only should end normally: %v", err)
			}
			services := []psi.PATProgram{{ServiceID: 101, PMTPID: 0x100}, {ServiceID: 102, PMTPID: 0x101}}
			pat := &psi.PAT{Programs: services}
			cc := map[uint16]byte{}
			var data []byte
			for range 150 {
				data = append(data, tsinfoPackets(0x100, tsinfoPMT(101, 0x200, 0x200, 0x201), cc)...)
				data = append(data, tsinfoPackets(0x101, tsinfoPMT(102, 0x300, 0x300, 0x301), cc)...)
				data = append(data, tsinfoPackets(0x300, nil, cc)...)
			}
			r := &tsinfoReviewReadError{Reader: bytes.NewReader(data), err: tc.err, errorAfterSeek: 2}
			selected, err := tsinfoSelectService(context.Background(), r, int64(len(data)), pat)
			if tc.failure {
				if !errors.Is(err, disk) {
					t.Fatalf("PID-frequency read lost non-EOF leaf: %v", err)
				}
			} else if err != nil || selected == nil || *selected != 102 {
				t.Fatalf("EOF-only frequency read unavailable: %v %v", selected, err)
			}
		})
	}
}

func TestTSInfoReviewJoinedReadFailureKeepsCallerClock(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			video, data := tsinfoFixture(t)
			clock := time.Date(2020, 1, 2, 3, 4, 5, 0, constants.JST)
			prefix := tsinfoPCRPacket(0x200, 90000, 0)
			prefix = append(prefix, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
			data = append(prefix, data...)
			video.FileSize = int64(len(data))
			old := time.Date(2026, 10, 7, 9, 0, 0, 123, constants.JST)
			oldEnd := old.Add(time.Minute)
			if known {
				video.RecordingStartTime, video.RecordingEndTime = &old, &oldEnd
			}
			before := *video
			p, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), independentJoinedEOFReader{bytes.NewReader(data)}, video, int64(len(data)), nil)
			if p != nil || ok {
				t.Fatal("joined storage failure must fall back")
			}
			if video.RecordingStartTime != before.RecordingStartTime || video.RecordingEndTime != before.RecordingEndTime {
				t.Fatal("read failure changed caller timestamp pointers")
			}
		})
	}
}

// Synthetic regression: a backwards PCR without a wrap near the modulus
// cannot place a 60-second recording more than a day before its valid TOT.
func TestIndependentReviewPCRBackwardCannotInventOldClock(t *testing.T) {
	clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
	data := tsinfoPCRPacket(0x200, 90000, 0)
	data = append(data, tsinfoPCRPacket(0x200, 0, 0)...)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
	start, finish := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, 60)
	if start != nil || finish != nil {
		t.Fatalf("3 packets (%d bytes), PCR base 90000 -> 0: expected unavailable clock; got start=%s, elapsed=%s, duration=60s", len(data), start.Format(time.RFC3339Nano), clock.Sub(*start))
	}
}

// Expected values are literal integer-rational nanosecond floors, not Python
// or rounded float clocks. The large counterexample overflows ticks * 1e9.
func TestTSInfoReviewPCRNanosecondFloor(t *testing.T) {
	for _, tc := range []struct {
		ticks uint64
		ns    int64
	}{
		{0, 0}, {1, 37}, {26999999, 999999962}, {27000000, 1000000000},
		{27000001, 1000000037}, {905670694133, 33543359041962},
		{905670694134, 33543359042000}, {905670694135, 33543359042037},
		{psi.PCRModulus - 1, 95443717688851},
	} {
		t.Run(fmt.Sprint(tc.ticks), func(t *testing.T) {
			clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
			data := tsinfoPCRPacket(0x200, 0, 0)
			data = append(data, tsinfoPCRPacket(0x200, tc.ticks/300, uint16(tc.ticks%300))...)
			data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
			duration := float64(tc.ticks/27000000 + 1)
			start, end := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, duration)
			if start == nil || end == nil {
				t.Fatal("valid synthetic PCR clock unavailable")
			}
			if got := clock.Sub(*start).Nanoseconds(); got != tc.ns {
				t.Fatalf("%d ticks: nanosecond floor=%d want=%d", tc.ticks, got, tc.ns)
			}
			if end.Sub(*start) != time.Duration(duration*float64(time.Second)) {
				t.Fatal("float duration end-addition changed")
			}
		})
	}
}

// No extra slack: even an elapsed PCR fraction hidden by nanosecond flooring
// must fit the caller's actual floating-point duration in seconds.
func TestTSInfoReviewPCRDurationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ticks     uint64
		duration  float64
		available bool
	}{
		{"equal", 27000000, 1, true},
		{"one_tick_below", 26999999, 1, true},
		{"one_tick_above", 27000001, 1, false},
		{"fraction_equal", 40500000, 1.5, true},
		{"fraction_one_tick_above", 40500001, 1.5, false},
		{"fraction_one_float_above", 27000001, math.Nextafter(1+1.0/27000000, math.Inf(1)), true},
		{"fraction_rounded_below", 27000001, 1 + 1.0/27000000, false},
		{"fraction_next_float_below", 27000001, math.Nextafter(1+1.0/27000000, 0), false},
		{"sub_nanosecond_over", 1, 37e-9, false},
		{"near_modulus_wrap", 600, 60, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := uint64(0)
			if tc.name == "near_modulus_wrap" {
				first = psi.PCRModulus - 300
			}
			last := (first + tc.ticks) % psi.PCRModulus
			clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
			data := tsinfoPCRPacket(0x200, first/300, uint16(first%300))
			data = append(data, tsinfoPCRPacket(0x200, last/300, uint16(last%300))...)
			data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
			start, end := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, tc.duration)
			if (start != nil && end != nil) != tc.available {
				t.Fatalf("elapsed=%d ticks duration=%0.18g: available=%v want=%v", tc.ticks, tc.duration, start != nil, tc.available)
			}
		})
	}
}
