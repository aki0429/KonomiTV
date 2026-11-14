package frameextract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestNativeDecoderWithMatchingLibraries(t *testing.T) {
	directory := os.Getenv("KONOMITV_TEST_FFMPEG_LIBRARIES")
	if directory == "" {
		t.Skip("KONOMITV_TEST_FFMPEG_LIBRARIES is not set")
	}
	l, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FFmpeg %s", l.Version())
	if _, err := l.Open(filepath.Join(t.TempDir(), "missing.ts"), "MPEG-TS"); err == nil {
		t.Fatal("missing media opened")
	}
	d, err := l.Open(filepath.Join("..", "metadata", "testdata", "thumbnail_tsreadex_input.mpegts"), "MPEG-TS")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info := d.Info()
	if info.Index != 0 || info.StartTime != 129600 || info.TimeBaseNumerator != 1 || info.TimeBaseDenominator != 90000 {
		t.Fatalf("stream=%+v", info)
	}
	for _, tc := range []struct {
		offset float64
		pts    int64
	}{{0, 226800}, {2, 399600}, {5, 658800}} {
		target := int64(float64(info.StartTime) + tc.offset/(float64(info.TimeBaseNumerator)/float64(info.TimeBaseDenominator)))
		if err := d.SeekTimestamp(target); err != nil {
			t.Fatal(err)
		}
		frame, packets, err := d.FirstI(context.Background(), 20000)
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil || frame.PTS != tc.pts || frame.Width != 640 || frame.Height != 360 || len(frame.Pixels) != 640*360*3 || packets <= 0 {
			t.Fatalf("offset=%v frame=%+v packets=%d", tc.offset, frame, packets)
		}
		if frame.Pixels[0] != 253 || frame.Pixels[1] != 0 || frame.Pixels[2] != 0 {
			t.Errorf("pixel=%v", frame.Pixels[:3])
		}
	}
	d.Close() // 二重解放を避けることも実ライブラリで検査する。
}

func TestDecoderPacketLimitAndInvalidPackets(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		limit, videoPackets  int
		invalidAt            int
		wantReads, wantSends int
	}{
		{"limit", 3, 10, 0, 6, 3},
		{"damaged-packet", 3, 10, 2, 6, 3},
		{"EOF-flush", 10, 2, 0, 5, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packet, frame := &packetHeader{}, &frameHeader{}
			reads, sends := 0, 0
			l := &Library{
				packetUnref: func(cptr) {}, frameUnref: func(cptr) {},
				errorString: func(_ int32, p *byte, n uintptr) int32 {
					copy(unsafe.Slice(p, int(n)), []byte("test error\x00"))
					return 0
				},
				read: func(_ cptr, p cptr) int32 {
					reads++
					if reads > tc.videoPackets*2 {
						return avEOF
					}
					(*packetHeader)(p).Stream = int32(reads % 2) // video index=0、他のストリームは件数に含めない。
					return 0
				},
				send: func(cptr, cptr) int32 {
					sends++
					if sends == tc.invalidAt {
						return avInvalidData
					}
					return 0
				},
				receive: func(cptr, cptr) int32 { return avAgain },
			}
			d := Decoder{library: l, packet: unsafe.Pointer(packet), frame: unsafe.Pointer(frame), info: StreamInfo{Index: 0}}
			got, count, err := d.FirstI(context.Background(), tc.limit)
			if err != nil || got != nil || count != tc.wantSends || reads != tc.wantReads || sends != tc.wantSends {
				t.Fatalf("frame=%v packets=%d reads=%d sends=%d err=%v", got, count, reads, sends, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (&Decoder{library: &Library{}}).FirstI(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}
