package videostream

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// mp4Box は MP4 box の位置とサイズを保持する。
// 移植元: MP4KeyFrameParser._MP4Box
type mp4Box struct {
	boxType    string
	offset     int64
	size       int64
	headerSize int64
}

// mp4TrackInfo は MP4 の映像トラックから同期サンプル DTS を復元するための情報。
// 移植元: MP4KeyFrameParser._MP4TrackInfo
type mp4TrackInfo struct {
	handlerType string
	timescale   int64
	sttsEntries [][2]int64
	stssSamples []int64
}

// ReadVideoKeyFrameDTS は MP4 の moov 内テーブルだけを読んで、映像同期サンプルの DTS を 90kHz 単位で返す。
// 移植元: MP4KeyFrameParser.readVideoKeyFrameDTS()
func ReadVideoKeyFrameDTS(path string) ([]int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat the recording file: %w", err)
	}
	fileSize := info.Size()
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open the recording file: %w", err)
	}
	defer func() { _ = file.Close() }()

	var moovBox *mp4Box
	for _, topLevelBox := range iterMP4Boxes(file, 0, fileSize) {
		if topLevelBox.boxType == "moov" {
			box := topLevelBox
			moovBox = &box
			break
		}
	}
	if moovBox == nil {
		return nil, fmt.Errorf("moov box was not found: %s", path)
	}

	// moov 直下の trak を順に調べ、映像トラックだけを DTS 生成対象にする
	for _, trackBox := range iterMP4Boxes(file, moovBox.offset+moovBox.headerSize, moovBox.offset+moovBox.size) {
		if trackBox.boxType != "trak" {
			continue
		}
		trackInfo, err := parseMP4Track(file, trackBox)
		if err != nil {
			return nil, err
		}
		if trackInfo.handlerType != "vide" {
			continue
		}
		if trackInfo.timescale == 0 {
			return nil, fmt.Errorf("video mdhd timescale was not found: %s", path)
		}
		if len(trackInfo.sttsEntries) == 0 {
			return nil, fmt.Errorf("video stts was not found: %s", path)
		}

		// stss がない映像トラックは、MP4 仕様上すべてのサンプルを同期サンプルとして扱う
		syncSamples := trackInfo.stssSamples
		if len(syncSamples) == 0 {
			var totalSampleCount int64
			for _, entry := range trackInfo.sttsEntries {
				totalSampleCount += entry[0]
			}
			syncSamples = make([]int64, 0, totalSampleCount)
			for sampleNumber := int64(1); sampleNumber <= totalSampleCount; sampleNumber++ {
				syncSamples = append(syncSamples, sampleNumber)
			}
		}

		syncSampleIndex := 0
		sampleNumber := int64(1)
		sampleDTS := int64(0)
		keyframeDTSList := []int64{}
		for _, entry := range trackInfo.sttsEntries {
			sampleCount, sampleDelta := entry[0], entry[1]
			// stts は同じ時間差のサンプルをまとめて持つため、現在のまとまりに含まれる最終サンプル番号を計算する
			entryLastSample := sampleNumber + sampleCount - 1
			for syncSampleIndex < len(syncSamples) && syncSamples[syncSampleIndex] <= entryLastSample {
				// stss の同期サンプル番号を stts の累積 DTS に変換する
				syncSampleNumber := syncSamples[syncSampleIndex]
				syncSampleDTS := sampleDTS + (syncSampleNumber-sampleNumber)*sampleDelta
				keyframeDTSList = append(keyframeDTSList, syncSampleDTS*HZ/trackInfo.timescale)
				syncSampleIndex++
			}
			// 次の stts のまとまりの先頭サンプル番号と DTS に進める
			sampleNumber += sampleCount
			sampleDTS += sampleCount * sampleDelta
		}

		// psisimux の -m はファイル先頭からの相対ミリ秒指定なので、先頭同期サンプルを 0 に正規化する
		if len(keyframeDTSList) == 0 {
			return []int64{}, nil
		}
		baseDTS := keyframeDTSList[0]
		normalized := make([]int64, 0, len(keyframeDTSList))
		for _, dts := range keyframeDTSList {
			normalized = append(normalized, dts-baseDTS)
		}
		return normalized, nil
	}
	return nil, fmt.Errorf("video trak was not found: %s", path)
}

