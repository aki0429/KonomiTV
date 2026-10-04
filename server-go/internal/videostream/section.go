package videostream

import "math"

// sectionParser は TS パケットから PSI セクション (PAT / PMT) を組み立てる。
// 移植元: biim.mpeg2ts.parser.SectionParser
type sectionParser struct {
	section []byte
	queue   [][]byte
}

// newSectionParser はセクションパーサーを生成する。
func newSectionParser() *sectionParser {
	return &sectionParser{}
}

// push は TS パケットを追加する。
func (p *sectionParser) push(packet []byte) {
	begin := payloadOffset(packet)
	if payloadUnitStartIndicator(packet) {
		begin++
	}
	if p.section == nil {
		if !payloadUnitStartIndicator(packet) {
			return
		}
		// pointer_field 分だけ読み飛ばす
		// 移植元: biim.mpeg2ts.parser.SectionParser.push() の ts.pointer_field(packet)
		// (pointer_field は pusi による +1 の前の位置にある)
		pointerPosition := payloadOffset(packet)
		if pointerPosition >= len(packet) {
			return
		}
		begin += int(packet[pointerPosition])
	}
	if begin >= len(packet) {
		return
	}

	if payloadUnitStartIndicator(packet) {
		for begin < len(packet) {
			if packet[begin] == 0xFF {
				break
			}
			var next int
			if p.section != nil {
				sectionLength := (int(p.section[1]&0x0F) << 8) | int(p.section[2])
				next = min(begin+sectionLength, len(packet))
			} else {
				if begin+3 > len(packet) {
					break
				}
				sectionLength := (int(packet[begin+1]&0x0F) << 8) | int(packet[begin+2])
				next = min(begin+3+sectionLength, len(packet))
				p.section = []byte{}
			}
			if next > begin {
				p.section = append(p.section, packet[begin:next]...)
			}
			if len(p.section) >= 3 {
				sectionLength := (int(p.section[1]&0x0F) << 8) | int(p.section[2])
				switch {
				case len(p.section) == sectionLength+3:
					p.queue = append(p.queue, p.section)
					p.section = nil
				case len(p.section) > sectionLength+3:
					p.section = nil
				}
			}
			begin = next
			if p.section == nil && begin >= len(packet) {
				break
			}
		}
	} else if p.section != nil {
		if len(p.section) < 3 {
			return
		}
		sectionLength := (int(p.section[1]&0x0F) << 8) | int(p.section[2])
		remains := max(0, sectionLength+3-len(p.section))
		next := min(begin+remains, len(packet))
		if next > begin {
			p.section = append(p.section, packet[begin:next]...)
		}
		if len(p.section) == sectionLength+3 {
			p.queue = append(p.queue, p.section)
			p.section = nil
		} else if len(p.section) > sectionLength+3 {
			p.section = nil
		}
	}
}

// pop は解析済みのセクションを 1 つ取り出す。
func (p *sectionParser) pop() ([]byte, bool) {
	if len(p.queue) == 0 {
		return nil, false
	}
	result := p.queue[0]
	p.queue = p.queue[1:]
	return result, true
}

// patEntry は PAT の 1 エントリ。
type patEntry struct {
	programNumber int
	pid           int
}

// parsePAT は PAT セクションからエントリ一覧を取得する。
func parsePAT(section []byte) []patEntry {
	entries := []patEntry{}
	// 先頭 8 バイト (table_id, section_length, transport_stream_id, version, section_number, last_section_number) を読み飛ばす
	if len(section) < 8 {
		return entries
	}
	// 末尾 4 バイトは CRC32
	for offset := 8; offset+4 <= len(section)-4; offset += 4 {
		programNumber := (int(section[offset]) << 8) | int(section[offset+1])
		pid := ((int(section[offset+2]) & 0x1F) << 8) | int(section[offset+3])
		entries = append(entries, patEntry{programNumber: programNumber, pid: pid})
	}
	return entries
}

// pmtStream は PMT の 1 ストリーム。
type pmtStream struct {
	streamType int
	pid        int
}

// parsePMT は PMT セクションから PCR PID とストリーム一覧を取得する。
func parsePMT(section []byte) (int, []pmtStream) {
	streams := []pmtStream{}
	// 先頭 12 バイト (table_id, section_length, program_number, version, section_number, last_section_number, PCR_PID, program_info_length) を読み飛ばす
	if len(section) < 12 {
		return -1, streams
	}
	pcrPID := ((int(section[8]) & 0x1F) << 8) | int(section[9])
	programInfoLength := (int(section[10]&0x0F) << 8) | int(section[11])
	offset := 12 + programInfoLength
	for offset+5 <= len(section)-4 {
		streamType := int(section[offset])
		elementaryPID := ((int(section[offset+1]) & 0x1F) << 8) | int(section[offset+2])
		esInfoLength := (int(section[offset+3]&0x0F) << 8) | int(section[offset+4])
		streams = append(streams, pmtStream{streamType: streamType, pid: elementaryPID})
		offset += 5 + esInfoLength
	}
	return pcrPID, streams
}

// mathRoundToEven は math.RoundToEven の別名 (seeker.go から使う) 。
func mathRoundToEven(value float64) float64 {
	return math.RoundToEven(value)
}
