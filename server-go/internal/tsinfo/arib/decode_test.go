package arib

import "testing"

func TestDecodeInitialHiragana(t *testing.T) {
	if got := Decode([]byte{0xa2, 0xa4, 0xa6}); got != "あいう" {
		t.Fatalf("Decode(initial GR=G2) = %q, want %q", got, "あいう")
	}
}

func TestDecodeHiraganaPunctuation(t *testing.T) {
	if got := Decode([]byte{0xf7, 0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe}); got != "ゝゞー。「」、・" {
		t.Fatalf("Decode(hiragana punctuation) = %q, want %q", got, "ゝゞー。「」、・")
	}
}

func TestDecodeLS1FullwidthAlphanumeric(t *testing.T) {
	if got := Decode([]byte{0x0e, 0x41, 0x42, 0x43, 0x31, 0x32, 0x33, 0x5c, 0x7e}); got != "ＡＢＣ１２３￥￣" {
		t.Fatalf("Decode(LS1) = %q, want %q", got, "ＡＢＣ１２３￥￣")
	}
}

func TestDecodeInitialKanji(t *testing.T) {
	if got := Decode([]byte{0x46, 0x7c, 0x4b, 0x5c, 0x38, 0x6c, 0xa2}); got != "日本語あ" {
		t.Fatalf("Decode(initial G0) = %q, want %q", got, "日本語あ")
	}
}

func TestDecodeLS0RestoresKanji(t *testing.T) {
	if got := Decode([]byte{0x0e, 0x41, 0x0f, 0x46, 0x7c}); got != "Ａ日" {
		t.Fatalf("Decode(LS0) = %q, want %q", got, "Ａ日")
	}
}

func TestDecodeSingleShifts(t *testing.T) {
	if got := Decode([]byte{0x19, 0x22, 0x1d, 0x22, 0x46, 0x7c}); got != "あア日" {
		t.Fatalf("Decode(SS2/SS3 one graphic) = %q, want %q", got, "あア日")
	}
}

