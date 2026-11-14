package reservations

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/testutil/edcbipc"
)

func TestLocalPipeTransportFragmentedResponse(t *testing.T) {
	directory := edcbipc.Directory(t)
	accept := edcbipc.CommandListener(t, directory+"command")
	request := []byte{1, 2, 3, 4, 5}
	want := bytes.Repeat([]byte("EDCB"), 20000)
	done := make(chan error, 1)
	go func() {
		conn, err := accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		got := make([]byte, len(request))
		if _, err := io.ReadFull(conn, got); err != nil {
			done <- err
			return
		}
		if !bytes.Equal(got, request) {
			done <- io.ErrUnexpectedEOF
			return
		}
		data := make([]byte, 8, 8+len(want))
		binary.LittleEndian.PutUint32(data[:4], 1)
		binary.LittleEndian.PutUint32(data[4:], uint32(len(want)))
		data = append(data, want...)
		for _, part := range [][]byte{data[:2], data[2:5], data[5:7], data[7:13], data[13:]} {
			if _, err := conn.Write(part); err != nil {
				done <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
		done <- nil
	}()
	transport := &pipeTransport{Path: directory, Name: "command", Timeout: 3 * time.Second}
	response, err := transport.SendAndReceive(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != 1 || !bytes.Equal(response.Data, want) {
		t.Fatalf("fragmented response changed: code=%d bytes=%d", response.Code, len(response.Data))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLocalPipeTransportIncompleteAndCancelled(t *testing.T) {
	for _, kind := range []string{"short-header", "short-body", "negative-size", "huge-truncated", "cancel", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			directory := edcbipc.Directory(t)
			accept := edcbipc.CommandListener(t, directory+"command")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverDone := make(chan error, 1)
			go func() {
				conn, err := accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				request := make([]byte, 1)
				if _, err := io.ReadFull(conn, request); err != nil {
					serverDone <- err
					return
				}
				header := []byte{1, 0, 0, 0, 4, 0, 0, 0}
				switch kind {
				case "cancel", "timeout":
					if kind == "cancel" {
						cancel()
					}
					_, err = conn.Read(request)
					if err == nil {
						serverDone <- io.ErrNoProgress
						return
					}
				case "short-header":
					_, err = conn.Write(header[:3])
				case "negative-size":
					binary.LittleEndian.PutUint32(header[4:], 0xffffffff)
					_, err = conn.Write(header)
				case "huge-truncated":
					binary.LittleEndian.PutUint32(header[4:], 0x7fffffff)
					_, err = conn.Write(header)
				default:
					_, err = conn.Write(append(header, 1, 2))
				}
				if kind == "cancel" || kind == "timeout" {
					err = nil
				}
				serverDone <- err
			}()
			transport := &pipeTransport{Path: directory, Name: "command", Timeout: 150 * time.Millisecond, Context: ctx}
			start := time.Now()
			if _, err := transport.SendAndReceive([]byte{1}); err == nil {
				t.Fatal("incomplete response succeeded")
			}
			if time.Since(start) > time.Second {
				t.Fatal("request did not stop on time")
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalPipeTransportURLContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, err := NewClientFromURLContext(ctx, "tcp://edcb-namedpipe/")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.transport.(*pipeTransport)
	if !ok || transport.Context != ctx {
		t.Fatal("URL client dropped the request context")
	}
	if _, err := transport.SendAndReceive([]byte{1}); err == nil {
		t.Fatal("cancelled URL client connected")
	}
}
