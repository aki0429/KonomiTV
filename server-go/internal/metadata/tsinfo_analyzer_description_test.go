package metadata

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// These helpers use only public, synthetic descriptors, never broadcast data.
func tsinfoDescriptionDescriptors(t *testing.T, raw []byte, language string) []psi.Descriptor {
	t.Helper()
	if language != "" {
		raw = append([]byte(nil), raw...)
		copy(raw[3:6], language)
	}
	descriptors, err := psi.DecodeDescriptors(raw)
	if err != nil {
		t.Fatal(err)
	}
	return descriptors
}

func tsinfoDescriptionMerge(t *testing.T, state *tsinfoEventState, raw []byte, language string) {
	t.Helper()
	state.merge(&RecordedVideo{FilePath: "synthetic.ts", Duration: 60},
		&Channel{NetworkID: 4, ServiceID: 101},
		psi.Event{EventID: 10, Descriptors: tsinfoDescriptionDescriptors(t, raw, language)})
}

func tsinfoDescriptionComplete(t *testing.T, short, language string) tsinfoEventState {
	t.Helper()
	state := tsinfoEventState{}
	tsinfoDescriptionMerge(t, &state, tsinfoShort("TITLE", short), "")
	tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}}), language)
	tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}}), language)
	tsinfoDescriptionWant(t, &state, map[bool]string{true: "AB", false: short}[short == ""], DetailEntry{Key: "HEAD", Value: "AB"})
	return state
}

func tsinfoDescriptionWant(t *testing.T, state *tsinfoEventState, description string, detail ...DetailEntry) {
	t.Helper()
	want := append([]DetailEntry{}, detail...)
	if state.program == nil || state.program.Description != description || !reflect.DeepEqual(state.program.Detail, want) {
		t.Fatalf("program=%+v; want Description=%q Detail=%v", state.program, description, want)
	}
}

// Minimal migration of TestFreshCycle1FragmentDescriptionInvalidation.
func TestTSInfoDescriptionDerivedFragmentInvalidation(t *testing.T) {
	for _, mode := range []string{"shorter", "longer_gap", "language_switch"} {
		t.Run(mode, func(t *testing.T) {
			language := map[string]string{"language_switch": "eng"}[mode]
			state := tsinfoDescriptionComplete(t, "", language)
			replacement := tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")})
			if mode == "longer_gap" {
				replacement = tsinfoExtended(0, 2, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")})
			}
			tsinfoDescriptionMerge(t, &state, replacement, "")
			if mode == "longer_gap" {
				tsinfoDescriptionWant(t, &state, "")
			} else {
				tsinfoDescriptionWant(t, &state, "X", DetailEntry{Key: "HEAD", Value: "X"})
			}
		})
	}
}

// Minimal migration of TestFreshCycle1DescriptionInvalidationEndToEnd.
func TestTSInfoDescriptionInvalidationEndToEnd(t *testing.T) {
	service := psi.PATProgram{ServiceID: 101, PMTPID: 0x100}
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, constants.JST)
	counters := map[uint16]byte{}
	var data []byte
	for range 80 {
		for _, table := range []struct {
			pid uint16
			raw []byte
		}{
			{0, tsinfoPAT(1, service)}, {0x11, tsinfoSDT(1, 4, service)}, {0x100, tsinfoPMT(101, 0x200, 0x200, 0x201)},
		} {
			data = append(data, tsinfoPackets(table.pid, table.raw, counters)...)
		}
	}
	initial := append(tsinfoShort("TITLE", ""), tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}})...)
	initial = append(initial, tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}})...)
	replacement := tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")})
	for _, descriptors := range [][]byte{initial, replacement} {
		section := tsinfoEIT(1, 4, 101, 0, 10, start, descriptors)
		if err := psi.ValidateSection(section); err != nil {
			t.Fatal("invalid synthetic EIT CRC", err)
		}
		data = append(data, tsinfoPackets(0x12, section, counters)...)
	}
	video := &RecordedVideo{FilePath: "synthetic.ts", FileSize: int64(len(data)), Duration: 60, ContainerFormat: "MPEG-TS"}
	program, ok := NewTSInfoProgramAnalyzer(nil, nil).analyzeTSInfo(context.Background(), bytes.NewReader(data), video, int64(len(data)), nil)
	if !ok || program == nil {
		t.Fatal("legal synthetic sections not analyzed")
	}
	if len(program.Detail) != 1 || program.Detail[0].Value != "X" || program.Description != "X" {
		t.Fatalf("CRC-valid same-event EIT AB -> 0/0=X without repeated short: Detail=%v Description=%q, want X/X", program.Detail, program.Description)
	}
}

