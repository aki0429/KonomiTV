package psi

import "time"

var jst = time.FixedZone("JST", 9*60*60)

func bcd(b byte) (int, bool) {
	if b>>4 > 9 || b&15 > 9 {
		return 0, false
	}
	return int(b>>4)*10 + int(b&15), true
}

func DecodeJSTTime(raw []byte) (time.Time, bool) {
	if len(raw) != 5 {
		return time.Time{}, false
	}
	h, a := bcd(raw[2])
	m, b := bcd(raw[3])
	s, c := bcd(raw[4])
	if !a || !b || !c || h > 23 || m > 59 || s > 59 {
		return time.Time{}, false
	}
	mjd := int(raw[0])<<8 | int(raw[1])
	date := time.Date(1858, 11, 17, h, m, s, 0, jst).AddDate(0, 0, mjd)
	return date, true
}

func DecodeDuration(raw []byte) (time.Duration, bool) {
	if len(raw) != 3 {
		return 0, false
	}
	h, a := bcd(raw[0])
	m, b := bcd(raw[1])
	s, c := bcd(raw[2])
	if !a || !b || !c || m > 59 || s > 59 {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second, true
}
