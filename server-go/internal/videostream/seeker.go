package videostream

import (
	"fmt"
	"io"
	"os"
)

// 探索時の定数 (移植元: TSKeyFrameSeeker) 。
const (
	// PCRSearchWindowBytes は PCR 探索のウィンドウサイズ。
	PCRSearchWindowBytes = 2 * 1024 * 1024
	// KeyframeBacktrackBytes はキーフレーム探索で遡るバイト数。
	KeyframeBacktrackBytes = 8 * 1024 * 1024
	// MaxKeyframeScanBytes はキーフレーム探索の最大スキャンバイト数。
	MaxKeyframeScanBytes = 96 * 1024 * 1024
)

// StreamInfo は TS コンテナ内の映像 PID と PCR PID をまとめて保持する。
// 移植元: TSKeyFrameSeeker.TSStreamInfo
type StreamInfo struct {
	// VideoPID は映像 PES が流れる PID。
	VideoPID int
	// PCRPID は PCR が流れる PID。
	PCRPID int
	// Codec は映像コーデック (MPEG-2 / H.264 / H.265) 。
	Codec string
	// PacketSize はファイル上の TS パケットサイズ (188 または 192) 。
	PacketSize int
}

// KeyFramePosition は録画ファイル内でエンコードを開始する位置と時刻を表す。
// 移植元: TSKeyFrameSeeker.TSKeyFramePosition
type KeyFramePosition struct {
	// SourceFilePosition は入力ファイルのバイト位置。
	SourceFilePosition int64
	// SourceStartDTS は入力ソース上の開始 DTS (90kHz) 。
	SourceStartDTS int64
}

