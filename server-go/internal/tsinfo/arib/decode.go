// Package arib decodes ARIB STD-B24 text used by broadcast metadata.
package arib

import "strings"

type characterSet struct {
	final  byte
	double bool
	drcs   bool
}

type decoder struct {
	data   []byte
	pos    int
	sets   [4]characterSet
	gl, gr int
	single int
	out    strings.Builder
}

// Decode converts ARIB STD-B24 encoded bytes into a Unicode string.
// It preserves fullwidth text; metadata normalization is a separate step.
func Decode(data []byte) string {
	d := decoder{data: data, sets: [4]characterSet{{final: 0x42, double: true}, {final: 0x4a}, {final: 0x30}, {final: 0x31}}, gr: 2, single: -1}
	for d.pos < len(d.data) {
		b := d.data[d.pos]
		d.pos++
		switch {
		case b == 0x0d || b == 0x0a:
			d.out.WriteByte('\n')
		case b == 0x20 || b == 0x09:
			d.out.WriteRune('　')
		case b == 0x0e:
			d.gl = 1
		case b == 0x0f:
			d.gl = 0
		case b == 0x1b:
			d.escape()
		case b == 0x19:
			d.single = 2
		case b == 0x1d:
			d.single = 3
		case b == 0x16 || b == 0x1c || (b >= 0x80 && b <= 0x9f):
			d.control(b)
		case b >= 0x21 && b <= 0x7e:
			slot := d.gl
			if d.single >= 0 {
				slot = d.single
				d.single = -1
			}
			d.graphic(d.sets[slot], b, false)
		case b == 0xa0 || b == 0xff:
			d.out.WriteRune('�')
		case b >= 0xa1 && b <= 0xfe:
			slot := d.gr
			if d.single >= 0 {
				slot = d.single
				d.single = -1
			}
			d.graphic(d.sets[slot], b&0x7f, true)
		}
	}
	if d.single >= 0 {
		d.out.WriteRune('�')
	}
	return d.out.String()
}

func (d *decoder) graphic(set characterSet, b byte, right bool) {
	if set.double {
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		next := d.data[d.pos]
		if right {
			if next < 0xa1 || next > 0xfe {
				d.out.WriteRune('�')
				return
			}
			next &= 0x7f
		} else if next < 0x21 || next > 0x7e {
			d.out.WriteRune('�')
			return
		}
		d.pos++
		if set.drcs || (set.final != 0x42 && set.final != 0x3b) {
			d.out.WriteRune('�')
			return
		}
		if label, ok := programSymbols[uint16(b)<<8|uint16(next)]; ok {
			d.out.WriteString(label)
			return
		}
		if r, ok := additionalSymbols[uint16(b)<<8|uint16(next)]; ok {
			d.out.WriteRune(r)
			return
		}
		if set.final == 0x3b {
			d.out.WriteRune('�')
			return
		}
		r := jis0208[int(b-0x21)*94+int(next-0x21)]
		if r == 0 {
			r = '�'
		}
		d.out.WriteRune(r)
		return
	}
	if set.drcs {
		d.out.WriteRune('�')
		return
	}
	switch set.final {
	case 0x4a, 0x36:
		r := rune(b) + 0xfee0
		if b == 0x5c {
			r = '￥'
		}
		if b == 0x7e {
			r = '￣'
		}
		d.out.WriteRune(r)
	case 0x31, 0x38:
		if b <= 0x76 {
			d.out.WriteRune(rune(0x30a1 + int(b-0x21)))
		} else {
			d.out.WriteRune([]rune("ヽヾー。「」、・")[b-0x77])
		}
	case 0x49:
		if b >= 0x26 && b <= 0x5d {
			const kana = "ヲァィゥェォャュョッーアイウエオカキクケコサシスセソタチツテトナニヌネノハヒフヘホマミムメモヤユヨラリルレロワン"
			d.out.WriteRune([]rune(kana)[b-0x26])
		} else {
			punctuation := map[byte]rune{0x21: '。', 0x22: '「', 0x23: '」', 0x24: '、', 0x25: '・', 0x5e: '゛', 0x5f: '゜'}
			if r, ok := punctuation[b]; ok {
				d.out.WriteRune(r)
			} else {
				d.out.WriteRune('�')
			}
		}
	case 0x30, 0x37:
		if b <= 0x73 {
			d.out.WriteRune(rune(0x3041 + int(b-0x21)))
		} else if b >= 0x77 {
			d.out.WriteRune([]rune("ゝゞー。「」、・")[b-0x77])
		} else {
			d.out.WriteRune('�')
		}
	default:
		d.out.WriteRune('�')
	}
}