func TestTSInfoDescriptionProvenance(t *testing.T) {
	t.Run("same_fragment_body_replacement", func(t *testing.T) {
		state := tsinfoDescriptionComplete(t, "", "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'X'}}), "")
		tsinfoDescriptionWant(t, &state, "XB", DetailEntry{Key: "HEAD", Value: "XB"})
	})
	t.Run("incomplete_japanese_language_switch", func(t *testing.T) {
		state := tsinfoDescriptionComplete(t, "", "eng")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 1, [2][]byte{nil, []byte{'Y'}}), "")
		tsinfoDescriptionWant(t, &state, "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'X'}}), "")
		tsinfoDescriptionWant(t, &state, "XY", DetailEntry{Key: "HEAD", Value: "XY"})
	})
	t.Run("complete_replacement_without_items", func(t *testing.T) {
		state := tsinfoDescriptionComplete(t, "", "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0), "")
		tsinfoDescriptionWant(t, &state, "")
	})
	for _, mode := range []string{"initial_equal_detail", "derived_then_equal_short", "derived_then_nonempty_short"} {
		t.Run(mode, func(t *testing.T) {
			initial, explicit := "", "AB"
			if mode == "initial_equal_detail" {
				initial = explicit
			}
			state := tsinfoDescriptionComplete(t, initial, "eng")
			if mode != "initial_equal_detail" {
				if mode == "derived_then_nonempty_short" {
					explicit = "EXPLICIT"
				}
				tsinfoDescriptionMerge(t, &state, tsinfoShort("UPDATED", explicit), "")
			}
			// Equality with the old detail is not evidence of derived provenance.
			tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")}), "")
			tsinfoDescriptionWant(t, &state, explicit, DetailEntry{Key: "HEAD", Value: "X"})
			tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 2, [2][]byte{tsinfoText("HEAD"), tsinfoText("Y")}), "")
			tsinfoDescriptionWant(t, &state, explicit)
		})
	}
	for _, short := range []string{"", " \t\r\n "} {
		name := "empty_short_reenables_derivation"
		if short != "" {
			name = "whitespace_short_reenables_derivation"
		}
		t.Run(name, func(t *testing.T) {
			state := tsinfoDescriptionComplete(t, "EXPLICIT", "")
			tsinfoDescriptionMerge(t, &state, tsinfoShort("UPDATED", short), "")
			tsinfoDescriptionWant(t, &state, "AB", DetailEntry{Key: "HEAD", Value: "AB"})
			tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")}), "")
			tsinfoDescriptionWant(t, &state, "X", DetailEntry{Key: "HEAD", Value: "X"})
		})
	}
	t.Run("absent_short_keeps_default_until_empty_short_arrives", func(t *testing.T) {
		state := tsinfoEventState{}
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("A")}), "eng")
		tsinfoDescriptionWant(t, &state, DefaultDescription, DetailEntry{Key: "HEAD", Value: "A"})
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("X")}), "")
		tsinfoDescriptionWant(t, &state, DefaultDescription, DetailEntry{Key: "HEAD", Value: "X"})
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 1, [2][]byte{tsinfoText("HEAD"), tsinfoText("Y")}), "")
		tsinfoDescriptionWant(t, &state, DefaultDescription)
		tsinfoDescriptionMerge(t, &state, tsinfoShort("TITLE", ""), "")
		tsinfoDescriptionWant(t, &state, "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 1, [2][]byte{nil, []byte{'Z'}}), "")
		tsinfoDescriptionWant(t, &state, "YZ", DetailEntry{Key: "HEAD", Value: "YZ"})
	})
	t.Run("retransmission_and_ignored_inputs_preserve_program", func(t *testing.T) {
		state := tsinfoDescriptionComplete(t, "", "")
		before := *state.program
		before.Detail = append([]DetailEntry(nil), state.program.Detail...)
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 1, [2][]byte{nil, []byte{'B'}}), "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("FOREIGN")}), "eng")
		tsinfoDescriptionMerge(t, &state, tsinfoDescriptor(0x4d, []byte{'j', 'p', 'n', 0, 9}), "")
		tsinfoDescriptionMerge(t, &state, tsinfoDescriptor(0x4e, []byte{0x10, 'j', 'p', 'n', 0, 0}), "")
		foreign := append(tsinfoShort("WRONG", "WRONG"), tsinfoExtended(0, 0, [2][]byte{tsinfoText("HEAD"), tsinfoText("WRONG")})...)
		state.merge(&RecordedVideo{Duration: 100}, &Channel{NetworkID: 4, ServiceID: 101},
			psi.Event{EventID: 11, FreeCA: true, Descriptors: tsinfoDescriptionDescriptors(t, foreign, "")})
		if !reflect.DeepEqual(*state.program, before) {
			t.Fatalf("duplicate, malformed or foreign input changed program: before=%+v after=%+v", before, *state.program)
		}
	})
	t.Run("out_of_order_headings_gap_and_body_update", func(t *testing.T) {
		state := tsinfoEventState{}
		tsinfoDescriptionMerge(t, &state, tsinfoShort("TITLE", ""), "")
		last := tsinfoExtended(2, 2, [2][]byte{nil, []byte{'C'}},
			[2][]byte{tsinfoText("HEAD"), tsinfoText("SECOND")}, [2][]byte{tsinfoText("HEAD"), tsinfoText("THIRD")})
		tsinfoDescriptionMerge(t, &state, last, "")
		tsinfoDescriptionWant(t, &state, "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(0, 2, [2][]byte{tsinfoText("HEAD"), []byte{0x0e, 'A'}}), "")
		tsinfoDescriptionWant(t, &state, "")
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 2, [2][]byte{nil, []byte{'B'}}), "")
		want := []DetailEntry{{Key: "HEAD", Value: "ABC"}, {Key: "HEAD\t", Value: "SECOND"}, {Key: "HEAD\t\t", Value: "THIRD"}}
		tsinfoDescriptionWant(t, &state, "ABC", want...)
		tsinfoDescriptionMerge(t, &state, last, "")
		tsinfoDescriptionWant(t, &state, "ABC", want...)
		tsinfoDescriptionMerge(t, &state, tsinfoExtended(1, 2, [2][]byte{nil, []byte{'D'}}), "")
		want[0].Value = "ADC"
		tsinfoDescriptionWant(t, &state, "ADC", want...)
	})
}