// FindStreamInfo は PAT / PMT から映像 PID と PCR PID を取得する。
// 移植元: TSKeyFrameSeeker.findStreamInfo()
func FindStreamInfo(path string, startOffset int64, maxScanBytes int64) (*StreamInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open the recording file: %w", err)
	}
	defer func() { _ = file.Close() }()

	head := make([]byte, 8192)
	read, _ := io.ReadFull(file, head)
	packetSize := detectPacketSize(head[:read])

	patParser := newSectionParser()
	pmtParser := newSectionParser()
	pmtPID := -1
	alignedStartOffset := max(int64(0), (startOffset/int64(packetSize))*int64(packetSize))
	maxPacketCount := int64(300000)
	if maxScanBytes > 0 {
		maxPacketCount = max(int64(1), maxScanBytes/int64(packetSize))
	}

	if _, err := file.Seek(alignedStartOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek the recording file: %w", err)
	}
	packet := make([]byte, packetSize)
	for range maxPacketCount {
		if _, err := io.ReadFull(file, packet); err != nil {
			break
		}
		normalized, ok := normalizePacket(packet, packetSize)
		if !ok {
			break
		}
		currentPID := pid(normalized)
		if currentPID == 0x00 {
			patParser.push(normalized)
			for {
				section, ok := patParser.pop()
				if !ok {
					break
				}
				// CRC が不正なセクションは無視する
				if mpegCRC32(section) != 0 {
					continue
				}
				// PAT から最初のプログラムの PMT PID を取得する
				for _, entry := range parsePAT(section) {
					if entry.programNumber != 0 {
						pmtPID = entry.pid
						break
					}
				}
			}
		} else if pmtPID >= 0 && currentPID == pmtPID {
			pmtParser.push(normalized)
			for {
				section, ok := pmtParser.pop()
				if !ok {
					break
				}
				if mpegCRC32(section) != 0 {
					continue
				}
				pcrPID, streams := parsePMT(section)
				for _, stream := range streams {
					switch stream.streamType {
					case 0x02:
						return &StreamInfo{VideoPID: stream.pid, PCRPID: pcrPID, Codec: "MPEG-2", PacketSize: packetSize}, nil
					case 0x1B:
						return &StreamInfo{VideoPID: stream.pid, PCRPID: pcrPID, Codec: "H.264", PacketSize: packetSize}, nil
					case 0x24:
						return &StreamInfo{VideoPID: stream.pid, PCRPID: pcrPID, Codec: "H.265", PacketSize: packetSize}, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("video stream information was not found: %s", path)
}

// FindBaseDTS は TS コンテナ内の最初のキーフレーム DTS を取得する。
// 移植元: TSKeyFrameSeeker.findBaseDTS()
func FindBaseDTS(path string, streamInfo *StreamInfo) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("failed to open the recording file: %w", err)
	}
	defer func() { _ = file.Close() }()

	parser := newPESParser()
	var pendingPESStart int64 = -1
	scannedBytes := int64(0)
	packet := make([]byte, streamInfo.PacketSize)
	var fileOffset int64
	for scannedBytes < MaxKeyframeScanBytes {
		fileOffset += int64(streamInfo.PacketSize)
		if _, err := io.ReadFull(file, packet); err != nil {
			break
		}
		scannedBytes += int64(streamInfo.PacketSize)
		normalized, ok := normalizePacket(packet, streamInfo.PacketSize)
		if !ok {
			break
		}
		if pid(normalized) != streamInfo.VideoPID {
			continue
		}
		currentPESStart := int64(-1)
		if payloadUnitStartIndicator(normalized) {
			currentPESStart = fileOffset - int64(streamInfo.PacketSize)
		}
		parser.push(normalized)
		for {
			current, ok := parser.pop()
			if !ok {
				break
			}
			pesStart := pendingPESStart
			if currentPESStart >= 0 {
				pendingPESStart = currentPESStart
			}
			if pesStart < 0 {
				continue
			}
			rawDTS, ok := pesTimestamp(current)
			if !ok {
				continue
			}
			if hasKeyFrame(current, streamInfo.Codec) {
				return rawDTS, nil
			}
		}
		if currentPESStart >= 0 && pendingPESStart != currentPESStart {
			pendingPESStart = currentPESStart
		}
	}
	return 0, fmt.Errorf("first keyframe DTS was not found: %s", path)
}

// pesTimestamp は PES の DTS (なければ PTS) を返す。
func pesTimestamp(p *pes) (int64, bool) {
	if value, ok := p.dts(); ok {
		return value, true
	}
	return p.pts()
}

// Seek は TS コンテナで、プレイリスト時刻以前の最も近いキーフレームを探索する。
// 移植元: TSKeyFrameSeeker.seek()
func Seek(
	path string,
	streamInfo *StreamInfo,
	playlistStartSeconds float64,
	sourceBaseDTS int64,
	maxKeyframeAgeTicks int64,
) (*KeyFramePosition, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat the recording file: %w", err)
	}
	fileSize := info.Size()
	targetDTS := sourceBaseDTS + int64(roundHalfEven(playlistStartSeconds*float64(HZ)))
	pcrOffset := findOffsetByPCRBinarySearch(path, streamInfo, playlistStartSeconds, fileSize)

	keyframe, err := resolveKeyFrameNearOffset(path, streamInfo, targetDTS, pcrOffset, maxKeyframeAgeTicks)
	if err != nil {
		return nil, err
	}

	// 通常はファイル先頭の PAT / PMT で取得した映像 PID のまま探索できる
	// 録画マージンでマルチ編成が切り替わる TS では PID が変わるため、失敗時だけ局所 PMT を読み直す
	if keyframe == nil {
		localStreamInfo, err := FindStreamInfo(path, pcrOffset, PCRSearchWindowBytes*2)
		if err == nil {
			keyframe, _ = resolveKeyFrameNearOffset(path, localStreamInfo, targetDTS, pcrOffset, maxKeyframeAgeTicks)
		}
	}
	if keyframe == nil {
		return nil, fmt.Errorf("keyframe was not found near requested time: %s", path)
	}
	return &KeyFramePosition{
		SourceFilePosition: keyframe.offset,
		SourceStartDTS:     keyframe.dts,
	}, nil
}

// keyFrameScanResult は TS コンテナの前方スキャンで見つかったキーフレーム情報。
// 移植元: TSKeyFrameSeeker._KeyFrameScanResult
type keyFrameScanResult struct {
	offset               int64
	dts                  int64
	scanBytes            int64
	hasConfirmedBoundary bool
}

// findKeyFrameBefore は targetDTS 直前のキーフレームを推定位置付近から探索する。
// 移植元: TSKeyFrameSeeker.__findKeyFrameBefore()
func findKeyFrameBefore(
	path string,
	streamInfo *StreamInfo,
	targetDTS int64,
	startOffset int64,
	maxScanBytes int64,
) (*keyFrameScanResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open the recording file: %w", err)
	}
	defer func() { _ = file.Close() }()

	alignedStartOffset := max(int64(0), (startOffset/int64(streamInfo.PacketSize))*int64(streamInfo.PacketSize))
	if _, err := file.Seek(alignedStartOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek the recording file: %w", err)
	}

	parser := newPESParser()
	var pendingPESStart int64 = -1
	var lastKeyframeBeforeOffset, lastKeyframeBeforeDTS int64 = -1, 0
	scannedBytes := int64(0)
	packet := make([]byte, streamInfo.PacketSize)
	fileOffset := alignedStartOffset
	for scannedBytes < maxScanBytes {
		if _, err := io.ReadFull(file, packet); err != nil {
			break
		}
		currentFileOffset := fileOffset
		fileOffset += int64(streamInfo.PacketSize)
		scannedBytes += int64(streamInfo.PacketSize)
		normalized, ok := normalizePacket(packet, streamInfo.PacketSize)
		if !ok {
			break
		}
		if pid(normalized) != streamInfo.VideoPID {
			continue
		}
		currentPESStart := int64(-1)
		if payloadUnitStartIndicator(normalized) {
			currentPESStart = currentFileOffset
		}
		parser.push(normalized)
		for {
			current, ok := parser.pop()
			if !ok {
				break
			}
			pesStart := pendingPESStart
			if currentPESStart >= 0 {
				pendingPESStart = currentPESStart
			}
			if pesStart < 0 {
				continue
			}
			rawDTS, ok := pesTimestamp(current)
			if !ok {
				continue
			}
			sourceDTS := unwrapNear(rawDTS, targetDTS)
			if !hasKeyFrame(current, streamInfo.Codec) {
				continue
			}
			if sourceDTS <= targetDTS {
				lastKeyframeBeforeOffset = pesStart
				lastKeyframeBeforeDTS = sourceDTS
				continue
			}
			if lastKeyframeBeforeOffset >= 0 {
				return &keyFrameScanResult{
					offset:               lastKeyframeBeforeOffset,
					dts:                  lastKeyframeBeforeDTS,
					scanBytes:            scannedBytes,
					hasConfirmedBoundary: true,
				}, nil
			}
			// 最初に見つかったキーフレームが targetDTS より後ろなら、この探索窓は開始位置として使わない
			return nil, nil
		}
		if currentPESStart >= 0 && pendingPESStart != currentPESStart {
			pendingPESStart = currentPESStart
		}
	}
	if lastKeyframeBeforeOffset >= 0 {
		return &keyFrameScanResult{
			offset:               lastKeyframeBeforeOffset,
			dts:                  lastKeyframeBeforeDTS,
			scanBytes:            scannedBytes,
			hasConfirmedBoundary: false,
		}, nil
	}
	return nil, nil
}

