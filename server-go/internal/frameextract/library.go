package frameextract

import (
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Library は検証済み FFmpeg 9 ライブラリの API。使用中に DLL の検索先を変更しない。
type Library struct {
	formatOpen          func(*cptr, string, cptr, cptr) int32
	formatInfo          func(cptr, cptr) int32
	formatClose         func(*cptr)
	inputFormat         func(string) cptr
	seek                func(cptr, int32, int64, int32) int32
	read                func(cptr, cptr) int32
	findDecoder         func(int32) cptr
	codecAllocate       func(cptr) cptr
	parametersToContext func(cptr, cptr) int32
	codecOpen           func(cptr, cptr, cptr) int32
	codecFree           func(*cptr)
	codecFlush          func(cptr)
	send                func(cptr, cptr) int32
	receive             func(cptr, cptr) int32
	packetAllocate      func() cptr
	packetUnref         func(cptr)
	packetFree          func(*cptr)
	frameAllocate       func() cptr
	frameClone          func(cptr) cptr
	frameUnref          func(cptr)
	frameFree           func(*cptr)
	frameProperties     func(cptr, cptr) int32
	pixelFormat         func(string) int32
	option              func(cptr, string, string, int32) int32
	optionRational      func(cptr, string, uint64, int32) int32
	scaleAllocate       func() cptr
	scaleFrame          func(cptr, cptr, cptr) int32
	scaleFree           func(*cptr)
	errorString         func(int32, *byte, uintptr) int32
	versionInfo         func() cptr
}

// Load は指定したディレクトリだけから同じ ABI のライブラリをロードする。
// 元の FFmpeg CLI の共有ライブラリとは別の場所を使い、既存エンコーダーへ影響を与えない。
func Load(directory string) (*Library, error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return nil, fmt.Errorf("thumbnail library ABI is unsupported on %s", runtime.GOARCH)
	}
	if directory == "" {
		return nil, fmt.Errorf("thumbnail library directory is empty")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve thumbnail libraries: %w", err)
	}
	open := func(name string) (uintptr, error) {
		pattern := "lib" + name + "-*.so.*"
		if runtime.GOOS == "windows" {
			pattern = name + "-*.dll"
		}
		paths, err := filepath.Glob(filepath.Join(absolute, pattern))
		if err != nil {
			return 0, err
		}
		if len(paths) != 1 {
			return 0, fmt.Errorf("expected one %s library in %s, found %d", name, absolute, len(paths))
		}
		return openLibrary(paths[0])
	}
	util, err := open("avutil")
	if err != nil {
		return nil, err
	}
	codec, err := open("avcodec")
	if err != nil {
		return nil, err
	}
	format, err := open("avformat")
	if err != nil {
		return nil, err
	}
	scale, err := open("swscale")
	if err != nil {
		return nil, err
	}
	bind := func(destination any, library uintptr, name string) error {
		address, err := lookupSymbol(library, name)
		if err != nil {
			return err
		}
		purego.RegisterFunc(destination, address)
		return nil
	}
	// 文字列と関数だけで ABI を検査する。異なる major の構造体を読むことはしない。
	for _, v := range []struct {
		handle uintptr
		name   string
		major  uint32
	}{{util, "avutil_version", 61}, {codec, "avcodec_version", 63}, {format, "avformat_version", 63}, {scale, "swscale_version", 10}} {
		var version func() uint32
		if err := bind(&version, v.handle, v.name); err != nil {
			return nil, err
		}
		if got := version() >> 16; got != v.major {
			return nil, fmt.Errorf("unsupported %s major %d, expected %d", v.name, got, v.major)
		}
	}
	l := &Library{}
	for _, f := range []struct {
		destination any
		handle      uintptr
		name        string
	}{
		{&l.formatOpen, format, "avformat_open_input"}, {&l.formatInfo, format, "avformat_find_stream_info"}, {&l.formatClose, format, "avformat_close_input"},
		{&l.inputFormat, format, "av_find_input_format"}, {&l.seek, format, "av_seek_frame"}, {&l.read, format, "av_read_frame"},
		{&l.findDecoder, codec, "avcodec_find_decoder"}, {&l.codecAllocate, codec, "avcodec_alloc_context3"}, {&l.parametersToContext, codec, "avcodec_parameters_to_context"},
		{&l.codecOpen, codec, "avcodec_open2"}, {&l.codecFree, codec, "avcodec_free_context"}, {&l.codecFlush, codec, "avcodec_flush_buffers"},
		{&l.send, codec, "avcodec_send_packet"}, {&l.receive, codec, "avcodec_receive_frame"},
		{&l.packetAllocate, codec, "av_packet_alloc"}, {&l.packetUnref, codec, "av_packet_unref"}, {&l.packetFree, codec, "av_packet_free"},
		{&l.frameAllocate, util, "av_frame_alloc"}, {&l.frameClone, util, "av_frame_clone"}, {&l.frameUnref, util, "av_frame_unref"}, {&l.frameFree, util, "av_frame_free"},
		{&l.frameProperties, util, "av_frame_copy_props"}, {&l.pixelFormat, util, "av_get_pix_fmt"}, {&l.option, util, "av_opt_set"}, {&l.optionRational, util, "av_opt_set_q"},
		{&l.scaleAllocate, scale, "sws_alloc_context"}, {&l.scaleFrame, scale, "sws_scale_frame"}, {&l.scaleFree, scale, "sws_free_context"},
		{&l.errorString, util, "av_strerror"}, {&l.versionInfo, util, "av_version_info"},
	} {
		if err := bind(f.destination, f.handle, f.name); err != nil {
			return nil, err
		}
	}
	var setLogLevel func(int32)
	if err := bind(&setLogLevel, util, "av_log_set_level"); err != nil {
		return nil, err
	}
	// 受信エラーは Go で候補単位に報告する。C 側の大量診断をサーバーへ流さない。
	setLogLevel(-8)
	return l, nil
}

// Version はロードした FFmpeg 自体のバージョンを返す。
func (l *Library) Version() string {
	p := l.versionInfo()
	if p == nil {
		return ""
	}
	for n := 0; n < 1024; n++ {
		if *(*byte)(unsafe.Add(p, n)) == 0 {
			return string(unsafe.Slice((*byte)(p), n))
		}
	}
	return "unknown"
}

// Error は FFmpeg が返した失敗コードを保持し、デコード不能パケットと I/O エラーを識別する。
type Error struct {
	Operation string
	Code      int32
	Detail    string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s (%d)", e.Operation, e.Detail, e.Code) }
func (l *Library) err(operation string, code int32) error {
	buf := make([]byte, 256)
	l.errorString(code, &buf[0], uintptr(len(buf)))
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return &Error{operation, code, string(buf[:n])}
}