func TestDecodeEscapeLockingShifts(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"LS2", []byte{0x1b, 0x6e, 0x22, 0x24}, "あい"},
		{"LS3", []byte{0x1b, 0x6f, 0x22, 0x24}, "アイ"},
		{"LS1R", []byte{0x1b, 0x7e, 0xc1, 0xc2}, "ＡＢ"},
		{"LS2R", []byte{0x1b, 0x7c, 0xa2, 0x1b, 0x7d, 0xa2}, "アあ"},
		{"LS3R", []byte{0x1b, 0x7c, 0xa2, 0xa4}, "アイ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeEscapeSingleByteDesignation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"G0_hiragana", []byte{0x1b, 0x28, 0x30, 0x22, 0x24}, "あい"},
		{"G1_katakana", []byte{0x1b, 0x29, 0x31, 0x0e, 0x22, 0x24}, "アイ"},
		{"G2_alphanumeric", []byte{0x1b, 0x2a, 0x4a, 0xc1, 0xc2}, "ＡＢ"},
		{"G3_hiragana", []byte{0x1b, 0x2b, 0x30, 0x1d, 0x22, 0x46, 0x7c}, "あ日"},
		{"proportional_alpha", []byte{0x1b, 0x28, 0x36, 0x41}, "Ａ"},
		{"proportional_hiragana", []byte{0x1b, 0x28, 0x37, 0x22}, "あ"},
		{"proportional_katakana", []byte{0x1b, 0x28, 0x38, 0x22}, "ア"},
		{"JIS_X0201_katakana", []byte{0x1b, 0x28, 0x49, 0x21, 0x26, 0x36, 0x5e, 0x5f}, "。ヲカ゛゜"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeEscapeDoubleByteDesignation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"G0_short", []byte{0x0e, 0x41, 0x1b, 0x24, 0x42, 0x0f, 0x46, 0x7c}, "Ａ日"},
		{"G0_explicit", []byte{0x1b, 0x24, 0x28, 0x42, 0x46, 0x7c}, "日"},
		{"G1", []byte{0x1b, 0x24, 0x29, 0x42, 0x0e, 0x46, 0x7c}, "日"},
		{"G2_right", []byte{0x1b, 0x24, 0x2a, 0x42, 0xc6, 0xfc}, "日"},
		{"G3_single", []byte{0x1b, 0x24, 0x2b, 0x42, 0x1d, 0x46, 0x7c, 0x4b, 0x5c}, "日本"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeProgramGaiji(t *testing.T) {
	data := []byte{0x7a, 0x56, 0x7a, 0x5c, 0x7a, 0x5b, 0x7a, 0x6b, 0x7a, 0x6d}
	if got := Decode(data); got != "[字][解][多][新][終]" {
		t.Fatalf("Decode(program marks) = %q, want %q", got, "[字][解][多][新][終]")
	}
}

func TestDecodeAllProgramGaiji(t *testing.T) {
	cases := []struct {
		cell byte
		want string
	}{
		{0x50, "[HV]"}, {0x51, "[SD]"}, {0x52, "[Ｐ]"}, {0x53, "[Ｗ]"}, {0x54, "[MV]"}, {0x55, "[手]"},
		{0x56, "[字]"}, {0x57, "[双]"}, {0x58, "[デ]"}, {0x59, "[Ｓ]"}, {0x5a, "[二]"}, {0x5b, "[多]"},
		{0x5c, "[解]"}, {0x5d, "[SS]"}, {0x5e, "[Ｂ]"}, {0x5f, "[Ｎ]"}, {0x62, "[天]"}, {0x63, "[交]"},
		{0x64, "[映]"}, {0x65, "[無]"}, {0x66, "[料]"}, {0x68, "[前]"}, {0x69, "[後]"}, {0x6a, "[再]"},
		{0x6b, "[新]"}, {0x6c, "[初]"}, {0x6d, "[終]"}, {0x6e, "[生]"}, {0x6f, "[販]"}, {0x70, "[声]"},
		{0x71, "[吹]"}, {0x72, "[PPV]"},
	}
	for _, tc := range cases {
		if got := Decode([]byte{0x7a, tc.cell}); got != tc.want {
			t.Errorf("program 7a%02x = %q, want %q", tc.cell, got, tc.want)
		}
	}
}

func TestDecodeAdditionalSymbols(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"extension_kanji", []byte{0x75, 0x21, 0x75, 0x22, 0x75, 0x2f, 0x76, 0x47}, "㐂𠅘𠮷髙"},
		{"arrows_units_weather_numbers", []byte{0x7c, 0x21, 0x7c, 0x2b, 0x7d, 0x60, 0x7e, 0x61}, "➡㎡☀①"},
		{"additional_designation", []byte{0x1b, 0x24, 0x3b, 0x7a, 0x56, 0x7c, 0x21}, "[字]➡"},
		{"unassigned", []byte{0x77, 0x21, 0x7a, 0x27}, "��"},
		{"private_glyph_no_font", []byte{0x7c, 0x58}, "�"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeUndefinedDRCS(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"DRCS1_G0", []byte{0x1b, 0x28, 0x20, 0x41, 0x21, 0x22, 0x1b, 0x28, 0x4a, 0x41}, "��Ａ"},
		{"DRCS1_G1", []byte{0x1b, 0x29, 0x20, 0x41, 0x0e, 0x21, 0x22}, "��"},
		{"DRCS2_G2", []byte{0x1b, 0x2a, 0x20, 0x42, 0xa1, 0xa2}, "��"},
		{"DRCS15_G3", []byte{0x1b, 0x2b, 0x20, 0x4f, 0x1d, 0x21, 0x46, 0x7c}, "�日"},
		{"DRCS0_G0", []byte{0x1b, 0x24, 0x28, 0x20, 0x40, 0x21, 0x22, 0x21, 0x23}, "��"},
		{"DRCS0_G1", []byte{0x1b, 0x24, 0x29, 0x20, 0x40, 0x0e, 0x21, 0x22}, "�"},
		{"DRCS0_G2_right", []byte{0x1b, 0x24, 0x2a, 0x20, 0x40, 0xa1, 0xa2}, "�"},
		{"DRCS0_G3_single", []byte{0x1b, 0x24, 0x2b, 0x20, 0x40, 0x1d, 0x21, 0x22, 0x46, 0x7c}, "�日"},
		{"unknown_one_byte_set", []byte{0x1b, 0x28, 0x55, 0x21, 0x22}, "��"},
		{"unknown_two_byte_set", []byte{0x1b, 0x24, 0x55, 0x21, 0x22, 0x21, 0x23}, "��"},
		{"mosaic", []byte{0x1b, 0x28, 0x32, 0x21, 0x22}, "��"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeWhitespaceAndNewlines(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"APR", []byte{0xa2, 0x0d, 0xa4}, "あ\nい"},
		{"APD", []byte{0xa2, 0x0a, 0xa4}, "あ\nい"},
		{"SP", []byte{0xa2, 0x20, 0xa4}, "あ　い"},
		{"SP_in_alpha", []byte{0x0e, 0x41, 0x20, 0x42}, "Ａ　Ｂ"},
		{"APF", []byte{0xa2, 0x09, 0xa4}, "あ　い"},
		{"ignorable_controls", []byte{0x00, 0x07, 0x08, 0x0b, 0x0c, 0x18, 0x1e, 0x1f, 0x7f, 0xa2}, "あ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeSingleShiftIsOneGraphicInEitherArea(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"right_SS3", []byte{0x1d, 0xa2, 0xa4}, "アい"},
		{"right_SS2_over_alpha", []byte{0x1b, 0x7e, 0x19, 0xa2, 0xc1}, "あＡ"},
		{"control_does_not_consume", []byte{0x1d, 0x80, 0x22, 0x46, 0x7c}, "ア日"},
		{"locking_shift_persists", []byte{0x0e, 0x19, 0x22, 0x41}, "あＡ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeControlOperandsAreNotText(t *testing.T) {
	controls := [][]byte{
		{0x16, 0x43}, {0x1c, 0x41, 0x42}, // PAPF, APS
		{0x8b, 0x41}, {0x90, 0x47}, {0x90, 0x20, 0x41}, // SZX, COL
		{0x91, 0x40}, {0x92, 0x40}, {0x92, 0x20, 0x41}, // FLC, CDC
		{0x93, 0x40}, {0x94, 0x40}, {0x97, 0x41}, {0x98, 0x43}, // POL, WMM, HLC, RPC
		{0x9b, 0x31, 0x3b, 0x32, 0x20, 0x53},   // CSI
		{0x9d, 0x20, 0x41}, {0x9d, 0x28, 0x40}, // TIME wait, TIME mode
		{0x9d, 0x29, 0x31, 0x3b, 0x32, 0x3b, 0x33, 0x20, 0x40}, // TIME timestamp
	}
	for _, control := range controls {
		data := append([]byte{0x0e, 0x41}, control...)
		data = append(data, 0x42)
		if got := Decode(data); got != "ＡＢ" {
			t.Errorf("control %x leaked: %q, want %q", control, got, "ＡＢ")
		}
	}
}

func TestDecodeMacroDefinitionIsNotExecuted(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"definition", []byte{0x0e, 0x41, 0x95, 0x40, 0x60, 0x1b, 0x28, 0x30, 0x22, 0x95, 0x4f, 0x42}, "ＡＢ"},
		{"definition_execute", []byte{0x0e, 0x41, 0x95, 0x41, 0x60, 0x1b, 0x28, 0x30, 0x22, 0x95, 0x4f, 0x42}, "ＡＢ"},
		{"undefined_macro_character", []byte{0x1b, 0x2b, 0x20, 0x70, 0x1d, 0x60, 0x46, 0x7c}, "�日"},
		{"truncated_definition", []byte{0x0e, 0x41, 0x95, 0x40, 0x60, 0x1b, 0x28, 0x30, 0x22}, "Ａ�"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeDamagedInputUsesReplacement(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"invalid_bytes", []byte{0xa0, 0xff, 0xa2}, "��あ"},
		{"reserved_hiragana", []byte{0xf4, 0xf5, 0xf6}, "���"},
		{"trailing_SS2", []byte{0x19}, "�"},
		{"trailing_SS3_after_control", []byte{0x1d, 0x80}, "�"},
		{"missing_double_second", []byte{0x46}, "�"},
		{"double_before_control", []byte{0x46, 0x0e, 0x41}, "�Ａ"},
		{"mixed_GL_GR", []byte{0x46, 0xfc, 0xa2}, "�」あ"},
		{"right_double_before_control", []byte{0x1b, 0x24, 0x2a, 0x42, 0xc6, 0x0e, 0x41}, "�Ａ"},
		{"ESC", []byte{0x1b}, "�"},
		{"ESC_designation", []byte{0x1b, 0x28}, "�"},
		{"ESC_double", []byte{0x1b, 0x24}, "�"},
		{"ESC_double_G", []byte{0x1b, 0x24, 0x29}, "�"},
		{"ESC_DRCS", []byte{0x1b, 0x28, 0x20}, "�"},
		{"ESC_DRCS0", []byte{0x1b, 0x24, 0x28, 0x20}, "�"},
		{"PAPF", []byte{0x16}, "�"},
		{"APS", []byte{0x1c, 0x41}, "�"},
		{"COL", []byte{0x90, 0x20}, "�"},
		{"CDC", []byte{0x92, 0x20}, "�"},
		{"SZX", []byte{0x8b}, "�"},
		{"TIME_wait", []byte{0x9d, 0x20}, "�"},
		{"TIME_mode", []byte{0x9d, 0x28}, "�"},
		{"TIME_timestamp", []byte{0x9d, 0x29, 0x31, 0x3b}, "�"},
		{"CSI", []byte{0x9b, 0x31, 0x3b, 0x32}, "�"},
		{"CSI_recovery", []byte{0x9b, 0x31, 0xa2}, "�あ"},
		{"parameter_recovery", []byte{0x90, 0xa2}, "�あ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decode(tc.data); got != tc.want {
				t.Fatalf("Decode(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestDecodeMalformedDesignationKeepsFollowingControl(t *testing.T) {
	cases := [][]byte{
		{0x1b, 0x28, 0x0e, 0x41},
		{0x1b, 0x24, 0x29, 0x0e, 0x41},
		{0x1b, 0x28, 0x20, 0x0e, 0x41},
		{0x1b, 0x24, 0x28, 0x20, 0x0e, 0x41},
	}
	for _, data := range cases {
		if got := Decode(data); got != "�Ａ" {
			t.Errorf("malformed designation %x = %q, want %q", data, got, "�Ａ")
		}
	}
}

func TestDecodeAdditionalDesignationDoesNotUseBaseJIS(t *testing.T) {
	if got := Decode([]byte{0x1b, 0x24, 0x3b, 0x46, 0x7c, 0x7a, 0x56}); got != "�[字]" {
		t.Fatalf("additional-only set = %q, want %q", got, "�[字]")
	}
}
