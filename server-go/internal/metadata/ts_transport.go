package metadata

import (
	"fmt"
	"io"
	"os"
)

// tsPacketSize は本家が使用する ariblib 0.2.0 と同じ優先順位で判定する。
func tsPacketSize(head []byte) int {
	if len(head) >= 384 && !(head[0] == 0x47 && head[188] == 0x47) && head[4] == 0x47 && head[196] == 0x47 {
		return 192
	}
	return 188
}

func detectTSPacketSize(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 188, err
	}
	defer file.Close()
	return tsPacketSizeAt(file)
}

func tsPacketSizeAt(reader io.ReaderAt) (int, error) {
	var head [384]byte
	n, err := reader.ReadAt(head[:], 0)
	if err != nil && !tsinfoOnlyEOF(err) {
		return 188, err
	}
	return tsPacketSize(head[:n]), nil
}

// m2tsReader は4バイトの追加ヘッダーを除いた仮想188バイトTSを読む。
// 元の映像ファイルとその物理オフセットは変更せず、解析中の読み取りだけを変換する。
type m2tsReader struct {
	source                     io.ReaderAt
	physicalEnd, end, position int64
}

func newM2TSReader(source io.ReaderAt, end int64) *m2tsReader {
	return &m2tsReader{source: source, physicalEnd: end, end: end/192*188 + max(end%192-4, 0)}
}

func (r *m2tsReader) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if r.position >= r.end {
		return 0, io.EOF
	}
	requested := int(min(int64(len(output)), r.end-r.position))
	first, skip := r.position/188, int(r.position%188)
	packets := (skip + requested + 187) / 188
	raw := make([]byte, packets*192)
	read, readErr := r.source.ReadAt(raw, first*192)
	raw = raw[:read]
	used := 0
	for packet := 0; packet < packets && used < requested; packet++ {
		start := packet*192 + 4 + skip
		if start >= len(raw) {
			break
		}
		count := min(188-skip, requested-used, len(raw)-start)
		copy(output[used:used+count], raw[start:start+count])
		used += count
		skip = 0
	}
	r.position += int64(used)
	if readErr != nil && !tsinfoOnlyEOF(readErr) {
		return used, readErr
	}
	if used == requested {
		if requested < len(output) {
			return used, io.EOF
		}
		return used, nil
	}
	if readErr == nil {
		readErr = io.ErrNoProgress
	}
	return used, readErr
}

func (r *m2tsReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.position
	case io.SeekEnd:
		offset += r.end
	default:
		return 0, fmt.Errorf("invalid MPEG-TS seek mode")
	}
	if offset < 0 {
		return 0, fmt.Errorf("negative MPEG-TS seek offset")
	}
	r.position = offset
	return offset, nil
}

func tsinfoTwentyPercent(reader io.ReadSeeker, end int64) int64 {
	switch r := reader.(type) {
	case *m2tsReader:
		return closestMultiple(r.physicalEnd/5, 192) / 192 * 188
	case *tsinfoWindowReader:
		if r.eventOffset != nil {
			return *r.eventOffset
		}
		return closestMultiple(end/5, 188)
	default:
		return closestMultiple(end/5, 188)
	}
}