// readFirstPCRNear は指定位置以降で最初に見つかった PCR を返す。
// 移植元: TSKeyFrameSeeker.__readFirstPCRNear()
func readFirstPCRNear(path string, streamInfo *StreamInfo, startOffset int64, maxScanBytes int64) (int64, int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()

	alignedOffset := max(int64(0), (startOffset/int64(streamInfo.PacketSize))*int64(streamInfo.PacketSize))
	if _, err := file.Seek(alignedOffset, io.SeekStart); err != nil {
		return 0, 0, false
	}
	scannedBytes := int64(0)
	packet := make([]byte, streamInfo.PacketSize)
	fileOffset := alignedOffset
	for scannedBytes < maxScanBytes {
		if _, err := io.ReadFull(file, packet); err != nil {
			return 0, 0, false
		}
		currentFileOffset := fileOffset
		fileOffset += int64(streamInfo.PacketSize)
		scannedBytes += int64(streamInfo.PacketSize)
		normalized, ok := normalizePacket(packet, streamInfo.PacketSize)
		if !ok {
			continue
		}
		if pid(normalized) == streamInfo.PCRPID {
			if value, ok := readPCR(normalized); ok {
				return currentFileOffset, value, true
			}
		}
	}
	return 0, 0, false
}

// findOffsetByPCRBinarySearch は PCR の単調増加を利用してプレイリスト時刻直前のファイル位置を二分探索する。
// 移植元: TSKeyFrameSeeker.__findOffsetByPCRBinarySearch()
func findOffsetByPCRBinarySearch(
	path string,
	streamInfo *StreamInfo,
	playlistStartSeconds float64,
	fileSize int64,
) int64 {
	firstOffset, firstPCR, ok := readFirstPCRNear(path, streamInfo, 0, PCRSearchWindowBytes)
	if !ok {
		return 0
	}
	targetPCR := firstPCR + int64(roundHalfEven(playlistStartSeconds*float64(HZ)))
	lo := firstOffset
	hi := max(firstOffset, fileSize-int64(streamInfo.PacketSize))
	for hi-lo > PCRSearchWindowBytes {
		mid := ((lo + hi) / 2 / int64(streamInfo.PacketSize)) * int64(streamInfo.PacketSize)
		_, midPCR, ok := readFirstPCRNear(path, streamInfo, mid, PCRSearchWindowBytes)
		if !ok {
			lo = mid
			continue
		}
		if unwrapNear(midPCR, targetPCR) <= targetPCR {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// resolveKeyFrameNearOffset は PCR で絞り込んだ位置から短い PES 前方スキャンを行い、目標時刻直前のキーフレームを解決する。
// 移植元: TSKeyFrameSeeker.__resolveKeyFrameNearOffset()
func resolveKeyFrameNearOffset(
	path string,
	streamInfo *StreamInfo,
	targetDTS int64,
	pcrOffset int64,
	maxKeyframeAgeTicks int64,
) (*keyFrameScanResult, error) {
	scanAttempts := [][2]int64{
		{KeyframeBacktrackBytes, MaxKeyframeScanBytes},
		{KeyframeBacktrackBytes * 4, MaxKeyframeScanBytes * 2},
	}
	totalScanBytes := int64(0)
	var lastResult *keyFrameScanResult
	for _, attempt := range scanAttempts {
		// PCR は映像 PES と別 PID で流れるため、少し手前から映像 PES を読んで直前キーフレームを探す
		scanStartOffset := max(int64(0), pcrOffset-attempt[0])
		result, err := findKeyFrameBefore(path, streamInfo, targetDTS, scanStartOffset, attempt[1])
		if err != nil {
			return nil, err
		}
		if result == nil {
			continue
		}
		totalScanBytes += result.scanBytes
		lastResult = result
		if result.hasConfirmedBoundary {
			return &keyFrameScanResult{
				offset:               result.offset,
				dts:                  result.dts,
				scanBytes:            totalScanBytes,
				hasConfirmedBoundary: true,
			}, nil
		}
	}
	if lastResult != nil {
		// 目標時刻を越えるキーフレームまで読めなかった場合でも、直前キーフレームが十分近ければ開始位置として使う
		keyframeAgeTicks := targetDTS - lastResult.dts
		if keyframeAgeTicks < 0 || keyframeAgeTicks >= maxKeyframeAgeTicks {
			return nil, nil
		}
		return &keyFrameScanResult{
			offset:               lastResult.offset,
			dts:                  lastResult.dts,
			scanBytes:            totalScanBytes,
			hasConfirmedBoundary: lastResult.hasConfirmedBoundary,
		}, nil
	}
	return nil, nil
}

// roundHalfEven は Python の round() と同じ銀行丸めを行う。
func roundHalfEven(value float64) float64 {
	return mathRoundToEven(value)
}
