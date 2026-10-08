package reservations

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPG3IndependentUTF16Strict(t *testing.T) {
	for name, raw := range map[string][]byte{"high": {0, 0xd8}, "low": {0, 0xdc}, "odd": {0x41}, "high41": {0x41, 0xd8}} {
		t.Run(name, func(t *testing.T) {
			b := binary.LittleEndian.AppendUint32(nil, uint32(6+len(raw)))
			b = append(b, raw...)
			b = append(b, 0, 0)
			_, ok := decodeResponse(func() string { r := wireReader{buf: b}; return r.readString(len(b)) })
			if ok {
				t.Errorf("Python strict decode rejects; Go accepted raw=%x", raw)
			}
		})
	}
}
func TestPG3IndependentUTF16ValidPair(t *testing.T) {
	b := []byte{10, 0, 0, 0, 0x3c, 0xd8, 0x21, 0xde, 0, 0}
	r := wireReader{buf: b}
	if r.readString(len(b)) != "🈡" {
		t.Fatal("pair")
	}
}
func TestPG3IndependentHugeCountBounded(t *testing.T) {
	b := []byte{8, 0, 0, 0, 255, 255, 255, 127}
	start := time.Now()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, ok := decodeResponse(func() []EventInfo { r := wireReader{buf: b}; return readVector(&r, len(b), readEventInfo) })
	runtime.ReadMemStats(&after)
	if ok || time.Since(start) > time.Second || after.TotalAlloc-before.TotalAlloc > 4<<20 {
		t.Fatalf("ok=%v alloc=%d elapsed=%v", ok, after.TotalAlloc-before.TotalAlloc, time.Since(start))
	}
}
func TestPG3IndependentStringTruncation(t *testing.T) {
	full := []byte{10, 0, 0, 0, 0x3c, 0xd8, 0x21, 0xde, 0, 0}
	for n := 0; n < len(full); n++ {
		b := full[:n]
		_, ok := decodeResponse(func() string { r := wireReader{buf: b}; return r.readString(len(b)) })
		if ok {
			t.Errorf("accepted truncated len=%d", n)
		}
	}
}
func TestPG3IndependentSystemTimeInvalid(t *testing.T) {
	for _, v := range [][6]uint16{{2025, 2, 29, 12, 0, 0}, {2024, 2, 30, 0, 0, 0}, {0, 1, 1, 0, 0, 0}, {2024, 1, 1, 24, 0, 0}, {2024, 1, 1, 0, 60, 0}, {2024, 1, 1, 0, 0, 60}} {
		b := make([]byte, 16)
		for i, x := range v {
			offset := []int{0, 2, 6, 8, 10, 12}[i]
			binary.LittleEndian.PutUint16(b[offset:], x)
		}
		r := wireReader{buf: b}
		if !r.readSystemTime(len(b)).Equal(unixEpoch) {
			t.Errorf("date=%v", v)
		}
	}
}
func TestPG3IndependentDurationIntegerOverflow(t *testing.T) {
	w := wireWriter{}
	writeSearchKeyInfo(&w, SearchKeyInfo{ChkDurationMin: 9223372036854775807}, false)
	r := wireReader{buf: w.buf}
	end := r.readStructIntro(len(w.buf))
	key := r.readString(end)
	// uv Python 整数参照: (9223372036854775807*10000)%100000000 = 58070000。
	if key != "D!{158070000}" {
		t.Errorf("duration min MaxInt64 Python key=D!{158070000}; Go key=%q", key)
	}
}
func TestPG3IndependentCancelReadBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := pg3TransportPeer(t, func(c net.Conn) error {
		var req [8]byte
		if _, e := io.ReadFull(c, req[:]); e != nil {
			return e
		}
		header := []byte{1, 0, 0, 0, 64, 0, 0, 0}
		if _, e := c.Write(append(header, 1, 2)); e != nil {
			return e
		}
		cancel()
		var b [1]byte
		_, e := c.Read(b[:])
		if !pg3PeerClosed(e) {
			return e
		}
		return nil
	})
	tr := TCPTransport{Address: address, Timeout: time.Second, Context: ctx}
	start := time.Now()
	_, e := tr.SendAndReceive(make([]byte, 8))
	if e == nil || time.Since(start) > time.Second {
		t.Fatalf("cancel err=%v elapsed=%v", e, time.Since(start))
	}
	t.Logf("cancel error type=%T context.Is=%v", e, errors.Is(e, context.Canceled))
}
func TestPG3IndependentRFCErrorCode(t *testing.T) {
	for _, code := range []uint32{0, 2, 0xffffffff} {
		address := pg3TransportPeer(t, func(c net.Conn) error {
			var req [8]byte
			if _, e := io.ReadFull(c, req[:]); e != nil {
				return e
			}
			b := binary.LittleEndian.AppendUint32(nil, code)
			b = binary.LittleEndian.AppendUint32(b, 8)
			b = append(b, 8, 0, 0, 0, 0, 0, 0, 0)
			_, e := c.Write(b)
			return e
		})
		client := NewClient(&TCPTransport{Address: address})
		_, ok := client.SearchPg(nil)
		if ok {
			t.Errorf("code %d accepted", code)
		}
	}
}
func TestPG3IndependentPartialWriteError(t *testing.T) {
	address := pg3TransportPeer(t, func(c net.Conn) error { var b [16]byte; _, e := io.ReadFull(c, b[:]); return e })
	req := make([]byte, 16<<20)
	binary.LittleEndian.PutUint32(req, 1025)
	_, e := (&TCPTransport{Address: address, Timeout: 200 * time.Millisecond}).SendAndReceive(req)
	if e == nil {
		t.Fatal("peer close during large write was ignored")
	}
	t.Logf("partial write error=%T %v", e, e)
}
func TestPG3IndependentExactlyCapHeader(t *testing.T) {
	address := pg3TransportPeer(t, func(c net.Conn) error {
		var req [8]byte
		if _, e := io.ReadFull(c, req[:]); e != nil {
			return e
		}
		b := binary.LittleEndian.AppendUint32(nil, 1)
		b = binary.LittleEndian.AppendUint32(b, 64<<20)
		_, e := c.Write(b)
		return e
	})
	_, e := (&TCPTransport{Address: address}).SendAndReceive(make([]byte, 8))
	if e == nil || strings.Contains(e.Error(), "invalid response size") {
		t.Fatalf("exact cap must reach EOF without preallocation: %v", e)
	}
}
