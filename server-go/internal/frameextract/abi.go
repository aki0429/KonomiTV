// Package frameextract は FFmpeg の公開 API を Go から呼び、候補ごとのシークとデコードを行う。
// ABI を読み取る処理は専用ワーカーで動かし、サーバーのプロセスへライブラリをロードしない。
package frameextract

import "unsafe"

type cptr = unsafe.Pointer

// FFmpeg 9.0.x の公開ヘッダーに対応する。構造体は参照する公開フィールドまでだけを宣言する。
// avformat / avcodec / avutil / swscale の major 検証前にはこれらを参照しない。
// https://github.com/FFmpeg/FFmpeg/tree/n9.0.2
type rational struct{ Num, Den int32 }
type formatHeader struct {
	Class, Input, Output, Private, IO cptr
	Flags                             int32
	StreamsCount                      uint32
	Streams                           cptr
}
type streamHeader struct {
	Class                   cptr
	Index, ID               int32
	Parameters, Private     cptr
	TimeBase                rational
	Start, Duration, Frames int64
}
type parametersHeader struct{ MediaType, CodecID int32 }
type packetHeader struct {
	Buffer              cptr
	PTS, DTS            int64
	Data                cptr
	Size, Stream, Flags int32
	SideData            cptr
	SideCount           int32
	Duration, Position  int64
	Opaque, OpaqueRef   cptr
	TimeBase            rational
}
type frameHeader struct {
	Data                                                   [8]cptr
	Line                                                   [8]int32
	Extended                                               cptr
	Width, Height, Samples, Format, Picture                int32
	SAR                                                    rational
	PTS, DTS                                               int64
	TimeBase                                               rational
	Quality                                                int32
	Opaque                                                 cptr
	Repeat, SampleRate                                     int32
	Buffer                                                 [8]cptr
	ExtBuffers                                             cptr
	ExtBufferCount                                         int32
	SideData                                               cptr
	SideCount, Flags, Range, Primaries, TRC, Space, Chroma int32
	BestTimestamp                                          int64
}

const (
	avEOF         int32 = -541478725
	avInvalidData int32 = -1094995529
	avAgain       int32 = -11
	avNoTimestamp int64 = -1 << 63
)
