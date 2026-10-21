package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCDPTransportResponseEventCancellationAndClosure(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		for {
			var request struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := socket.ReadJSON(&request); err != nil {
				return
			}
			if request.Method == "hang" {
				continue
			}
			if request.Method == "close" {
				return
			}
			if request.Method == "event" {
				if err := socket.WriteJSON(map[string]any{"method": "test.event", "params": map[string]any{"value": 42}}); err != nil {
					return
				}
			}
			response := map[string]any{"id": request.ID, "result": map[string]any{"method": request.Method}}
			if request.Method == "error" {
				response = map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "SECRET-SHOULD-NOT-APPEAR"}}
			}
			if err := socket.WriteJSON(response); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	session, err := newCDPSession(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	events, unsubscribe := session.Subscribe("test.event")
	defer unsubscribe()
	var result struct {
		Method string `json:"method"`
	}
	if err := session.Call(context.Background(), "event", nil, &result); err != nil || result.Method != "event" {
		t.Fatalf("command %v %+v", err, result)
	}
	select {
	case event := <-events:
		var params map[string]int
		if err := json.Unmarshal(event.Params, &params); err != nil || params["value"] != 42 {
			t.Fatal("bad event")
		}
	case <-time.After(time.Second):
		t.Fatal("event lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := session.Call(ctx, "hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel %v", err)
	}
	session.mu.Lock()
	remaining := len(session.pending)
	session.mu.Unlock()
	if remaining != 0 {
		t.Fatal("canceled command retained")
	}
	if err := session.Call(context.Background(), "error", nil, nil); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaked: %v", err)
	}
	if err := session.Call(context.Background(), "close", nil, nil); !errors.Is(err, ErrBrowserConnectionLost) {
		t.Fatalf("closure %v", err)
	}
}
