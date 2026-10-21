package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// BrowserEvent は CDP の通知。購読していない通知は保持しない。
type BrowserEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// BrowserSession は Chromium の CDP 接続とプロセス寿命を抽象化する。
// 隔離テストでは同じ契約を満たすモックを使用する。
type BrowserSession interface {
	Call(context.Context, string, any, any) error
	Subscribe(...string) (<-chan BrowserEvent, func())
	Done() <-chan struct{}
	Close(context.Context) error
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	BrowserEvent
}

type cdpSubscriber struct {
	methods map[string]bool
	events  chan BrowserEvent
}

// cdpSession は read を一つに集約し、command id ごとに応答を配る。
// write と read は別 goroutine。pending/通知は上限付きで、遅い購読者で無制限に蓄積しない。
type cdpSession struct {
	socket      *websocket.Conn
	writeMu     sync.Mutex
	mu          sync.Mutex
	nextID      int64
	pending     map[int64]chan cdpResponse
	subscribers map[*cdpSubscriber]struct{}
	done        chan struct{}
	closeOnce   sync.Once
	stopProcess func(context.Context) error
}

func newCDPSession(ctx context.Context, address string) (*cdpSession, error) {
	socket, response, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, address, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect browser CDP: %w", err)
	}
	socket.SetReadLimit(64 << 20)
	session := &cdpSession{socket: socket, pending: map[int64]chan cdpResponse{}, subscribers: map[*cdpSubscriber]struct{}{}, done: make(chan struct{})}
	go session.readLoop()
	return session, nil
}

func (s *cdpSession) Done() <-chan struct{} { return s.done }

func (s *cdpSession) readLoop() {
	defer s.disconnect()
	for {
		var message cdpResponse
		if err := s.socket.ReadJSON(&message); err != nil {
			return
		}
		s.mu.Lock()
		if message.ID != 0 {
			if pending := s.pending[message.ID]; pending != nil {
				pending <- message
				delete(s.pending, message.ID)
			}
		} else {
			for subscriber := range s.subscribers {
				if !subscriber.methods[message.Method] {
					continue
				}
				select {
				case subscriber.events <- message.BrowserEvent:
				default:
					// 必須イベントを黙って捨てると投稿成功を誤判定するため接続を失敗させる。
					s.mu.Unlock()
					return
				}
			}
		}
		s.mu.Unlock()
	}
}

func (s *cdpSession) disconnect() {
	s.closeOnce.Do(func() { close(s.done); _ = s.socket.Close() })
}

func (s *cdpSession) Call(ctx context.Context, method string, params, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if len(s.pending) >= 64 {
		s.mu.Unlock()
		return errors.New("browser CDP command capacity exceeded")
	}
	s.nextID++
	id := s.nextID
	response := make(chan cdpResponse, 1)
	s.pending[id] = response
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()

	s.writeMu.Lock()
	deadline := time.Now().Add(10 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = s.socket.SetWriteDeadline(deadline)
	err := s.socket.WriteJSON(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params})
	s.writeMu.Unlock()
	if err != nil {
		s.disconnect()
		return ErrBrowserConnectionLost
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrBrowserConnectionLost
	case value := <-response:
		if value.Error != nil {
			// CDP のメッセージに JS/認証本文が含まれる可能性があるので外へそのまま出さない。
			return fmt.Errorf("browser CDP %s failed (code %d)", method, value.Error.Code)
		}
		if output != nil {
			return json.Unmarshal(value.Result, output)
		}
		return nil
	}
}

func (s *cdpSession) Subscribe(methods ...string) (<-chan BrowserEvent, func()) {
	subscriber := &cdpSubscriber{methods: map[string]bool{}, events: make(chan BrowserEvent, 256)}
	for _, method := range methods {
		subscriber.methods[method] = true
	}
	s.mu.Lock()
	s.subscribers[subscriber] = struct{}{}
	s.mu.Unlock()
	return subscriber.events, func() { s.mu.Lock(); delete(s.subscribers, subscriber); s.mu.Unlock() }
}

func (s *cdpSession) Close(ctx context.Context) error {
	// Browser.close は返信前に socket が閉じる場合がある。所有プロセスの終了確認で判定する。
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	_ = s.Call(closeCtx, "Browser.close", nil, nil)
	cancel()
	s.disconnect()
	if s.stopProcess != nil {
		return s.stopProcess(ctx)
	}
	return nil
}
