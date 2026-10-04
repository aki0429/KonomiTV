// Package videostream は録画番組のストリーミング (HLS セグメント配信) を扱う。
//
// 移植元: server/app/streams/VideoStream.py・VideoEncodingTask.py・VideoSegmentPlanner.py と
// server/app/utils/TSKeyFrameSeeker.py・MP4KeyFrameParser.py・TSSecondaryAudioExtractor.py
package videostream

import (
	"bytes"
	"encoding/binary"
	"math"
)

// MPEG-TS の定数 (biim.mpeg2ts の ts モジュール相当) 。
const (
	// PacketSize は正規化後の TS パケットサイズ。
	PacketSize = 188
	// HeaderSize は TS パケットのヘッダーサイズ。
	HeaderSize = 4
	// SyncByte は TS パケットの同期バイト。
	SyncByte = 0x47
	// PCRCycle は PCR / PTS / DTS の 33bit ラップアラウンド周期。
	PCRCycle = int64(1) << 33
	// HZ は PCR / PTS / DTS の時間分解能 (90kHz) 。
	HZ = int64(90000)
	// PESHeaderSize は PES パケットのヘッダーサイズ (packet_start_code_prefix + stream_id + PES_packet_length) 。
	PESHeaderSize = 6
)

// pid は TS パケットの PID を返す。
func pid(packet []byte) int {
	return (int(packet[1]&0x1F) << 8) | int(packet[2])
}

// payloadUnitStartIndicator は PES / セクションの先頭パケットかどうかを返す。
func payloadUnitStartIndicator(packet []byte) bool {
	return packet[1]&0x40 != 0
}

// hasAdaptationField はアダプテーションフィールドを持つかどうかを返す。
func hasAdaptationField(packet []byte) bool {
	return packet[3]&0x20 != 0
}

// adaptationFieldLength はアダプテーションフィールドの長さを返す。
func adaptationFieldLength(packet []byte) int {
	if !hasAdaptationField(packet) {
		return 0
	}
	return int(packet[4])
}

// payloadOffset は TS パケットのペイロード開始位置を返す。
// アダプテーションフィールドがある場合のみ、その長さバイト分を読み飛ばす。
// 移植元: biim.mpeg2ts の各パーサーで使われる
// ts.HEADER_SIZE + (1 + ts.adaptation_field_length(packet) if ts.has_adaptation_field(packet) else 0)
func payloadOffset(packet []byte) int {
	if hasAdaptationField(packet) {
		return HeaderSize + 1 + adaptationFieldLength(packet)
	}
	return HeaderSize
}

// hasPCR はアダプテーションフィールドに PCR が含まれるかどうかを返す。
func hasPCR(packet []byte) bool {
	return hasAdaptationField(packet) && adaptationFieldLength(packet) > 0 && packet[HeaderSize+1]&0x10 != 0
}

// readPCR は TS パケットから PCR (33bit, 90kHz) を読み取る。
func readPCR(packet []byte) (int64, bool) {
	if !hasPCR(packet) {
		return 0, false
	}
	offset := HeaderSize + 1 + 1
	var pcrBase int64
	for index := range 4 {
		pcrBase = (pcrBase << 8) | int64(packet[offset+index])
	}
	pcrBase = (pcrBase << 1) | int64((packet[offset+4]&0x80)>>7)
	return pcrBase, true
}

// detectPacketSize は TS パケットサイズを 188 バイトまたは 192 バイトから推定する。
// 移植元: TSKeyFrameSeeker.__detectPacketSize()
func detectPacketSize(head []byte) int {
	for _, packetSize := range []int{PacketSize, 192} {
		for startOffset := range packetSize {
			aligned := true
			for index := range 5 {
				position := startOffset + packetSize*index
				if position >= len(head) || head[position] != SyncByte {
					aligned = false
					break
				}
			}
			if aligned {
				return packetSize
			}
		}
	}
	return PacketSize
}

// normalizePacket は 188 / 192 バイトの TS パケットを 188 バイトのパケットへ正規化する。
// 同期が取れない場合は false を返す。
// 移植元: TSKeyFrameSeeker.normalizePacket()
func normalizePacket(packet []byte, packetSize int) ([]byte, bool) {
	if len(packet) != packetSize {
		return nil, false
	}
	if packetSize == 192 {
		packet = packet[4:]
	}
	if len(packet) != PacketSize || packet[0] != SyncByte {
		return nil, false
	}
	return packet, true
}

// pes は解析済みの PES パケット。
type pes struct {
	payload []byte
}

