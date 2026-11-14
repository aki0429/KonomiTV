package frameextract

import (
	"context"
	"fmt"
	"unsafe"
)

// StreamInfo は対象にした最初の映像ストリームの時刻単位。
type StreamInfo struct {
	Index                                  int32
	TimeBaseNumerator, TimeBaseDenominator int32
	StartTime                              int64
}

// RGB は RGB24 のフレーム。C の参照からコピー済みなので、デコーダー終了後も有効。
type RGB struct {
	Width, Height int
	Pixels        []byte
	PTS           int64
}

// Decoder は一つのコンテナと最初の映像ストリームを保持する。
type Decoder struct {
	library                                   *Library
	format, codec, packet, frame, rgb, scaler cptr
	info                                      StreamInfo
}

// Open はコンテナを一度だけ開き、PyAV と同じスレッド方式で最初の映像ストリームを準備する。
func (l *Library) Open(path, containerFormat string) (result *Decoder, err error) {
	d := &Decoder{library: l}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	var hint cptr
	if containerFormat == "MPEG-TS" {
		hint = l.inputFormat("mpegts")
		if hint == nil {
			return nil, fmt.Errorf("MPEG-TS input format is unavailable")
		}
	}
	if ret := l.formatOpen(&d.format, path, hint, nil); ret < 0 {
		return nil, l.err("open thumbnail input", ret)
	}
	if ret := l.formatInfo(d.format, nil); ret < 0 {
		return nil, l.err("inspect thumbnail input", ret)
	}
	header := (*formatHeader)(d.format)
	if header.StreamsCount > 4096 || header.Streams == nil {
		return nil, fmt.Errorf("invalid thumbnail input stream table")
	}
	streams := unsafe.Slice((*cptr)(header.Streams), int(header.StreamsCount))
	var selected *streamHeader
	for _, p := range streams {
		if p == nil {
			continue
		}
		stream := (*streamHeader)(p)
		if stream.Parameters == nil {
			continue
		}
		parameters := (*parametersHeader)(stream.Parameters)
		if parameters.MediaType == 0 {
			selected = stream
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("no video stream found in thumbnail input")
	}
	parameters := (*parametersHeader)(selected.Parameters)
	codec := l.findDecoder(parameters.CodecID)
	if codec == nil {
		return nil, fmt.Errorf("video decoder is unavailable: %d", parameters.CodecID)
	}
	d.codec = l.codecAllocate(codec)
	if d.codec == nil {
		return nil, fmt.Errorf("allocate thumbnail decoder")
	}
	if ret := l.parametersToContext(d.codec, selected.Parameters); ret < 0 {
		return nil, l.err("copy thumbnail decoder parameters", ret)
	}
	for _, option := range [][2]string{{"skip_frame", "nointra"}, {"threads", "auto"}, {"thread_type", "slice"}} {
		if ret := l.option(d.codec, option[0], option[1], 0); ret < 0 {
			return nil, l.err("set thumbnail decoder "+option[0], ret)
		}
	}
	packed := uint64(uint32(selected.TimeBase.Num)) | uint64(uint32(selected.TimeBase.Den))<<32
	if ret := l.optionRational(d.codec, "pkt_timebase", packed, 0); ret < 0 {
		return nil, l.err("set thumbnail packet time base", ret)
	}
	if ret := l.codecOpen(d.codec, codec, nil); ret < 0 {
		return nil, l.err("open thumbnail decoder", ret)
	}
	d.packet = l.packetAllocate()
	d.frame = l.frameAllocate()
	d.rgb = l.frameAllocate()
	d.scaler = l.scaleAllocate()
	if d.packet == nil || d.frame == nil || d.rgb == nil || d.scaler == nil {
		return nil, fmt.Errorf("allocate thumbnail frame state")
	}
	if ret := l.option(d.scaler, "sws_flags", "bilinear", 0); ret < 0 {
		return nil, l.err("set thumbnail RGB interpolation", ret)
	}
	d.info = StreamInfo{selected.Index, selected.TimeBase.Num, selected.TimeBase.Den, selected.Start}
	return d, nil
}

// Info はファイルを再度開かずに映像ストリームの時刻情報を返す。
func (d *Decoder) Info() StreamInfo { return d.info }

// SeekTimestamp は最も近い過去側のキーフレームへ移動し、デコーダーの待機フレームを破棄する。
func (d *Decoder) SeekTimestamp(timestamp int64) error {
	if ret := d.library.seek(d.format, d.info.Index, timestamp, 1); ret < 0 {
		return d.library.err("seek thumbnail input", ret)
	}
	d.library.codecFlush(d.codec)
	return nil
}

// FirstI は指定数までの映像 AVPacket を処理する。音声や別ストリームは件数に含めない。
// InvalidDataError はそのパケットだけを飛ばし、他の FFmpeg エラーは候補の呼び出し元へ返す。
func (d *Decoder) FirstI(ctx context.Context, maxPackets int) (*RGB, int, error) {
	l := d.library
	for count := 0; count < maxPackets; {
		if err := ctx.Err(); err != nil {
			return nil, count, err
		}
		l.packetUnref(d.packet)
		ret := l.read(d.format, d.packet)
		if ret < 0 && ret != avEOF {
			return nil, count, l.err("demux thumbnail packet", ret)
		}
		packet := (*packetHeader)(d.packet)
		if ret >= 0 && packet.Stream != d.info.Index {
			continue
		}
		count++
		packet.TimeBase = rational{d.info.TimeBaseNumerator, d.info.TimeBaseDenominator}
		argument := d.packet
		if ret == avEOF {
			argument = nil
		}
		picture, err := d.decodePacket(argument)
		if err != nil {
			if native, ok := err.(*Error); !ok || native.Code != avInvalidData {
				return nil, count, err
			}
		} else if picture != nil {
			image, err := d.convertRGB(picture)
			l.frameFree(&picture)
			return image, count, err
		}
		if ret == avEOF {
			return nil, count, nil
		}
	}
	return nil, maxPackets, nil
}

func (d *Decoder) decodePacket(packet cptr) (cptr, error) {
	l := d.library
	if ret := l.send(d.codec, packet); ret < 0 {
		return nil, l.err("decode thumbnail packet", ret)
	}
	var firstI cptr
	hadFrame := false
	for {
		l.frameUnref(d.frame)
		ret := l.receive(d.codec, d.frame)
		if ret == avAgain || ret == avEOF {
			break
		}
		if ret < 0 {
			// PyAV は既にフレームを受け取ったパケットの末尾の InvalidDataError を無視する。
			if ret == avInvalidData && hadFrame {
				break
			}
			if firstI != nil {
				l.frameFree(&firstI)
			}
			return nil, l.err("receive thumbnail frame", ret)
		}
		hadFrame = true
		if firstI == nil && (*frameHeader)(d.frame).Picture == 1 {
			firstI = l.frameClone(d.frame)
			if firstI == nil {
				return nil, fmt.Errorf("retain thumbnail I-frame")
			}
		}
	}
	l.frameUnref(d.frame)
	return firstI, nil
}

func (d *Decoder) convertRGB(picture cptr) (*RGB, error) {
	l := d.library
	input := (*frameHeader)(picture)
	if input.Width <= 0 || input.Height <= 0 || input.Width > 16384 || input.Height > 16384 {
		return nil, fmt.Errorf("invalid thumbnail frame dimensions: %dx%d", input.Width, input.Height)
	}
	l.frameUnref(d.rgb)
	if ret := l.frameProperties(d.rgb, picture); ret < 0 {
		return nil, l.err("copy thumbnail frame properties", ret)
	}
	output := (*frameHeader)(d.rgb)
	output.Width = input.Width
	output.Height = input.Height
	output.Format = l.pixelFormat("rgb24")
	// PyAV 19 の通常 RGB 変換と同じく、色域や伝達関数の変換は明示指定しない。
	input.TRC = 2
	input.Primaries = 2
	output.TRC = 2
	output.Primaries = 2
	// PyAV の SWS_CS_ITU601 値5の既定変換は AVCOL_SPC_SMPTE170M 値6になる。
	if input.Space == 5 {
		input.Space = 6
		output.Space = 6
	}
	if ret := l.scaleFrame(d.scaler, d.rgb, picture); ret < 0 {
		return nil, l.err("convert thumbnail frame to RGB", ret)
	}
	w, h := int(output.Width), int(output.Height)
	if output.Data[0] == nil || int64(output.Line[0]) < int64(w)*3 {
		return nil, fmt.Errorf("invalid thumbnail RGB plane")
	}
	pixels := make([]byte, w*h*3)
	for y := range h {
		copy(pixels[y*w*3:(y+1)*w*3], unsafe.Slice((*byte)(unsafe.Add(output.Data[0], y*int(output.Line[0]))), w*3))
	}
	return &RGB{w, h, pixels, input.PTS}, nil
}

// Close はコンテナ・デコーダー・一時フレームを下流から解放する。複数回呼べる。
func (d *Decoder) Close() {
	if d == nil || d.library == nil {
		return
	}
	l := d.library
	if d.scaler != nil {
		l.scaleFree(&d.scaler)
	}
	if d.rgb != nil {
		l.frameFree(&d.rgb)
	}
	if d.frame != nil {
		l.frameFree(&d.frame)
	}
	if d.packet != nil {
		l.packetFree(&d.packet)
	}
	if d.codec != nil {
		l.codecFree(&d.codec)
	}
	if d.format != nil {
		l.formatClose(&d.format)
	}
}