// FindKeyFrameDTSBefore は MP4 の同期サンプル DTS 一覧から、プレイリスト時刻以前の最も近い開始 DTS を選ぶ。
// 移植元: MP4KeyFrameParser.findKeyFrameDTSBefore()
func FindKeyFrameDTSBefore(keyframeDTSList []int64, playlistStartSeconds float64) (int64, error) {
	if len(keyframeDTSList) == 0 {
		return 0, fmt.Errorf("MP4 keyframe DTS list is empty")
	}
	// MP4 はファイル位置へシークせず、psisimux の -m へ渡す時刻だけを決める
	targetDTS := int64(roundHalfEven(playlistStartSeconds * float64(HZ)))
	keyframeIndex := sort.Search(len(keyframeDTSList), func(index int) bool {
		return keyframeDTSList[index] > targetDTS
	}) - 1
	if keyframeIndex < 0 {
		keyframeIndex = 0
	}
	return keyframeDTSList[keyframeIndex], nil
}

// readMP4BoxHeader は現在位置の MP4 box header を読み、payload の範囲を計算する。
// 移植元: MP4KeyFrameParser.__readMP4BoxHeader()
func readMP4BoxHeader(file *os.File, fileEnd int64) (*mp4Box, error) {
	boxOffset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if boxOffset+8 > fileEnd {
		return nil, nil
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, nil
	}
	size := int64(binary.BigEndian.Uint32(header[0:4]))
	boxType := string(header[4:8])
	headerSize := int64(8)

	if size == 1 {
		extendedSize := make([]byte, 8)
		if _, err := io.ReadFull(file, extendedSize); err != nil {
			return nil, nil
		}
		size = int64(binary.BigEndian.Uint64(extendedSize))
		headerSize = 16
	}
	// size 0 は親 box の終端まで続く特殊値なので、呼び出し側から渡された境界で長さを決める
	if size == 0 {
		size = fileEnd - boxOffset
	}
	if size < headerSize || boxOffset+size > fileEnd {
		return nil, nil
	}
	return &mp4Box{boxType: boxType, offset: boxOffset, size: size, headerSize: headerSize}, nil
}

// iterMP4Boxes は指定範囲の直下にある MP4 box 一覧を読む。
// 移植元: MP4KeyFrameParser.__iterMP4Boxes()
func iterMP4Boxes(file *os.File, startOffset int64, endOffset int64) []mp4Box {
	boxes := []mp4Box{}
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return boxes
	}
	for {
		position, err := file.Seek(0, io.SeekCurrent)
		if err != nil || position+8 > endOffset {
			break
		}
		box, err := readMP4BoxHeader(file, endOffset)
		if err != nil || box == nil {
			break
		}
		boxes = append(boxes, *box)
		if _, err := file.Seek(box.offset+box.size, io.SeekStart); err != nil {
			break
		}
	}
	return boxes
}

// findMP4ChildBox は親 box 直下から指定 type の子 box を探す。
// 移植元: MP4KeyFrameParser.__findMP4ChildBox()
func findMP4ChildBox(file *os.File, parentBox mp4Box, boxType string) *mp4Box {
	for _, childBox := range iterMP4Boxes(file, parentBox.offset+parentBox.headerSize, parentBox.offset+parentBox.size) {
		if childBox.boxType == boxType {
			box := childBox
			return &box
		}
	}
	return nil
}

// readMP4Payload は MP4 box の payload を読む。
// 移植元: MP4KeyFrameParser.__readMP4Payload()
func readMP4Payload(file *os.File, box mp4Box) []byte {
	if _, err := file.Seek(box.offset+box.headerSize, io.SeekStart); err != nil {
		return nil
	}
	payload := make([]byte, box.size-box.headerSize)
	if _, err := io.ReadFull(file, payload); err != nil {
		return nil
	}
	return payload
}

