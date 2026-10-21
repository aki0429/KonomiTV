package reservations

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipeTransportPartialFrame(t *testing.T) {
	transport := pipePeer(t, func(conn io.ReadWriteCloser) {
		defer conn.Close()
		var request [8]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return
		}
		frame := []byte{1, 0, 0, 0, 5, 0, 0, 0, 1, 0, 3, 0, 5}
		for _, value := range frame {
			if _, err := conn.Write([]byte{value}); err != nil {
				return
			}
		}
	})
	response, err := transport.SendAndReceive(make([]byte, 8))
	if err != nil || response.Code != 1 || !bytes.Equal(response.Data, []byte{1, 0, 3, 0, 5}) {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestPipeTransportCancellation(t *testing.T) {
	entered := make(chan struct{})
	transport := pipePeer(t, func(conn io.ReadWriteCloser) {
		defer conn.Close()
		var request [8]byte
		_, _ = io.ReadFull(conn, request[:])
		close(entered)
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport.Context = ctx
	done := make(chan error, 1)
	go func() { _, err := transport.SendAndReceive(make([]byte, 8)); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("peer was not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pipe Read was not cancelled")
	}
}

func TestPipeTransportResponseBounds(t *testing.T) {
	for _, size := range []uint32{5, 0x7fffffff, 0xffffffff} {
		transport := pipePeer(t, func(conn io.ReadWriteCloser) {
			defer conn.Close()
			var request [8]byte
			_, _ = io.ReadFull(conn, request[:])
			var header [8]byte
			binary.LittleEndian.PutUint32(header[:], 1)
			binary.LittleEndian.PutUint32(header[4:], size)
			_, _ = conn.Write(header[:])
			_, _ = conn.Write([]byte{1, 2})
		})
		_, err := transport.SendAndReceive(make([]byte, 8))
		if err == nil {
			t.Fatalf("invalid size %d accepted", size)
		}
		if size == 5 && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial body err=%v", err)
		}
	}
}
