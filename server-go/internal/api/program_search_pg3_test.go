package api

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// pg3TCP は本物の EDCB を使わず、ローカル TCP 上で CtrlCmd frame を検証する。
func pg3TCP(t *testing.T, serve func(net.Conn) error) string {
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
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		done <- serve(conn)
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("synthetic CtrlCmd peer did not stop")
		}
	})
	return "tcp://" + listener.Addr().String() + "/"
}

func pg3ReadRequest(conn net.Conn) (uint32, []byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	size := binary.LittleEndian.Uint32(header[4:])
	if size > 1<<20 {
		return 0, nil, fmt.Errorf("unbounded request: %d", size)
	}
	body := make([]byte, size)
	_, err := io.ReadFull(conn, body)
	return binary.LittleEndian.Uint32(header), body, err
}

func pg3SendResponse(conn net.Conn, code uint32, body []byte) error {
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header, code)
	binary.LittleEndian.PutUint32(header[4:], uint32(len(body)))
	_, err := conn.Write(append(header, body...))
	return err
}

// TestPG3NativeEmptySearch は HTTP 200 だけでなく wire 上の SearchPg と空配列を固定する。
func TestPG3NativeEmptySearch(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		command, body, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		if command != 1025 {
			return fmt.Errorf("command=%d, want SearchPg 1025", command)
		}
		if len(body) < 12 || int(binary.LittleEndian.Uint32(body)) != len(body) || binary.LittleEndian.Uint32(body[4:]) != 1 || int(binary.LittleEndian.Uint32(body[8:])) != len(body)-8 {
			return fmt.Errorf("invalid search vector/struct lengths: %x", body)
		}
		return pg3SendResponse(conn, 1, []byte{8, 0, 0, 0, 0, 0, 0, 0})
	})
	response := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{"service_ranges":[]}`, "", "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("native search status=%d body=%s", response.Code, response.Body.String())
	}
	var actual map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	programs, ok := actual["programs"].([]any)
	if len(actual) != 2 || actual["total"] != float64(0) || !ok || len(programs) != 0 {
		t.Fatalf("empty native search=%s, want {total:0,programs:[]}", response.Body.String())
	}
}
