package reservations

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPG3LegacyLargeHeader は旧 URL client の FileCopy 系に検索専用上限を漏らさない。
// 本文なしの巨大な申告長だけでは先取り allocation をしないことも同時に固定する。
func TestPG3LegacyLargeHeader(t *testing.T) {
	for _, command := range []uint32{1060, 2060} {
		t.Run(strconv.Itoa(int(command)), func(t *testing.T) {
			address := pg3TransportPeer(t, func(conn net.Conn) error {
				var request [8]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil {
					return err
				}
				var header [8]byte
				binary.LittleEndian.PutUint32(header[:], 1)
				binary.LittleEndian.PutUint32(header[4:], 0x7fffffff)
				_, err := conn.Write(header[:])
				return err
			})
			client, err := NewClientFromURL("tcp://" + address + "/")
			if err != nil {
				t.Fatal(err)
			}
			var request [8]byte
			binary.LittleEndian.PutUint32(request[:], command)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err = client.transport.SendAndReceive(request[:])
			runtime.ReadMemStats(&after)
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("legacy FileCopy header must reach bounded body read, got %v", err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16*1024*1024 {
				t.Errorf("header alone allocated %d bytes", allocated)
			}
		})
	}
}

// TestPG3LegacyLargeFileCopy は 64 MiB を超える正常な FileCopy の本文が欠落しないことを固定する。
// 申告長だけの probe と区別し、実 TCP で内容・長さの両方を検証する。
func TestPG3LegacyLargeFileCopy(t *testing.T) {
	const size = 64*1024*1024 + 1
	address := pg3TransportPeer(t, func(conn net.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		var request [8]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		body := make([]byte, binary.LittleEndian.Uint32(request[4:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(request[:]) != 1060 {
			return fmt.Errorf("unexpected command")
		}
		var header [8]byte
		binary.LittleEndian.PutUint32(header[:], 1)
		binary.LittleEndian.PutUint32(header[4:], size)
		if _, err := conn.Write(header[:]); err != nil {
			return err
		}
		chunk := bytes.Repeat([]byte{0x5a}, 32*1024)
		for remaining := size; remaining > 0; {
			n := min(remaining, len(chunk))
			written, err := conn.Write(chunk[:n])
			remaining -= written
			if err != nil {
				return err
			}
		}
		return nil
	})
	client, err := NewClientFromURL("tcp://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	data, ok := client.FileCopy("large.synthetic")
	if !ok || len(data) != size {
		t.Fatalf("large FileCopy ok=%v len=%d want=%d", ok, len(data), size)
	}
	for index, value := range data {
		if value != 0x5a {
			t.Fatalf("corrupt byte at %d: %x", index, value)
		}
	}
}

// TestPG3LegacySearchRetainsBound は旧 client でも SearchPg だけは巨大 frame を即座に拒否する。
func TestPG3LegacySearchRetainsBound(t *testing.T) {
	address := pg3TransportPeer(t, func(conn net.Conn) error {
		var request [8]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		var header [8]byte
		binary.LittleEndian.PutUint32(header[:], 1)
		binary.LittleEndian.PutUint32(header[4:], 64*1024*1024+1)
		_, err := conn.Write(header[:])
		return err
	})
	client, err := NewClientFromURL("tcp://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	var request [8]byte
	binary.LittleEndian.PutUint32(request[:], 1025)
	_, err = client.transport.SendAndReceive(request[:])
	if err == nil || !strings.Contains(err.Error(), "invalid response size") {
		t.Fatalf("SearchPg bound missing on legacy client: %v", err)
	}
}

// TestPG3SearchClientRetainsBound は HTTP 検索専用 client が ChSet5 を含め有限上限を維持する。
func TestPG3SearchClientRetainsBound(t *testing.T) {
	address := pg3TransportPeer(t, func(conn net.Conn) error {
		var request [8]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		var header [8]byte
		binary.LittleEndian.PutUint32(header[:], 1)
		binary.LittleEndian.PutUint32(header[4:], 64*1024*1024+1)
		_, err := conn.Write(header[:])
		return err
	})
	client, err := NewClientFromURLContext(context.Background(), "tcp://"+address+"/")
	if err != nil {
		t.Fatal(err)
	}
	var request [8]byte
	binary.LittleEndian.PutUint32(request[:], 1060)
	_, err = client.transport.SendAndReceive(request[:])
	if err == nil || !strings.Contains(err.Error(), "invalid response size") {
		t.Fatalf("search ChSet5 bound missing: %v", err)
	}
}
