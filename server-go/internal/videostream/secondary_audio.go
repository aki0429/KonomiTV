package videostream

import "fmt"

// aacStreamTypes は PMT 上の AAC ストリームタイプ。
// 移植元: TSSecondaryAudioExtractor.AAC_STREAM_TYPES
var aacStreamTypes = map[int]bool{0x0F: true, 0x11: true}

// pmtEntry は PMT の 1 ストリームエントリ (記述子を含む) 。
// 移植元: biim.mpeg2ts.pmt.PMTSection の entry
type pmtEntry struct {
	streamType  int
	pid         int
	descriptors [][]byte
}

// ExtractSecondaryAudio は MPEG-TS セグメントから第 2 AAC 音声を抽出する。
// 移植元: TSSecondaryAudioExtractor.extract()
func ExtractSecondaryAudio(segmentData []byte) ([]byte, error) {
	if len(segmentData)%PacketSize != 0 {
		return nil, fmt.Errorf("MPEG-TS segment size is not aligned to 188-byte packets")
	}
	// 188 バイト単位へ分割し、壊れた同期バイトを先に検出する
	packets := [][]byte{}
	for offset := 0; offset < len(segmentData); offset += PacketSize {
		packet := segmentData[offset : offset+PacketSize]
		if packet[0] != SyncByte {
			return nil, fmt.Errorf("invalid MPEG-TS sync byte was found in the segment")
		}
		packets = append(packets, packet)
	}

	pmtPID, err := getPMTPID(packets)
	if err != nil {
		return nil, err
	}
	pmtSection, err := getPMTSection(packets, pmtPID)
	if err != nil {
		return nil, err
	}
	secondaryAudioPID, filteredPMTSection, err := buildFilteredPMT(pmtSection)
	if err != nil {
		return nil, err
	}
	pcrPID := parsePMTPCRPID(pmtSection)

	var pmtPacket []byte
	for _, packet := range packets {
		if pid(packet) == pmtPID && payloadUnitStartIndicator(packet) {
			pmtPacket = packet
			break
		}
	}
	if pmtPacket == nil {
		return nil, fmt.Errorf("PMT was not found in the MPEG-TS segment")
	}
	filteredPMTPackets := packetizeSection(
		filteredPMTSection,
		pmtPacket[1]&0x80 != 0,
		pmtPacket[1]&0x20 != 0,
		pmtPID,
		int((pmtPacket[3]&0xC0)>>6),
		int(pmtPacket[3]&0x0F),
	)

	// PAT と再構築した PMT は元の出現位置を保ち、音声 PES の到着順と PCR の時間順を変えずに返す
	output := []byte{}
	for _, packet := range packets {
		currentPID := pid(packet)
		switch {
		case currentPID == 0:
			output = append(output, packet...)
		case currentPID == pmtPID:
			if &packet[0] == &pmtPacket[0] {
				for _, filteredPacket := range filteredPMTPackets {
					output = append(output, filteredPacket...)
				}
			}
		case currentPID == secondaryAudioPID:
			output = append(output, packet...)
		case currentPID == pcrPID && hasPCR(packet):
			output = append(output, buildPCROnlyPacket(packet)...)
		}
	}
	return output, nil
}