// newPES は PES のペイロードを保持する pes を生成する。
func newPES(payload []byte) *pes {
	return &pes{payload: payload}
}

// streamID は stream_id を返す。
func (p *pes) streamID() byte {
	if len(p.payload) < 4 {
		return 0
	}
	return p.payload[3]
}

// pesPacketLength は PES_packet_length を返す。
func (p *pes) pesPacketLength() int {
	if len(p.payload) < PESHeaderSize {
		return 0
	}
	return int(binary.BigEndian.Uint16(p.payload[4:6]))
}

// hasOptionalPESHeader はオプショナル PES ヘッダーを持つかどうかを返す。
func (p *pes) hasOptionalPESHeader() bool {
	switch p.streamID() {
	case 0xBC, 0xBF, 0xF0, 0xF1, 0xF2, 0xF8, 0xFF, 0xBE:
		return false
	default:
		return true
	}
}

// hasPTS は PTS を持つかどうかを返す。
func (p *pes) hasPTS() bool {
	if !p.hasOptionalPESHeader() || len(p.payload) < PESHeaderSize+2 {
		return false
	}
	return p.payload[PESHeaderSize+1]&0x80 != 0
}

// hasDTS は DTS を持つかどうかを返す。
func (p *pes) hasDTS() bool {
	if !p.hasOptionalPESHeader() || len(p.payload) < PESHeaderSize+2 {
		return false
	}
	return p.payload[PESHeaderSize+1]&0x40 != 0
}

// readTimestamp は 5 バイトの PTS / DTS 表現を 33bit の値として読み取る。
func readTimestamp(data []byte) int64 {
	if len(data) < 5 {
		return 0
	}
	var value int64
	value |= int64((data[0]&0x0E)>>1) << 30
	value |= int64(data[1]) << 22
	value |= int64((data[2]&0xFE)>>1) << 15
	value |= int64(data[3]) << 7
	value |= int64((data[4] & 0xFE) >> 1)
	return value
}

// pts は PTS を返す (存在しない場合は false) 。
func (p *pes) pts() (int64, bool) {
	if !p.hasPTS() || len(p.payload) < PESHeaderSize+8 {
		return 0, false
	}
	return readTimestamp(p.payload[PESHeaderSize+3 : PESHeaderSize+8]), true
}

// dts は DTS を返す (存在しない場合は false) 。
func (p *pes) dts() (int64, bool) {
	if !p.hasDTS() || len(p.payload) < PESHeaderSize+8 {
		return 0, false
	}
	// PTS がある場合は DTS はその 5 バイト後ろに格納される
	offset := PESHeaderSize + 3
	if p.hasPTS() {
		offset += 5
	}
	if len(p.payload) < offset+5 {
		return 0, false
	}
	return readTimestamp(p.payload[offset : offset+5]), true
}

// packetData は PES のペイロード (エレメンタリーストリーム) を返す。
func (p *pes) packetData() []byte {
	if !p.hasOptionalPESHeader() || len(p.payload) < PESHeaderSize+3 {
		return nil
	}
	headerLength := int(p.payload[PESHeaderSize+2])
	offset := PESHeaderSize + 3 + headerLength
	if offset > len(p.payload) {
		return nil
	}
	return p.payload[offset:]
}

// startCode はエレメンタリーストリームの開始コード (00 00 01 または 00 00 00 01) 。
var startCodePattern = []byte{0x00, 0x00, 0x01}

// splitElementaryStream はエレメンタリーストリームを開始コード (00 00 01 / 00 00 00 01) で分割する。
// 移植元: biim.mpeg2ts.h264.H264PES の re.split(開始コードパターン, ...)
func splitElementaryStream(data []byte) [][]byte {
	parts := [][]byte{}
	position := 0
	for {
		index := bytes.Index(data[position:], startCodePattern)
		if index < 0 {
			// 残りを最後の要素として追加する
			if position < len(data) {
				parts = append(parts, data[position:])
			}
			break
		}
		index += position
		// 開始コードの手前までを 1 つの要素として追加する
		if index > position {
			parts = append(parts, data[position:index])
		}
		position = index + len(startCodePattern)
	}
	return parts
}

