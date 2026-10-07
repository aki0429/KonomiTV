package psi

import "errors"

var (
	ErrTransport        = errors.New("transport error indicator")
	ErrScrambled        = errors.New("scrambled TS payload")
	ErrContinuity       = errors.New("TS continuity counter gap or conflicting duplicate")
	ErrOptions          = errors.New("invalid scan options")
	ErrLimit            = errors.New("scan limit reached")
	ErrPacket           = errors.New("invalid TS packet")
	ErrUnsupportedTable = errors.New("unsupported PSI table ID")
	ErrSection          = errors.New("invalid PSI section")
	ErrCRC              = errors.New("invalid MPEG-2 CRC")
)

const MaxSectionSize = 4096

// assemblyProblemCounts counts leaves, not joined parents: every framing/CRC
// error describes one invalid section, and a CC gap is a separate event.
// Following both Unwrap forms also keeps wrapped sentinels countable.
func assemblyProblemCounts(err error) (invalid, continuity int64) {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			i, c := assemblyProblemCounts(child)
			invalid += i
			continuity += c
		}
	case interface{ Unwrap() error }:
		return assemblyProblemCounts(e.Unwrap())
	default:
		if errors.Is(err, ErrSection) || errors.Is(err, ErrCRC) {
			invalid++
		}
		if errors.Is(err, ErrContinuity) {
			continuity++
		}
	}
	return invalid, continuity
}
