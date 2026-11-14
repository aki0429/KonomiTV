package metadata

// clearLeadingPartialAACFrames はサンプル先頭の切れたAACフレームを除去し、
// 偶然含まれるADTS同期語をFFprobeが誤認しないようにする。変更するのは渡されたサンプルだけ。
// 移植元: MetadataAnalyzer.__analyzeFFprobe.ClearLeadingPartialAACFrames
func clearLeadingPartialAACFrames(sample []byte, packetSize int) []byte {
	type audio struct {
		data      []byte
		positions []int
	}
	audios := map[uint16]*audio{}
	for offset := 0; offset+packetSize <= len(sample); offset += packetSize {
		start := offset + packetSize - 188
		packet := sample[start : start+188]
		if packet[0] != 0x47 || packet[3]&0x10 == 0 {
			continue
		}
		pid := uint16(packet[1]&31)<<8 | uint16(packet[2])
		payload := start + 4
		if packet[3]&0x20 != 0 {
			payload += 1 + int(packet[4])
		}
		payload = min(payload, start+188)
		position := payload
		if packet[1]&0x40 != 0 {
			if payload+9 > len(sample) || sample[payload] != 0 || sample[payload+1] != 0 || sample[payload+2] != 1 {
				continue
			}
			if audios[pid] == nil {
				if sample[payload+3] < 0xc0 || sample[payload+3] > 0xdf {
					continue
				}
				audios[pid] = &audio{}
			}
			position = payload + 9 + int(sample[payload+8])
		} else if audios[pid] == nil {
			continue
		}
		a := audios[pid]
		if len(a.data) >= 64*1024 || position >= start+188 {
			continue
		}
		for p := position; p < start+188; p++ {
			a.data = append(a.data, sample[p])
			a.positions = append(a.positions, p)
		}
	}
	parse := func(data []byte, position int) ([4]int, bool) {
		if position+7 > len(data) || data[position] != 0xff || data[position+1]&0xf6 != 0xf0 {
			return [4]int{}, false
		}
		v := data[position:]
		header := [4]int{int(v[2] >> 6), int(v[2] >> 2 & 15), int(v[2]&1)<<2 | int(v[3]>>6), int(v[3]&3)<<11 | int(v[4])<<3 | int(v[5]>>5)}
		return header, header[1] < 13 && header[3] >= 7
	}
	for _, a := range audios {
		for position := range len(a.data) {
			first, ok := parse(a.data, position)
			if !ok {
				continue
			}
			next, count := position, 0
			for count < 3 {
				header, ok := parse(a.data, next)
				if !ok || header[0] != first[0] || header[1] != first[1] || header[2] != first[2] {
					break
				}
				count++
				next += header[3]
			}
			if count == 3 {
				for _, p := range a.positions[:position] {
					sample[p] = 0
				}
				break
			}
		}
	}
	return sample
}