// escape consumes designation parameters without ever expanding code strings.
func (d *decoder) escape() {
	if d.pos >= len(d.data) {
		d.out.WriteRune('�')
		return
	}
	c := d.data[d.pos]
	d.pos++
	switch c {
	case 0x6e:
		d.gl = 2
		return
	case 0x6f:
		d.gl = 3
		return
	case 0x7e:
		d.gr = 1
		return
	case 0x7d:
		d.gr = 2
		return
	case 0x7c:
		d.gr = 3
		return
	}
	slot := 0
	set := characterSet{}
	if c == 0x24 {
		set.double = true
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		c = d.data[d.pos]
		d.pos++
		if c >= 0x28 && c <= 0x2b {
			slot = int(c - 0x28)
			if d.pos >= len(d.data) {
				d.out.WriteRune('�')
				return
			}
			c = d.data[d.pos]
			d.pos++
		}
	} else if c >= 0x28 && c <= 0x2b {
		slot = int(c - 0x28)
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		c = d.data[d.pos]
		d.pos++
	} else {
		d.out.WriteRune('�')
		return
	}
	if c == 0x20 {
		set.drcs = true
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		c = d.data[d.pos]
		d.pos++
	}
	if c < 0x30 || c > 0x7e {
		// Let the outer loop interpret this byte normally, especially LS/ESC.
		d.pos--
		d.out.WriteRune('�')
		return
	}
	set.final = c
	d.sets[slot] = set
}

// control discards presentation attributes including all parameter bytes.
// This is a text decoder, not a caption renderer: no cursor or repeat effects.
func (d *decoder) control(code byte) {
	switch code {
	case 0x16, 0x8b, 0x91, 0x93, 0x94, 0x97, 0x98:
		d.skipParameters(1)
	case 0x1c:
		d.skipParameters(2)
	case 0x90, 0x92:
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		if d.data[d.pos] == 0x20 {
			d.pos++
			d.skipParameters(1)
		} else {
			d.skipParameters(1)
		}
	case 0x95:
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		mode := d.data[d.pos]
		d.pos++
		if mode == 0x4f {
			return
		}
		if mode != 0x40 && mode != 0x41 {
			d.out.WriteRune('�')
			return
		}
		for d.pos+1 < len(d.data) {
			if d.data[d.pos] == 0x95 && d.data[d.pos+1] == 0x4f {
				d.pos += 2
				return
			}
			d.pos++
		}
		d.pos = len(d.data)
		d.out.WriteRune('�')
	case 0x9b:
		d.skipSequence()
	case 0x9d:
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		mode := d.data[d.pos]
		d.pos++
		if mode == 0x20 || mode == 0x28 {
			d.skipParameters(1)
		} else if mode == 0x29 {
			d.skipSequence()
		} else {
			d.out.WriteRune('�')
		}
	}
}

func (d *decoder) skipParameters(count int) {
	for range count {
		if d.pos >= len(d.data) {
			d.out.WriteRune('�')
			return
		}
		b := d.data[d.pos]
		if b < 0x40 || b > 0x7f {
			d.out.WriteRune('�')
			return
		}
		d.pos++
	}
}

func (d *decoder) skipSequence() {
	for d.pos < len(d.data) {
		b := d.data[d.pos]
		if b >= 0x40 && b <= 0x7e {
			d.pos++
			return
		}
		if b < 0x20 || b > 0x3f {
			d.out.WriteRune('�')
			return
		}
		d.pos++
	}
	d.out.WriteRune('�')
}
