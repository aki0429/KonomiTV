package psi

import (
	"bytes"
	"errors"
)

type Section struct {
	PID          uint16
	PacketOffset int64
	PCRAtPUSI    *PCRSnapshot
	Raw          []byte
}
type AssemblerOptions struct {
	PIDs   []uint16
	PCRPID *uint16
}
type assemblyState struct {
	pending    []byte
	origin     int64
	snapshot   *PCRSnapshot
	haveCC     bool
	cc         uint8
	lastPacket []byte
}
type Assembler struct {
	pids    map[uint16]bool
	states  map[uint16]*assemblyState
	pcrPID  *uint16
	clocks  map[uint16]PCRSnapshot
	current *PCRSnapshot
}

func NewAssembler(opts AssemblerOptions) *Assembler {
	a := &Assembler{states: make(map[uint16]*assemblyState), clocks: make(map[uint16]PCRSnapshot)}
	if opts.PCRPID != nil {
		pid := *opts.PCRPID
		a.pcrPID = &pid
	}
	if opts.PIDs != nil {
		a.pids = make(map[uint16]bool)
		for _, pid := range opts.PIDs {
			a.pids[pid] = true
		}
	}
	return a
}

func (a *Assembler) Push(p Packet) ([]Section, error) {
	parsed, err := ParsePacket(p.Raw, p.Offset)
	if err != nil {
		if packetPIDReadable(p.Raw) {
			a.ResetPID(parsed.PID)
		} else {
			a.Reset()
		}
		return nil, err
	}
	p = parsed
	s := a.states[p.PID]
	if s == nil {
		s = &assemblyState{}
		a.states[p.PID] = s
	}
	duplicate := p.HasPayload && s.haveCC && p.ContinuityCounter == s.cc && duplicatePayloadPacket(p, s.lastPacket)
	if duplicate && bytes.Equal(p.Raw, s.lastPacket) {
		return nil, nil
	}
	if p.Discontinuity && !duplicate {
		*s = assemblyState{}
		delete(a.clocks, p.PID)
		if a.current != nil && a.current.PID == p.PID {
			a.current = nil
		}
	}
	if p.PCR != nil {
		snap := PCRSnapshot{PID: p.PID, PacketOffset: p.Offset, Value: *p.PCR, UnwrappedTicks: p.PCR.Ticks()}
		if prev, ok := a.clocks[p.PID]; ok {
			snap.UnwrappedTicks = prev.UnwrappedTicks + PCRDelta(prev.Value, *p.PCR)
		}
		a.clocks[p.PID] = snap
		if a.pcrPID == nil || *a.pcrPID == p.PID {
			a.current = &snap
		}
	}
	if duplicate {
		s.lastPacket = bytes.Clone(p.Raw)
		return nil, nil
	}
	if !p.HasPayload || len(p.Payload) == 0 {
		return nil, nil
	}
	selected := a.pids == nil || a.pids[p.PID]
	var sections []Section
	var problem error
	if selected && s.haveCC && p.ContinuityCounter != (s.cc+1)&15 {
		s.pending = nil
		problem = ErrContinuity
	}
	// Payload retransmission/PCR bookkeeping is independent of section selection.
	s.haveCC = true
	s.cc = p.ContinuityCounter
	s.lastPacket = bytes.Clone(p.Raw)
	if !selected {
		return nil, nil
	}
	pcr := a.current
	data := p.Payload
	if p.PUSI {
		n := int(data[0]) + 1
		if n > len(data) {
			s.pending = nil
			return nil, errors.Join(problem, ErrSection)
		}
		if len(s.pending) > 0 {
			more, err := s.feed(data[1:n], false, p, pcr)
			sections = append(sections, more...)
			if err != nil {
				problem = errors.Join(problem, err)
			}
			if len(s.pending) > 0 {
				problem = errors.Join(problem, ErrSection)
			}
		}
		s.pending = nil
		more, err := s.feed(data[n:], true, p, pcr)
		sections = append(sections, more...)
		if err != nil {
			problem = errors.Join(problem, err)
		}
	} else if len(s.pending) > 0 {
		more, err := s.feed(data, false, p, pcr)
		sections = append(sections, more...)
		if err != nil {
			problem = errors.Join(problem, err)
		}
	}
	return sections, problem
}

// duplicatePayloadPacket compares validated packets. PCR is always the first
// optional adaptation field (bytes 6..11); its flag and field boundaries must
// match too. No other header, adaptation, stuffing or payload byte may differ.
func duplicatePayloadPacket(p Packet, previous []byte) bool {
	if bytes.Equal(p.Raw, previous) {
		return true
	}
	return p.PCR != nil && len(previous) == PacketSize &&
		bytes.Equal(p.Raw[:6], previous[:6]) && bytes.Equal(p.Raw[12:], previous[12:])
}

func (a *Assembler) ResetPID(pid uint16) {
	delete(a.states, pid)
	delete(a.clocks, pid)
	if a.current != nil && a.current.PID == pid {
		a.current = nil
	}
}
func (a *Assembler) Reset() {
	a.states = make(map[uint16]*assemblyState)
	a.clocks = make(map[uint16]PCRSnapshot)
	a.current = nil
}

func (s *assemblyState) feed(data []byte, starts bool, p Packet, pcr *PCRSnapshot) ([]Section, error) {
	var out []Section
	var problem error
	for len(data) > 0 {
		if len(s.pending) == 0 {
			if !starts || data[0] == 0xff {
				break
			}
			s.origin = p.Offset
			s.snapshot = nil
			if pcr != nil {
				snap := *pcr
				s.snapshot = &snap
			}
		}
		target := 3
		if len(s.pending) >= 3 {
			target = 3 + (int(s.pending[1]&15)<<8 | int(s.pending[2]))
		}
		n := target - len(s.pending)
		if n > len(data) {
			n = len(data)
		}
		s.pending = append(s.pending, data[:n]...)
		data = data[n:]
		if len(s.pending) < 3 {
			break
		}
		target = 3 + (int(s.pending[1]&15)<<8 | int(s.pending[2]))
		if target > MaxSectionSize {
			s.pending = nil
			return out, errors.Join(problem, ErrSection)
		}
		if len(s.pending) < target {
			continue
		}
		raw := s.pending
		s.pending = nil
		if err := ValidateSection(raw); err != nil {
			problem = errors.Join(problem, err)
		} else {
			out = append(out, Section{PID: p.PID, PacketOffset: s.origin, PCRAtPUSI: s.snapshot, Raw: bytes.Clone(raw)})
		}
		if !starts {
			break
		}
	}
	return out, problem
}
