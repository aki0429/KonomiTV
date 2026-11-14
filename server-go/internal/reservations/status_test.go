package reservations

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

type statusTransport func([]byte) (Response, error)

func (f statusTransport) SendAndReceive(request []byte) (Response, error) { return f(request) }

func TestEDCBServerStatus(t *testing.T) {
	for _, state := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			writer := &wireWriter{}
			writer.writeUshort(5)
			start := len(writer.buf)
			writer.writeInt(0)
			writer.writeUint(100)
			writer.writeSystemTime(time.Date(2026, 11, 14, 12, 0, 0, 0, time.FixedZone("JST", 9*3600)))
			writer.writeUint(state)
			writer.writeUint(0)
			writer.writeUint(0)
			writer.writeString("")
			writer.writeString("状態")
			writer.writeString("")
			writer.writeIntInplace(start, len(writer.buf)-start)
			client := NewClient(statusTransport(func(request []byte) (Response, error) {
				if len(request) != 14 || binary.LittleEndian.Uint32(request) != 2200 || binary.LittleEndian.Uint32(request[4:]) != 6 || binary.LittleEndian.Uint16(request[8:]) != 5 || binary.LittleEndian.Uint32(request[10:]) != 0 {
					t.Errorf("unexpected status request: %x", request)
				}
				return Response{Code: 1, Data: writer.buf}, nil
			}))
			want := []string{"Normal", "Recording", "EPGGathering", "Unknown"}[state]
			if got := client.ServerStatus(); got != want {
				t.Fatalf("status=%s, want %s", got, want)
			}
			for end := range len(writer.buf) {
				client = NewClient(statusTransport(func([]byte) (Response, error) { return Response{Code: 1, Data: writer.buf[:end]}, nil }))
				if got := client.ServerStatus(); got != "Unknown" {
					t.Fatalf("truncated status at %d accepted: %s", end, got)
				}
			}
		})
	}
	for _, response := range []Response{{Code: 0}, {Code: 1, Data: []byte{4, 0}}} {
		client := NewClient(statusTransport(func([]byte) (Response, error) { return response, nil }))
		if got := client.ServerStatus(); got != "Unknown" {
			t.Fatal(got)
		}
	}
}
