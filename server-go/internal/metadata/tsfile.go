package metadata

import (
	"crypto/md5"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// 録画ファイル解析の定数 (MetadataAnalyzer)。
const (
	// maxStreamScanBytes は TS の映像ストリーム変化検出で各サンプル位置から読み込む最大バイト数。
	maxStreamScanBytes = 8 * 1024 * 1024
	// videoStreamChangeSampleRatio は映像ストリーム変化検出に使うサンプル比率。
	// (先頭 / 本編付近 / 末尾付近)
	// 末尾のみ固定バイト数を読む。
	excludingZeroBlockSize = 4096
)

// hashChunkSize とハッシュチャンク数 (MetadataAnalyzer.__calculateFileHash) 。
const (
	hashChunkSize = 1024 * 1024
	hashNumChunks = 3
)

// closestMultiple は n に最も近い multiple の倍数を返す (app.utils.ClosestMultiple 相当) 。
// Python の round() と同じく偶数丸め (banker's rounding) を使う。
func closestMultiple(n int64, multiple int64) int64 {
	return int64(math.RoundToEven(float64(n)/float64(multiple))) * multiple
}

// calculateTSFileDuration は TS ファイルの先頭と末尾の有効な PCR から再生時間と有効データ終了位置を算出する。
// 移植元: MetadataAnalyzer.__calculateTSFileDuration()
func calculateTSFileDuration(path string, searchBlockSize int64) (float64, int64, bool) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	fileSize := fileInfo.Size()
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()

	// 先頭ブロックからの PCR 抽出
	headData := make([]byte, searchBlockSize)
	read, _ := io.ReadFull(file, headData)
	headData = headData[:read]
	if len(headData) > 0 && headData[0] != videostream.SyncByte {
		correctedOffset := -1
		for index, value := range headData {
			if value == videostream.SyncByte {
				correctedOffset = index
				break
			}
		}
		if correctedOffset < 0 {
			return 0, 0, false
		}
		headData = headData[correctedOffset:]
	}

	var firstTimestamp *float64
	for index := 0; index+videostream.PacketSize <= len(headData); index += videostream.PacketSize {
		packet := headData[index : index+videostream.PacketSize]
		if packet[0] != videostream.SyncByte {
			continue
		}
		if pcr, ok := readPCRValue(packet); ok {
			value := float64(pcr) / float64(videostream.HZ)
			firstTimestamp = &value
			break
		}
	}
	if firstTimestamp == nil {
		return 0, 0, false
	}

	// 末尾のゼロ埋め領域の境界をバイナリサーチで検出
	blockCheckSize := int64(excludingZeroBlockSize)
	low, high := int64(0), fileSize
	zeroBoundary := fileSize
	for low <= high {
		mid := (low + high) / 2
		if _, err := file.Seek(mid, io.SeekStart); err != nil {
			return 0, 0, false
		}
		candidate := make([]byte, blockCheckSize)
		candidateRead, _ := io.ReadFull(file, candidate)
		candidate = candidate[:candidateRead]
		allZero := len(candidate) > 0
		for _, value := range candidate {
			if value != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			zeroBoundary = mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	validDataEnd := fileSize
	if zeroBoundary < fileSize {
		validDataEnd = zeroBoundary
	}

	// 末尾領域から最後の有効な PCR を取得
	startOffset := validDataEnd - searchBlockSize
	if startOffset < 0 {
		startOffset = 0
	}
	startOffset = (startOffset / videostream.PacketSize) * videostream.PacketSize
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return 0, 0, false
	}
	tailChunk := make([]byte, validDataEnd-startOffset)
	tailRead, _ := io.ReadFull(file, tailChunk)
	tailChunk = tailChunk[:tailRead]

	offsetInChunk := 0
	if len(tailChunk) > 0 && tailChunk[0] != videostream.SyncByte {
		for index, value := range tailChunk {
			if value == videostream.SyncByte {
				offsetInChunk = index
				break
			}
		}
	}

	validPCRs := []float64{}
	for index := offsetInChunk; index+videostream.PacketSize <= len(tailChunk); index += videostream.PacketSize {
		packet := tailChunk[index : index+videostream.PacketSize]
		if packet[0] != videostream.SyncByte {
			continue
		}
		if pcr, ok := readPCRValue(packet); ok {
			validPCRs = append(validPCRs, float64(pcr)/float64(videostream.HZ))
		}
	}
	if len(validPCRs) == 0 {
		return 0, 0, false
	}
	lastTimestamp := validPCRs[len(validPCRs)-1]

	// PCR ラップアラウンドの補正
	if lastTimestamp < *firstTimestamp {
		lastTimestamp += float64(videostream.PCRCycle) / float64(videostream.HZ)
	}
	return lastTimestamp - *firstTimestamp, validDataEnd, true
}

// readPCRValue は TS パケットのアダプテーションフィールドから PCR (33bit, 90kHz) を読み取る。
// 移植元: biim.mpeg2ts の ts.pcr()
func readPCRValue(packet []byte) (int64, bool) {
	if len(packet) < videostream.PacketSize {
		return 0, false
	}
	hasAdaptationField := packet[3]&0x20 != 0
	if !hasAdaptationField || packet[4] == 0 || packet[videostream.HeaderSize+1]&0x10 == 0 {
		return 0, false
	}
	offset := videostream.HeaderSize + 1 + 1
	var pcrBase int64
	for index := range 4 {
		pcrBase = (pcrBase << 8) | int64(packet[offset+index])
	}
	pcrBase = (pcrBase << 1) | int64((packet[offset+4]&0x80)>>7)
	return pcrBase, true
}

// calculateFileHash は録画ファイルの複数箇所 (1/4, 2/4, 3/4 位置) の MD5 を計算する。
// 移植元: MetadataAnalyzer.__calculateFileHash()
func calculateFileHash(path string, endTSOffset *int64) (string, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	fileSize := fileInfo.Size()
	effectiveSize := fileSize
	if endTSOffset != nil && *endTSOffset > 0 && *endTSOffset < fileSize {
		effectiveSize = *endTSOffset
	}
	if effectiveSize < hashChunkSize*hashNumChunks {
		return "", fmt.Errorf("file size must be at least %d bytes", hashChunkSize*hashNumChunks)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := md5.New()
	buffer := make([]byte, hashChunkSize)
	for chunkIndex := range hashNumChunks {
		offset := (effectiveSize / (hashNumChunks + 1)) * int64(chunkIndex+1)
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		remaining := effectiveSize - offset
		if remaining <= 0 {
			break
		}
		readSize := min(int64(hashChunkSize), remaining)
		read, err := io.ReadFull(file, buffer[:readSize])
		if read > 0 {
			hash.Write(buffer[:read])
		}
		if err != nil {
			break
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// detectTSVideoStreamChanges は録画 TS 内で映像 PID や映像コーデックが変化しているかを検出する。
// 移植元: MetadataAnalyzer.__detectTSVideoStreamChanges()
func detectTSVideoStreamChanges(path string, endTSOffset *int64) bool {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	fileSize := fileInfo.Size()
	effectiveSize := fileSize
	if endTSOffset != nil && *endTSOffset < fileSize {
		effectiveSize = *endTSOffset
	}
	if effectiveSize < videostream.PacketSize*100 {
		return false
	}

	type streamSample struct {
		videoPID int
		codec    string
	}
	samples := []streamSample{}
	selectedOffsets := map[int64]bool{}
	for _, sampleRatio := range []float64{0.0, 0.25, 0.98} {
		var sampleOffset int64
		if sampleRatio == 0.98 {
			base := effectiveSize - maxStreamScanBytes
			if base < 0 {
				base = 0
			}
			sampleOffset = closestMultiple(base, videostream.PacketSize)
		} else {
			sampleOffset = closestMultiple(int64(float64(effectiveSize)*sampleRatio), videostream.PacketSize)
		}
		if sampleOffset > effectiveSize-videostream.PacketSize {
			sampleOffset = closestMultiple(max(effectiveSize-videostream.PacketSize, 0), videostream.PacketSize)
		}
		if sampleOffset > effectiveSize-videostream.PacketSize {
			sampleOffset = max(sampleOffset-videostream.PacketSize, 0)
		}
		if selectedOffsets[sampleOffset] {
			continue
		}
		selectedOffsets[sampleOffset] = true
		streamInfo, err := videostream.FindStreamInfo(path, sampleOffset, maxStreamScanBytes)
		if err != nil {
			// PMT がサンプル範囲で拾えない TS でもメタデータ解析自体は継続する
			continue
		}
		samples = append(samples, streamSample{videoPID: streamInfo.VideoPID, codec: streamInfo.Codec})
	}
	if len(samples) < 2 {
		return false
	}
	base := samples[0]
	for _, sample := range samples[1:] {
		if sample.videoPID != base.videoPID || sample.codec != base.codec {
			return true
		}
	}
	return false
}

// readFirstSyncByteIsValid は録画ファイルの先頭 1 バイトが TS の sync_byte かどうかを返す。
// 移植元: MetadataAnalyzer.analyze() の sync_byte チェック
func readFirstSyncByteIsValid(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	buffer := make([]byte, 1)
	if _, err := io.ReadFull(file, buffer); err != nil {
		// 空ファイルは Python 側では IndexError → 解析失敗扱いになる
		return false, fmt.Errorf("failed to read the first byte: %w", err)
	}
	return buffer[0] == videostream.SyncByte, nil
}

// extractSampleData は録画ファイルの 25% 位置から 30 秒程度 (18Mbps 想定) のデータを切り出す。
// 移植元: MetadataAnalyzer.__analyzeFFprobe() のサンプル切り出し処理
func extractSampleData(path string) ([]byte, *int64, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	fileSize := fileInfo.Size()
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	offset := closestMultiple(int64(float64(fileSize)*0.25), videostream.PacketSize)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	sampleSize := closestMultiple(18*1024*1024*30/8, videostream.PacketSize)
	sampleData := make([]byte, sampleSize)
	read, _ := io.ReadFull(file, sampleData)
	sampleData = sampleData[:read]

	if isAllZero(sampleData) {
		// ゼロ埋め領域の境界を取得するためフォールバックの尺計算を実行する
		_, endTSOffset, ok := calculateTSFileDuration(path, 1024*1024)
		if !ok {
			return nil, nil, fmt.Errorf("failed to calculate duration")
		}
		offset = closestMultiple(int64(float64(endTSOffset)*0.25), videostream.PacketSize)
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, nil, err
		}
		sampleSize = min(sampleSize, endTSOffset-offset)
		if sampleSize < 0 {
			sampleSize = 0
		}
		sampleData = make([]byte, sampleSize)
		read, _ = io.ReadFull(file, sampleData)
		sampleData = sampleData[:read]
		return sampleData, &endTSOffset, nil
	}
	return sampleData, nil, nil
}

// isAllZero はバイト列が全て 0 かどうかを返す (空の場合は False = Python の all() と同じ) 。
func isAllZero(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
