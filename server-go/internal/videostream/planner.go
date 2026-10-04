package videostream

import (
	"math"
	"math/big"
	"sort"
)

// ComputeSegmentDurationSeconds は録画のフレームレートから、約 6 秒に最も近い整数フレーム長を秒数へ変換する。
// 移植元: VideoSegmentPlanner.computeSegmentDurationSeconds()
func ComputeSegmentDurationSeconds(videoFrameRate float64) float64 {
	// メタデータが壊れている録画でもプレイリスト生成を止めず、従来値の 6 秒で扱う
	if videoFrameRate <= 0 {
		return 6.0
	}
	frameRate := normalizeFrameRate(videoFrameRate)
	// フレームレートは有理数のため、有理数のまま整数フレーム長へ丸める
	segmentFrameCount := roundRational(new(big.Rat).Mul(frameRate, big.NewRat(6, 1)))
	if segmentFrameCount == 0 {
		segmentFrameCount = 1
	}
	// フレーム数をフレームレートで割る (Python 版の Fraction / Fraction と同じ厳密な計算)
	result := new(big.Rat).Quo(big.NewRat(segmentFrameCount, 1), frameRate)
	value, _ := result.Float64()
	return value
}

// normalizeFrameRate は DB に保存済みの小数フレームレートを、代表的な放送系の有理数へ戻す。
// 移植元: VideoSegmentPlanner.__normalizeFrameRate()
func normalizeFrameRate(videoFrameRate float64) *big.Rat {
	// MetadataAnalyzer は FFprobe の分数値を float に変換して保存するため、
	// DB 上では 30000/1001 が 29.97 のように丸められている
	knownRates := [][2]int64{
		{60000, 1001}, {30000, 1001}, {24000, 1001}, {15000, 1001},
		{60, 1}, {50, 1}, {30, 1}, {25, 1}, {24, 1}, {15, 1},
	}
	const tolerance = 0.05
	for _, rate := range knownRates {
		rational := big.NewRat(rate[0], rate[1])
		value, _ := rational.Float64()
		if math.Abs(value-videoFrameRate) <= tolerance {
			return rational
		}
	}
	// 既知のフレームレートに一致しない場合は、分母を 100000 以下に制限した有理数で近似する
	return limitDenominator(videoFrameRate, 100000)
}

// limitDenominator は浮動小数点数を分母が maxDenominator 以下の有理数で近似する。
// 移植元: fractions.Fraction(value).limit_denominator()
func limitDenominator(value float64, maxDenominator int64) *big.Rat {
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return big.NewRat(int64(value), 1)
	}
	// 連分数展開で近似分数を求める (Python の limit_denominator と同じアルゴリズム)
	sign := int64(1)
	if value < 0 {
		sign = -1
		value = -value
	}
	p0, q0 := int64(0), int64(1)
	p1, q1 := int64(1), int64(0)
	remainder := value
	for range 100 {
		integer := math.Floor(remainder)
		integerValue := int64(integer)
		p2 := integerValue*p1 + p0
		q2 := integerValue*q1 + q0
		if q2 > maxDenominator {
			break
		}
		p0, q0, p1, q1 = p1, q1, p2, q2
		remainder = 1 / (remainder - integer)
		if math.IsInf(remainder, 0) || math.IsNaN(remainder) {
			break
		}
	}
	if q1 == 0 {
		return big.NewRat(sign, 1)
	}
	return big.NewRat(sign*p1, q1)
}

