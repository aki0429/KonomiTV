package reservations

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// pg3TransportPeer は本番接続を伴わない、有限寿命の TCP peer。
func pg3TransportPeer(t *testing.T, serve func(net.Conn) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		done <- serve(conn)
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("peer did not terminate")
		}
	})
	return listener.Addr().String()
}

// TestPG3TransportOversizedFrame は不正長に対し確保/本文待ちを行わず拒否する。
func TestPG3TransportOversizedFrame(t *testing.T) {
	address := pg3TransportPeer(t, func(conn net.Conn) error {
		request := make([]byte, 8)
		if _, err := io.ReadFull(conn, request); err != nil {
			return err
		}
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header, 1)
		binary.LittleEndian.PutUint32(header[4:], 64*1024*1024+1)
		if _, err := conn.Write(header); err != nil {
			return err
		}
		var b [1]byte
		_, err := conn.Read(b[:])
		if !pg3PeerClosed(err) {
			return fmt.Errorf("client failed to close: %v", err)
		}
		return nil
	})
	transport := TCPTransport{Address: address, Timeout: 100 * time.Millisecond}
	_, err := transport.SendAndReceive(make([]byte, 8))
	if err == nil || !strings.Contains(err.Error(), "invalid response size") {
		t.Fatalf("oversized frame must be rejected before body read, got %v", err)
	}
}

// TestPG3TransportTimeout は HTTP deadline がないクライアントでも有限時間に失敗する。
func TestPG3TransportTimeout(t *testing.T) {
	address := pg3TransportPeer(t, func(conn net.Conn) error {
		request := make([]byte, 8)
		if _, err := io.ReadFull(conn, request); err != nil {
			return err
		}
		var b [1]byte
		_, err := conn.Read(b[:])
		if !pg3PeerClosed(err) {
			return err
		}
		return nil
	})
	transport := TCPTransport{Address: address, Timeout: 30 * time.Millisecond}
	start := time.Now()
	_, err := transport.SendAndReceive(make([]byte, 8))
	timeout, ok := err.(net.Error)
	if !ok || !timeout.Timeout() {
		t.Fatalf("timeout error=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("transport timeout was ignored")
	}
}
