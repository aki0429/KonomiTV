package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func newCDPTestPeer(t *testing.T, handler func(*websocket.Conn, cdpMessage)) *cdpConnection {
	t.Helper()
	server := httptest.NewServer(websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			for {
				var request cdpMessage
				if websocket.JSON.Receive(ws, &request) != nil {
					return
				}
				handler(ws, request)
			}
		},
	})
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialCDP(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func replyCDP(ws *websocket.Conn, request cdpMessage, result any) {
	_ = websocket.JSON.Send(ws, map[string]any{"id": request.ID, "result": result, "sessionId": request.SessionID})
}

func TestCDPOutOfOrderAndQueuedEvents(t *testing.T) {
	var pending []cdpMessage
	c := newCDPTestPeer(t, func(ws *websocket.Conn, request cdpMessage) {
		switch request.Method {
		case "Batch":
			pending = append(pending, request)
			if len(pending) == 20 {
				for index := len(pending) - 1; index >= 0; index-- {
					replyCDP(ws, pending[index], json.RawMessage(pending[index].Params))
				}
			}
		case "Emit":
			for index := range 1000 {
				_ = websocket.JSON.Send(ws, map[string]any{"method": "Fixture.event", "sessionId": "page", "params": map[string]any{"index": index}})
			}
			replyCDP(ws, request, map[string]any{})
		default:
			replyCDP(ws, request, json.RawMessage(request.Params))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var group sync.WaitGroup
	for index := range 20 {
		group.Go(func() {
			var response struct {
				Index int `json:"index"`
			}
			if err := c.call(ctx, "page", "Batch", map[string]any{"index": index}, &response); err != nil || response.Index != index {
				t.Errorf("out of order response: %+v %v", response, err)
			}
		})
	}
	group.Wait()
	sub := c.subscribe("page", "Fixture.event")
	defer sub.Close()
	other := c.subscribe("other-page", "Fixture.event")
	defer other.Close()
	if err := c.call(ctx, "page", "Emit", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	// イベントの消費前でも、受信ループは後続コマンドへの応答を配送できる。
	if err := c.call(ctx, "page", "Echo", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	for index := range 1000 {
		event, err := sub.next(ctx)
		var data struct {
			Index int `json:"index"`
		}
		if err != nil || json.Unmarshal(event.Params, &data) != nil || data.Index != index {
			t.Fatalf("queued event %d: %+v %v", index, data, err)
		}
	}
	isolated, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, err := other.next(isolated); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("session isolation failed: %v", err)
	}
}

func TestCDPCancellationUnsubscribeAndDisconnect(t *testing.T) {
	seen := make(chan struct{}, 2)
	c := newCDPTestPeer(t, func(ws *websocket.Conn, request cdpMessage) {
		if request.Method == "Hold" {
			seen <- struct{}{}
			return
		}
		replyCDP(ws, request, map[string]any{})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.call(ctx, "page", "Hold", map[string]any{}, nil) }()
	<-seen
	// 別コマンドの往復で Hold のフレーム送信完了を確認し、応答待ちの中止を独立して検査する。
	probe, stopProbe := context.WithTimeout(context.Background(), time.Second)
	defer stopProbe()
	if err := c.call(probe, "page", "Echo", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled command: %v", err)
	}
	sub := c.subscribe("page", "Fixture.event")
	go func() { _, err := sub.next(context.Background()); done <- err }()
	sub.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unsubscribe did not stop the waiter: %v", err)
	}
	go func() { done <- c.call(context.Background(), "page", "Hold", map[string]any{}, nil) }()
	select {
	case <-seen:
	case err := <-done:
		t.Fatalf("second request did not reach the peer: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second request was not transmitted")
	}
	c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBrowserConnectionLost) {
			t.Fatalf("closed connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("socket close did not release the command")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 || len(c.events) != 0 {
		t.Fatal("command or subscription was retained")
	}
}

type blockedFrameConnection struct {
	net.Conn
	pause                  bool
	started, released      chan struct{}
	startOnce, releaseOnce sync.Once
}

func (c *blockedFrameConnection) Write(data []byte) (int, error) {
	if c.pause {
		c.startOnce.Do(func() { close(c.started) })
		<-c.released
		return 0, net.ErrClosed
	}
	return c.Conn.Write(data)
}

func (c *blockedFrameConnection) Close() error {
	c.releaseOnce.Do(func() { close(c.released) })
	return c.Conn.Close()
}

func TestCDPCancelBlockedWrite(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   func(ws *websocket.Conn) { defer ws.Close(); <-release },
	})
	defer server.Close()
	defer close(release)
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockedFrameConnection{Conn: socket, started: make(chan struct{}), released: make(chan struct{})}
	ws, err := websocket.NewClient(config, blocked)
	if err != nil {
		t.Fatal(err)
	}
	c := &cdpConnection{ws: ws, socket: blocked, done: make(chan struct{}), write: make(chan struct{}, 1)}
	c.write <- struct{}{}
	defer c.Close()
	// OS の自動バッファー拡張に依存せず、Write が待機したことを確認してから中止する。
	encoded, err := json.Marshal(map[string]any{"data": strings.Repeat("x", 1024*1024)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked.pause = true
	done := make(chan error, 1)
	go func() { done <- c.sendFrame(ctx, encoded) }()
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("frame write did not reach the controlled wait")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked frame write ignored cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked write stopped too late")
	}
	select {
	case <-c.done:
	default:
		t.Fatal("cancelled partial frame did not close the connection")
	}
}

func TestCDPEvaluateAndInvalidGraphQLValues(t *testing.T) {
	for _, value := range []string{"null", "[]", `"unexpected"`, "{}", `{"statusCode":200,"requestError":null}`} {
		c := newCDPTestPeer(t, func(ws *websocket.Conn, request cdpMessage) {
			replyCDP(ws, request, map[string]any{"result": map[string]any{"value": json.RawMessage(value)}, "exceptionDetails": nil})
		})
		b := NewBrowserBackend(&Account{}, BrowserOptions{}).(*BrowserBackend)
		b.cdp, b.session = c, "page"
		response, err := b.InvokeGraphQLAPI(context.Background(), "Viewer", NewOrderedMap(), nil)
		c.Close()
		if strings.HasPrefix(value, "{") {
			if response == nil || err != nil {
				t.Fatalf("valid response object rejected: %s %v", value, err)
			}
		} else if err == nil {
			t.Fatalf("non-object response accepted: %s", value)
		}
	}
	c := newCDPTestPeer(t, func(ws *websocket.Conn, request cdpMessage) {
		replyCDP(ws, request, map[string]any{"exceptionDetails": map[string]any{"text": "fixture JS exception"}})
	})
	if err := c.evaluate(context.Background(), "page", "throw Error()", false, nil); err == nil {
		t.Fatal("JavaScript exception was ignored")
	}
}
