package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPG3NativeCancellation は受信待ちの HTTP context 終了が TCP 接続を閉じることを固定する。
func TestPG3NativeCancellation(t *testing.T) {
	for _, phase := range []string{"search", "chset"} {
		t.Run(phase, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			server.config.General.Backend = "EDCB"
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			peerDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					peerDone <- err
					return
				}
				command, _, err := pg3ReadRequest(conn)
				want := uint32(1025)
				if phase == "chset" {
					want = 1060
				}
				if err != nil || command != want {
					conn.Close()
					peerDone <- fmt.Errorf("command=%d err=%v", command, err)
					return
				}
				accepted <- conn
				var b [1]byte
				_, err = conn.Read(b[:])
				conn.Close()
				if !pg3PeerClosed(err) {
					peerDone <- fmt.Errorf("peer close err=%v", err)
				} else {
					peerDone <- nil
				}
			}()
			server.config.General.EDCBURL = "tcp://" + listener.Addr().String() + "/"
			body := `{"service_ranges":[]}`
			if phase == "chset" {
				body = `{}`
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/api/programs/search", strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			result := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { server.Handler().ServeHTTP(result, req); close(done) }()
			var conn net.Conn
			select {
			case conn = <-accepted:
			case err := <-peerDone:
				t.Fatal(err)
			case <-time.After(2 * time.Second):
				t.Fatal("request was not sent")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				t.Error("cancelled HTTP request remained blocked waiting for CtrlCmd")
				conn.Close()
				<-done
			}
			select {
			case err := <-peerDone:
				if err != nil && !t.Failed() {
					t.Error(err)
				}
			case <-time.After(time.Second):
				conn.Close()
				t.Error("cancel did not close the peer")
			}
		})
	}
}

// TestPG3NativeDeadline は HTTP deadline が受信待ちを打ち切ることを固定する。
func TestPG3NativeDeadline(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		_, _, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		var b [1]byte
		_, err = conn.Read(b[:])
		if !pg3PeerClosed(err) {
			return fmt.Errorf("deadline did not close connection: %v", err)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/programs/search", strings.NewReader(`{"service_ranges":[]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	start := time.Now()
	server.Handler().ServeHTTP(result, req)
	if time.Since(start) > time.Second {
		t.Fatal("HTTP deadline was ignored")
	}
	if result.Code != 200 || result.Body.String() != "{\"total\":0,\"programs\":[]}\n" {
		t.Fatalf("deadline response=%d %s", result.Code, result.Body.String())
	}
}
