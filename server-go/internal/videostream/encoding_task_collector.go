package videostream

import "math"

// TSKeyFrameCollector は連続して読み込まれる TS パケットから、入力ファイル側のキーフレーム位置を収集する。
// 移植元: app/utils/TSKeyFrameSeeker.py の TSKeyFrameCollector
type TSKeyFrameCollector struct {
	streamInfo StreamInfo
	parser     *pesParser
	// pendingPESStart は直前に見つけた PES 開始位置 (未確定は -1) 。
	// PES パーサーは「次の PES 開始」を見た時点で前の PES を返すため、開始位置を別途保持する。
	pendingPESStart int64
	// unwrapTargetDTS は 33bit DTS を単調増加の DTS に展開するための直近基準。
	unwrapTargetDTS int64
}

// NewTSKeyFrameCollector は収集器を生成する。
// initialUnwrapTargetDTS は 33bit DTS を展開する際の初期基準 DTS (通常は開始セグメントの source_start_dts) 。
func NewTSKeyFrameCollector(streamInfo *StreamInfo, initialUnwrapTargetDTS int64) *TSKeyFrameCollector {
	return &TSKeyFrameCollector{
		streamInfo:      *streamInfo,
		parser:          newPESParser(),
		pendingPESStart: -1,
		unwrapTargetDTS: initialUnwrapTargetDTS,
	}
}

// Push は読み込んだ TS チャンクを解析し、見つかったキーフレーム位置を返す。
// chunkFileOffset は chunk の先頭が元ファイル上で始まるバイト位置。
func (c *TSKeyFrameCollector) Push(chunk []byte, chunkFileOffset int64) []KeyFramePosition {
	positions := []KeyFramePosition{}
	packetSize := c.streamInfo.PacketSize
	offset := 0
	// チャンク内の TS パケットを 1 つずつ走査し、映像 PID のキーフレームを検出する
	for offset+packetSize <= len(chunk) {
		currentFileOffset := chunkFileOffset + int64(offset)
		packet, ok := normalizePacket(chunk[offset:offset+packetSize], packetSize)
		offset += packetSize
		if !ok {
			continue
		}
		// 映像 PID 以外のパケットはキーフレーム判定に不要
		if pid(packet) != c.streamInfo.VideoPID {
			continue
		}

		// payload_unit_start_indicator が立っているパケットは新しい PES の先頭を示す
		currentPESStart := int64(-1)
		if packet[1]&0x40 != 0 {
			currentPESStart = currentFileOffset
		}

		c.parser.push(packet)
		for {
			current, ok := c.parser.pop()
			if !ok {
				break
			}
			// 直前の PES 開始位置を取り出し、今回のパケットが PES 開始なら次回用にセットする
			pesStart := c.pendingPESStart
			if currentPESStart >= 0 {
				c.pendingPESStart = currentPESStart
			}
			// 初回は pending が未確定のためスキップ (先頭 PES の開始位置が確定していない)
			if pesStart < 0 {
				continue
			}

			// DTS がなければ PTS で代用する (B フレームを持たないストリーム向け)
			rawDTS, ok := pesTimestamp(current)
			if !ok {
				continue
			}
			// 33bit の生 DTS を、直前の DTS に最も近い単調増加値へ展開する
			sourceDTS := unwrapNear(rawDTS, c.unwrapTargetDTS)
			c.unwrapTargetDTS = sourceDTS
			if !hasKeyFrame(current, c.streamInfo.Codec) {
				continue
			}
			positions = append(positions, KeyFramePosition{SourceFilePosition: pesStart, SourceStartDTS: sourceDTS})
		}

		// parser が PES を返さなかった場合も、次回の PES 判定に開始位置を渡す
		if currentPESStart >= 0 && c.pendingPESStart != currentPESStart {
			c.pendingPESStart = currentPESStart
		}
	}
	return positions
}

// packetizePES は PES を TS パケット列に変換する (transport_error / priority / scrambling はすべて 0) 。
// 移植元: biim.mpeg2ts.packetize.packetize_pes()
func packetizePES(pesData []byte, packetPID int, continuityCounter int) [][]byte {
	const payloadSize = PacketSize - HeaderSize
	result := [][]byte{}
	begin := 0
	for begin < len(pesData) {
		next := min(len(pesData), begin+payloadSize)
		length := next - begin
		packet := make([]byte, 0, PacketSize)
		adaptation := byte(0x10)
		if payloadSize > length {
			adaptation = 0x30
		}
		packet = append(packet,
			SyncByte,
			boolToByte(begin == 0)<<6|byte((packetPID&0x1F00)>>8),
			byte(packetPID&0x00FF),
			adaptation|byte(continuityCounter&0x0F),
		)
		if payloadSize > length {
			packet = append(packet, byte(payloadSize-length-1))
		}
		if payloadSize > length+1 {
			packet = append(packet, 0x00)
		}
		if payloadSize > length+2 {
			for range payloadSize - length - 2 {
				packet = append(packet, 0xFF)
			}
		}
		packet = append(packet, pesData[begin:next]...)
		result = append(result, packet)
		continuityCounter = (continuityCounter + 1) & 0x0F
		begin = next
	}
	return result
}