// parseMP4Track は MP4 の trak box から映像トラック判定とキーフレーム時刻生成に必要な表を読む。
// 移植元: MP4KeyFrameParser.__parseMP4Track()
func parseMP4Track(file *os.File, trackBox mp4Box) (*mp4TrackInfo, error) {
	trackInfo := &mp4TrackInfo{}

	// trak の中身は mdia 配下にまとまっているため、mdia がないトラックは空情報として扱う
	mdiaBox := findMP4ChildBox(file, trackBox, "mdia")
	if mdiaBox == nil {
		return trackInfo, nil
	}
	// hdlr の handler_type で映像トラックかどうかを後段で判定する
	if hdlrBox := findMP4ChildBox(file, *mdiaBox, "hdlr"); hdlrBox != nil {
		hdlrPayload := readMP4Payload(file, *hdlrBox)
		if len(hdlrPayload) >= 12 {
			trackInfo.handlerType = string(hdlrPayload[8:12])
		}
	}
	// mdhd の timescale は stts の時間単位を 90kHz DTS へ換算するときに使う
	if mdhdBox := findMP4ChildBox(file, *mdiaBox, "mdhd"); mdhdBox != nil {
		mdhdPayload := readMP4Payload(file, *mdhdBox)
		if len(mdhdPayload) >= 24 {
			version := mdhdPayload[0]
			timescaleOffset := 12
			if version == 1 {
				timescaleOffset = 20
			}
			if len(mdhdPayload) >= timescaleOffset+4 {
				trackInfo.timescale = int64(binary.BigEndian.Uint32(mdhdPayload[timescaleOffset : timescaleOffset+4]))
			}
		}
	}
	minfBox := findMP4ChildBox(file, *mdiaBox, "minf")
	var stblBox *mp4Box
	if minfBox != nil {
		stblBox = findMP4ChildBox(file, *minfBox, "stbl")
	}
	if stblBox == nil {
		return trackInfo, nil
	}

	// stts はサンプル数と時間差のランレングス表で、全サンプルの DTS を復元する元データになる
	if sttsBox := findMP4ChildBox(file, *stblBox, "stts"); sttsBox != nil {
		sttsPayload := readMP4Payload(file, *sttsBox)
		if len(sttsPayload) >= 8 {
			entryCount := int64(binary.BigEndian.Uint32(sttsPayload[4:8]))
			for entryIndex := range entryCount {
				entryOffset := 8 + entryIndex*8
				if entryOffset+8 > int64(len(sttsPayload)) {
					break
				}
				sampleCount := int64(binary.BigEndian.Uint32(sttsPayload[entryOffset : entryOffset+4]))
				sampleDelta := int64(binary.BigEndian.Uint32(sttsPayload[entryOffset+4 : entryOffset+8]))
				trackInfo.sttsEntries = append(trackInfo.sttsEntries, [2]int64{sampleCount, sampleDelta})
			}
		}
	}
	// stss は同期サンプル番号の一覧で、存在しない場合は上位側で全サンプルを同期サンプルとして扱う
	if stssBox := findMP4ChildBox(file, *stblBox, "stss"); stssBox != nil {
		stssPayload := readMP4Payload(file, *stssBox)
		if len(stssPayload) >= 8 {
			entryCount := int64(binary.BigEndian.Uint32(stssPayload[4:8]))
			for entryIndex := range entryCount {
				sampleOffset := 8 + entryIndex*4
				if sampleOffset+4 > int64(len(stssPayload)) {
					break
				}
				trackInfo.stssSamples = append(trackInfo.stssSamples, int64(binary.BigEndian.Uint32(stssPayload[sampleOffset:sampleOffset+4])))
			}
		}
	}
	return trackInfo, nil
}