// roundRational は有理数を Python の round() (銀行丸め) で整数に丸める。
func roundRational(value *big.Rat) int64 {
	// 分子を分母で割った商と余りから、0.5 の扱いを決める
	quotient := new(big.Int).Quo(value.Num(), value.Denom())
	remainder := new(big.Int).Rem(value.Num(), value.Denom())
	// 2 * |remainder| と |denominator| を比較する
	twiceRemainder := new(big.Int).Abs(remainder)
	twiceRemainder.Lsh(twiceRemainder, 1)
	denominator := new(big.Int).Abs(value.Denom())
	comparison := twiceRemainder.Cmp(denominator)
	if comparison > 0 {
		// 0.5 より大きい場合は切り上げる
		if value.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	} else if comparison == 0 && quotient.Bit(0) == 1 {
		// ちょうど 0.5 の場合は偶数側に丸める
		if value.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	return quotient.Int64()
}

// KeyFrame は入力 TS から収集したキーフレーム (server/app/schemas.py の KeyFrame 相当) 。
type KeyFrame struct {
	// DTS はキーフレームの DTS (90kHz) 。
	DTS int64
	// Offset は PES 開始位置のファイルオフセット。
	Offset int64
}

// SegmentMapEntry は HLS シーケンス番号ごとの入力開始位置キャッシュ (server/app/schemas.py の SegmentMapEntry 相当) 。
type SegmentMapEntry struct {
	// SequenceIndex は HLS シーケンス番号。
	SequenceIndex int
	// SourceFilePosition は入力ファイルのバイト位置。
	SourceFilePosition int64
	// SourceStartDTS は入力ソース上の開始 DTS (90kHz) 。
	SourceStartDTS int64
}

// ConvertKeyFramesToSegmentMap は既存の key_frames から、オンデマンド解決と同じ規則の segment_map を生成する。
// 移植元: VideoSegmentPlanner.convertKeyFramesToSegmentMap()
func ConvertKeyFramesToSegmentMap(keyFrames []KeyFrame, videoFrameRate float64, durationSeconds float64) []SegmentMapEntry {
	// 旧 KeyFrameAnalyzer は末尾シーク用の番兵を追加するため、最後の要素は実キーフレームとして扱わない
	usableKeyFrames := keyFrames
	if len(usableKeyFrames) > 0 {
		usableKeyFrames = usableKeyFrames[:len(usableKeyFrames)-1]
	}
	if len(usableKeyFrames) == 0 || durationSeconds <= 0 {
		return []SegmentMapEntry{}
	}

	segmentDurationSeconds := ComputeSegmentDurationSeconds(videoFrameRate)
	segmentDurationTicks := int64(roundHalfEven(segmentDurationSeconds * float64(HZ)))
	segmentCount := max(1, int(math.Ceil(durationSeconds/segmentDurationSeconds)))
	sourceBaseDTS := usableKeyFrames[0].DTS
	dtsList := make([]int64, 0, len(usableKeyFrames))
	for _, keyFrame := range usableKeyFrames {
		dtsList = append(dtsList, keyFrame.DTS)
	}

	segmentMap := []SegmentMapEntry{}
	for sequenceIndex := range segmentCount {
		// プレイリスト上の時刻に対し、その時刻以前で最も近いキーフレームを採用する
		playlistStartSeconds := float64(sequenceIndex) * segmentDurationSeconds
		targetDTS := sourceBaseDTS + int64(roundHalfEven(playlistStartSeconds*float64(HZ)))
		keyFrameIndex := sort.Search(len(dtsList), func(index int) bool { return dtsList[index] > targetDTS }) - 1
		if keyFrameIndex < 0 {
			keyFrameIndex = 0
		}
		keyFrame := usableKeyFrames[keyFrameIndex]

		// 負の値は要求時刻より後ろのキーフレームなので、セグメント冒頭の映像を欠く
		keyframeAgeTicks := targetDTS - keyFrame.DTS
		if keyframeAgeTicks < 0 {
			continue
		}
		// 旧 key_frames が途中で途切れている録画では、最後のキーフレームが残り全セグメントへ割り当たる
		if keyframeAgeTicks >= segmentDurationTicks && keyFrameIndex == len(usableKeyFrames)-1 {
			continue
		}
		entry := SegmentMapEntry{
			SequenceIndex:      sequenceIndex,
			SourceFilePosition: keyFrame.Offset,
			SourceStartDTS:     keyFrame.DTS,
		}
		// 同じ入力位置を複数セグメントへ保存すると、シーク時に同一範囲を何度もエンコードしてしまうため、
		// 長い GOP や PID 切替直前の不自然な key_frames は、該当シーケンスだけオンデマンド探索へ任せる
		duplicated := false
		for _, saved := range segmentMap {
			if saved.SourceFilePosition == entry.SourceFilePosition && saved.SourceStartDTS == entry.SourceStartDTS {
				duplicated = true
				break
			}
		}
		if duplicated {
			continue
		}
		segmentMap = append(segmentMap, entry)
	}
	return segmentMap
}

// IsSegmentMapProbablyBroken は連続セグメントが同じ入力開始位置を指している segment_map かどうかを判定する。
// 移植元: VideoSegmentPlanner.isSegmentMapProbablyBroken()
func IsSegmentMapProbablyBroken(segmentMap []SegmentMapEntry) bool {
	sorted := make([]SegmentMapEntry, len(segmentMap))
	copy(sorted, segmentMap)
	sort.SliceStable(sorted, func(left, right int) bool {
		return sorted[left].SequenceIndex < sorted[right].SequenceIndex
	})
	var previous *SegmentMapEntry
	for index := range sorted {
		entry := sorted[index]
		if previous != nil &&
			previous.SequenceIndex == entry.SequenceIndex-1 &&
			previous.SourceFilePosition == entry.SourceFilePosition &&
			previous.SourceStartDTS == entry.SourceStartDTS {
			return true
		}
		previous = &entry
	}
	return false
}