// initialPATPMT は録画ファイルから抽出した、セグメント開始位置に最も近い PAT / PMT パケット。
type initialPATPMT struct {
	// PAT / PMT は 188 バイトの TS パケット (見つからなければ nil) 。
	PAT []byte
	PMT []byte
	// PATDistance / PMTDistance は開始位置からのバイト距離。
	PATDistance int64
	PMTDistance int64
}

// Data は PAT と PMT を連結したデータを返す (両方見つかっていなければ nil) 。
func (p initialPATPMT) Data() []byte {
	if p.PAT == nil || p.PMT == nil {
		return nil
	}
	data := make([]byte, 0, len(p.PAT)+len(p.PMT))
	data = append(data, p.PAT...)
	return append(data, p.PMT...)
}

// findClosestPATPMT は searchData (ファイル位置 searchStartPos から始まる) から、sourcePos に最も近い PAT / PMT を探す。
// 移植元: VideoEncodingTask.run() の「PAT/PMT を抽出（セグメント開始位置に最も近いものを保持）」ブロック
//
// Python 版と同じく、このブロックは 188 バイトパケット固定で走査する (192 バイトの録画でも同じ)。
func findClosestPATPMT(searchData []byte, searchStartPos int64, sourcePos int64) initialPATPMT {
	result := initialPATPMT{PATDistance: math.MaxInt64, PMTDistance: math.MaxInt64}
	patParser := newSectionParser()
	pmtParser := newSectionParser()
	pmtPID := -1

	firstProgramPID := func(section []byte) int {
		for _, entry := range parsePAT(section) {
			if entry.programNumber != 0 {
				return entry.pid
			}
		}
		return -1
	}
	absolute := func(value int64) int64 {
		if value < 0 {
			return -value
		}
		return value
	}

	offset := 0
	for offset+PacketSize <= len(searchData) {
		// 同期バイトを探す
		if searchData[offset] != SyncByte {
			offset++
			continue
		}
		// 188 バイト先 (必要であればさらに 188 バイト先) の同期バイトを確認し、TS パケット境界であるか検証する
		isAligned := true
		nextOffset := offset + PacketSize
		if nextOffset < len(searchData) && searchData[nextOffset] != SyncByte {
			isAligned = false
		}
		secondOffset := offset + PacketSize*2
		if isAligned && secondOffset < len(searchData) && searchData[secondOffset] != SyncByte {
			isAligned = false
		}
		if !isAligned {
			offset++
			continue
		}

		packet := searchData[offset : offset+PacketSize]
		packetPID := pid(packet)
		// 現在のパケットの実際のファイル位置
		currentFilePos := searchStartPos + int64(offset)
		distance := absolute(currentFilePos - sourcePos)

		if packetPID == 0x00 {
			patParser.push(packet)
			for {
				section, ok := patParser.pop()
				if !ok {
					break
				}
				if mpegCRC32(section) != 0 {
					continue
				}
				// 開始位置以前の PAT を優先 (より近いものに更新)
				if currentFilePos <= sourcePos {
					if result.PAT == nil || distance < result.PATDistance {
						result.PAT = append([]byte(nil), packet...)
						result.PATDistance = distance
						if pid := firstProgramPID(section); pid >= 0 {
							pmtPID = pid
						}
					}
				} else if result.PAT == nil {
					// 開始位置以前に PAT が見つからなかった場合のフォールバック
					result.PAT = append([]byte(nil), packet...)
					result.PATDistance = distance
					if pid := firstProgramPID(section); pid >= 0 {
						pmtPID = pid
					}
				}
				break
			}
		} else if pmtPID >= 0 && packetPID == pmtPID {
			pmtParser.push(packet)
			for {
				section, ok := pmtParser.pop()
				if !ok {
					break
				}
				if mpegCRC32(section) != 0 {
					continue
				}
				if currentFilePos <= sourcePos {
					if result.PMT == nil || distance < result.PMTDistance {
						result.PMT = append([]byte(nil), packet...)
						result.PMTDistance = distance
					}
				} else if result.PMT == nil {
					result.PMT = append([]byte(nil), packet...)
					result.PMTDistance = distance
				}
				break
			}
		}
		offset += PacketSize
	}
	return result
}
