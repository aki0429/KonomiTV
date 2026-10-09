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

// Python 版は負の経過時間を 0 にクランプするため、PCR の巻き戻しは過去時計を
// 生み出さず、録画開始は TOT 時点そのものになる。
func TestIndependentReviewPCRBackwardCannotInventOldClock(t *testing.T) {
	clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
	data := tsinfoPCRPacket(0x200, 90000, 0)
	data = append(data, tsinfoPCRPacket(0x200, 0, 0)...)
	data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
	start, finish := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, 60)
	if start == nil || finish == nil {
		t.Fatal("clamped backward PCR must still yield a recording clock")
	}
	if !start.Equal(clock) || !finish.Equal(clock.Add(time.Minute)) {
		t.Fatalf("3 packets (%d bytes), PCR base 90000 -> 0: backward PCR invented an old clock: start=%s want=%s", len(data), start.Format(time.RFC3339Nano), clock.Format(time.RFC3339Nano))
	}
}

// Python 版は PCR base (90kHz) のみを使い、timedelta(seconds=...) と同じく
// マイクロ秒へ四捨五入 (banker's rounding) する。9bit extension は使わない。
func TestTSInfoReviewPCRMicrosecondRounding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseFirst  uint64
		baseLast   uint64
		extFirst   uint16
		extLast    uint16
		wantMicros int64
	}{
		// extension を変えても base 差が 0 なら経過時間は 0 (extension 無視)。
		{"extension_ignored", 100, 100, 0, 299, 0},
		{"one_base_tick", 0, 1, 0, 0, 11},        // 1/90000 s = 11.11 µs
		{"nine_base_ticks", 0, 9, 0, 0, 100},     // ちょうど 100 µs
		{"fraction_rounds_up", 0, 5, 0, 0, 56},   // 55.56 µs
		{"fraction_rounds_down", 0, 4, 0, 0, 44}, // 44.44 µs
		// 実測値に依らない合成ケース: base 差がマイクロ秒の端数を持つ代表的な大きさ。
		// 期待値は Python の timedelta(seconds=...) と同じ banker's rounding。
		{"fractional_62659_ticks", 1000000, 1062659, 29, 245, 696211},   // 696211.111 µs
		{"fractional_109307_ticks", 1000000, 1109307, 295, 72, 1214522}, // 1214522.222 µs
		{"fractional_280805_ticks", 1000000, 1280805, 102, 89, 3120056}, // 3120055.556 µs
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
			data := tsinfoPCRPacket(0x200, tc.baseFirst, tc.extFirst)
			if tc.baseLast != tc.baseFirst || tc.extLast != tc.extFirst {
				data = append(data, tsinfoPCRPacket(0x200, tc.baseLast, tc.extLast)...)
			}
			data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
			duration := 60.0
			start, end := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, duration)
			if start == nil || end == nil {
				t.Fatal("valid synthetic PCR clock unavailable")
			}
			want := clock.Add(-time.Duration(tc.wantMicros) * time.Microsecond)
			if !start.Equal(want) {
				t.Fatalf("base %d->%d: elapsed=%d µs want=%d µs; got start=%s want=%s", tc.baseFirst, tc.baseLast, clock.Sub(*start)/time.Microsecond, tc.wantMicros, start.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
			if end.Sub(*start) != time.Duration(math.RoundToEven(duration*1e6))*time.Microsecond {
				t.Fatalf("duration must round to microseconds: got %s", end.Sub(*start))
			}
		})
	}
}

// Python 版は「TOT 時点の経過時間が録画長を超えたら不採用」というガードを持たない。
// base 差が duration を超えても TOT を基準に 1 つの時計を返し、負の差だけを 0 に
// クランプする (Go 従来実装の duration 上限ガードは Python 非互換だった)。
func TestTSInfoReviewPCRDurationDoesNotBoundClock(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseFirst  uint64
		baseLast   uint64
		duration   float64
		wantMicros int64
	}{
		{"elapsed_equal_duration", 0, 90000, 1, 1000000},
		{"elapsed_one_base_above", 0, 90001, 1, 1000011},
		{"elapsed_far_above_duration", 0, 90000 * 90, 60, 90000000},
		{"sub_microsecond_above", 0, 1, 37e-9, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Date(2026, 10, 7, 10, 0, 10, 0, constants.JST)
			data := tsinfoPCRPacket(0x200, tc.baseFirst, 0)
			data = append(data, tsinfoPCRPacket(0x200, tc.baseLast, 0)...)
			data = append(data, tsinfoPackets(0x14, tsinfoTOT(clock), map[uint16]byte{})...)
			start, end := tsinfoRecordingTime(context.Background(), bytes.NewReader(data), int64(len(data)), 0x200, tc.duration)
			if start == nil || end == nil {
				t.Fatalf("duration must not gate the clock (base delta %d)", tc.baseLast-tc.baseFirst)
			}
			want := clock.Add(-time.Duration(tc.wantMicros) * time.Microsecond)
			if !start.Equal(want) {
				t.Fatalf("start=%s want=%s", start.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
		})
	}
}