// getPMTPID は PAT から最初の番組の PMT PID を取得する。
// 移植元: TSSecondaryAudioExtractor.__getPMTPID()
func getPMTPID(packets [][]byte) (int, error) {
	parser := newSectionParser()
	for _, packet := range packets {
		if pid(packet) != 0 {
			continue
		}
		parser.push(packet)
		for {
			section, ok := parser.pop()
			if !ok {
				break
			}
			if section[0] != 0x00 || mpegCRC32(section) != 0 {
				continue
			}
			for _, entry := range parsePAT(section) {
				if entry.programNumber != 0 {
					return entry.pid, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("PAT does not contain a program map PID")
}

// getPMTSection は PMT PID のパケットから有効な PMT セクションを取得する。
// 移植元: TSSecondaryAudioExtractor.__getPMTSection()
func getPMTSection(packets [][]byte, pmtPID int) ([]byte, error) {
	parser := newSectionParser()
	for _, packet := range packets {
		if pid(packet) != pmtPID {
			continue
		}
		parser.push(packet)
		for {
			section, ok := parser.pop()
			if !ok {
				break
			}
			if section[0] == 0x02 && mpegCRC32(section) == 0 {
				return section, nil
			}
		}
	}
	return nil, fmt.Errorf("PMT was not found in the MPEG-TS segment")
}

// parsePMTPCRPID は PMT セクションから PCR PID を取得する。
func parsePMTPCRPID(section []byte) int {
	if len(section) < 10 {
		return -1
	}
	return ((int(section[8]) & 0x1F) << 8) | int(section[9])
}

// parsePMTEntries は PMT セクションから記述子付きのストリーム一覧を取得する。
// 移植元: biim.mpeg2ts.pmt.PMTSection.__init__ の entry 構築
func parsePMTEntries(section []byte) []pmtEntry {
	entries := []pmtEntry{}
	if len(section) < 12 {
		return entries
	}
	programInfoLength := (int(section[10]&0x0F) << 8) | int(section[11])
	begin := 12 + programInfoLength
	sectionLength := (int(section[1]&0x0F) << 8) | int(section[2])
	for begin < 3+sectionLength-4 {
		if begin+5 > len(section) {
			break
		}
		streamType := int(section[begin])
		elementaryPID := ((int(section[begin+1]) & 0x1F) << 8) | int(section[begin+2])
		esInfoLength := (int(section[begin+3]&0x0F) << 8) | int(section[begin+4])

		descriptors := [][]byte{}
		offset := begin + 5
		for offset < begin+5+esInfoLength {
			if offset+2 > len(section) {
				break
			}
			descriptorLength := int(section[offset+1])
			end := min(offset+2+descriptorLength, len(section))
			descriptors = append(descriptors, section[offset+2:end])
			offset += 2 + descriptorLength
		}
		entries = append(entries, pmtEntry{streamType: streamType, pid: elementaryPID, descriptors: descriptors})
		begin += 5 + esInfoLength
	}
	return entries
}

// buildFilteredPMT は解析した PMT から第 2 AAC だけを持つ PMT を構築する。
// 移植元: TSSecondaryAudioExtractor.__buildFilteredPMT()
func buildFilteredPMT(pmtSection []byte) (int, []byte, error) {
	entries := parsePMTEntries(pmtSection)
	aacStreams := []pmtEntry{}
	for _, entry := range entries {
		if aacStreamTypes[entry.streamType] {
			aacStreams = append(aacStreams, entry)
		}
	}
	if len(aacStreams) < 2 {
		return 0, nil, fmt.Errorf("the second AAC stream was not found in the PMT")
	}

	// 解析済みの記述子長で元セクションをたどり、副音声の ES エントリーをそのまま保持する
	if len(pmtSection) < 12 {
		return 0, nil, fmt.Errorf("PMT section is too short")
	}
	programInfoLength := (int(pmtSection[10]&0x0F) << 8) | int(pmtSection[11])
	entryOffset := 12 + programInfoLength
	secondaryAudioPID := aacStreams[1].pid
	sectionWithoutCRC := append([]byte{}, pmtSection[:entryOffset]...)
	for _, entry := range entries {
		entryLength := 5
		for _, descriptor := range entry.descriptors {
			entryLength += 2 + len(descriptor)
		}
		if entry.pid == secondaryAudioPID {
			end := min(entryOffset+entryLength, len(pmtSection))
			sectionWithoutCRC = append(sectionWithoutCRC, pmtSection[entryOffset:end]...)
			break
		}
		entryOffset += entryLength
	}

	// ES を除去した分だけセクション長と CRC を更新する
	sectionLength := len(sectionWithoutCRC) + 4 - 3
	sectionWithoutCRC[1] = (sectionWithoutCRC[1] & 0xF0) | byte((sectionLength>>8)&0x0F)
	sectionWithoutCRC[2] = byte(sectionLength & 0xFF)
	crc := mpegCRC32(sectionWithoutCRC)
	result := append(sectionWithoutCRC, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return secondaryAudioPID, result, nil
}

// packetizeSection はセクションを TS パケット列に変換する。
// 移植元: biim.mpeg2ts.packetize.packetize_section()
func packetizeSection(
	section []byte,
	transportErrorIndicator bool,
	transportPriority bool,
	pid int,
	transportScramblingControl int,
	continuityCounter int,
) [][]byte {
	result := [][]byte{}
	begin := 0
	for begin < len(section) {
		next := min(len(section), begin+(PacketSize-HeaderSize)-boolToInt(begin == 0))
		packet := []byte{
			SyncByte,
			boolToByte(transportErrorIndicator)<<7 | boolToByte(begin == 0)<<6 | boolToByte(transportPriority)<<5 | byte((pid&0x1F00)>>8),
			byte(pid & 0x00FF),
			byte(transportScramblingControl<<6) | 0x10 | byte(continuityCounter&0x0F),
		}
		if begin == 0 {
			packet = append(packet, 0)
		}
		packet = append(packet, section[begin:next]...)
		stuffing := (PacketSize - HeaderSize) - ((next - begin) + boolToInt(begin == 0))
		for range stuffing {
			packet = append(packet, 0xFF)
		}
		result = append(result, packet)
		continuityCounter = (continuityCounter + 1) & 0x0F
		begin = next
	}
	return result
}

// buildPCROnlyPacket は入力パケットの PCR だけを保持したアダプテーションフィールド専用パケットを返す。
// 移植元: TSSecondaryAudioExtractor.__buildPCROnlyPacket()
func buildPCROnlyPacket(packet []byte) []byte {
	// PCR の 6 バイトをそのまま保持し、映像ペイロードをスタッフィングへ置き換える
	output := make([]byte, PacketSize)
	for index := range output {
		output[index] = 0xFF
	}
	copy(output[:12], packet[:12])
	output[1] &= 0xBF
	output[3] = 0x20 | (packet[3] & 0x0F)
	output[4] = PacketSize - HeaderSize - 1
	output[5] = 0x10 | (packet[5] & 0x80)
	return output
}

// boolToInt は bool を int に変換する。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// boolToByte は bool を byte に変換する。
func boolToByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}