// hasKeyFrame は PES がセグメント開始に使えるキーフレームかを判定する。
// 移植元: TSKeyFrameSeeker.hasKeyFrame()
func hasKeyFrame(p *pes, codec string) bool {
	data := p.packetData()
	switch codec {
	case "H.264":
		for _, unit := range splitElementaryStream(data) {
			if len(unit) > 0 && unit[0]&0x1F == 0x05 {
				return true
			}
		}
		return false
	case "H.265":
		for _, unit := range splitElementaryStream(data) {
			if len(unit) == 0 {
				continue
			}
			// H.265 は BLA / IDR / CRA の NAL unit type 16〜21 をランダムアクセスフレームとして扱う
			unitType := (unit[0] >> 1) & 0x3F
			if unitType >= 16 && unitType <= 21 {
				return true
			}
		}
		return false
	case "MPEG-2":
		// MPEG-2 Video は picture_start_code 直後の picture_coding_type で I ピクチャを判定する
		markerIndex := bytes.Index(data, []byte{0x00, 0x00, 0x01, 0x00})
		if markerIndex < 0 || markerIndex+5 >= len(data) {
			return false
		}
		pictureCodingType := (data[markerIndex+5] >> 3) & 0x07
		return pictureCodingType == 1
	default:
		return false
	}
}

// pesParser は TS パケットから PES パケットを組み立てる。
// 移植元: biim.mpeg2ts.parser.PESParser (次の PES の先頭を見つけた時点で前の PES を返す)
type pesParser struct {
	pes   []byte
	queue []*pes
}

// newPESParser は PES パーサーを生成する。
func newPESParser() *pesParser {
	return &pesParser{}
}

// push は TS パケットを追加する。
func (p *pesParser) push(packet []byte) {
	begin := payloadOffset(packet)
	if !payloadUnitStartIndicator(packet) && p.pes == nil {
		return
	}
	if begin > len(packet) {
		return
	}

	if payloadUnitStartIndicator(packet) {
		// 直前の PES が PES_packet_length == 0 (可変長) の場合はここで確定する
		if p.pes != nil && len(p.pes) >= PESHeaderSize && binary.BigEndian.Uint16(p.pes[4:6]) == 0 {
			p.queue = append(p.queue, newPES(p.pes))
		}
		if begin+6 > len(packet) {
			return
		}
		pesLength := int(binary.BigEndian.Uint16(packet[begin+4 : begin+6]))
		next := PacketSize
		if pesLength != 0 {
			next = min(begin+PESHeaderSize+pesLength, PacketSize)
		}
		if next > len(packet) {
			next = len(packet)
		}
		p.pes = append([]byte{}, packet[begin:next]...)
	} else if p.pes != nil {
		if len(p.pes) < PESHeaderSize {
			return
		}
		pesLength := int(binary.BigEndian.Uint16(p.pes[4:6]))
		next := PacketSize
		if pesLength != 0 {
			next = min(begin+PESHeaderSize+pesLength-len(p.pes), PacketSize)
		}
		if next > len(packet) {
			next = len(packet)
		}
		if begin < next {
			p.pes = append(p.pes, packet[begin:next]...)
		}
	} else {
		return
	}

	if len(p.pes) < PESHeaderSize {
		return
	}
	pesLength := int(binary.BigEndian.Uint16(p.pes[4:6]))
	if pesLength > 0 {
		if len(p.pes) == PESHeaderSize+pesLength {
			p.queue = append(p.queue, newPES(p.pes))
			p.pes = nil
		} else if len(p.pes) > PESHeaderSize+pesLength {
			p.pes = nil
		}
	}
}

// pop は解析済みの PES を 1 つ取り出す。
func (p *pesParser) pop() (*pes, bool) {
	if len(p.queue) == 0 {
		return nil, false
	}
	result := p.queue[0]
	p.queue = p.queue[1:]
	return result, true
}

// flush は未確定の PES (PES_packet_length == 0 でファイル終端まで続くもの) を確定する。
func (p *pesParser) flush() {
	if p.pes != nil && len(p.pes) >= PESHeaderSize {
		p.queue = append(p.queue, newPES(p.pes))
		p.pes = nil
	}
}

// unwrapNear は 33bit の時刻を target に最も近い展開済み時刻へ寄せる。
// 移植元: TSKeyFrameSeeker.unwrapNear()
func unwrapNear(value int64, target int64) int64 {
	wrapCount := int64(math.RoundToEven(float64(target-value) / float64(PCRCycle)))
	return value + wrapCount*PCRCycle
}

// mpegCRC32 は MPEG-2 の CRC32 (poly 0x04C11DB7, init 0xFFFFFFFF, 反転なし, 最終 XOR なし) を計算する。
func mpegCRC32(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, value := range data {
		crc ^= uint32(value) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
