package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// cdpConnection は応答とイベントを分離し、イベント処理中の追加コマンドも受信できる接続。
type cdpConnection struct {
	ws      *websocket.Conn
	socket  net.Conn
	write   chan struct{}
	done    chan struct{}
	close   sync.Once
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan cdpMessage
	events  map[*cdpSubscription]struct{}
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// JSON の符号化とフレーム送信を分け、CPU 側の符号化が中止された場合は接続を壊さない。
var cdpTextFrame = websocket.Codec{Marshal: func(value any) ([]byte, byte, error) {
	return value.([]byte), websocket.TextFrame, nil
}}

func dialCDP(ctx context.Context, endpoint string) (*cdpConnection, error) {
	config, err := websocket.NewConfig(endpoint, "http://localhost")
	if err != nil {
		return nil, err
	}
	if config.Location.Scheme != "ws" {
		return nil, fmt.Errorf("browser CDP endpoint must use ws")
	}
	socket, err := (&net.Dialer{}).DialContext(ctx, "tcp", config.Location.Host)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = socket.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	ws, err := websocket.NewClient(config, socket)
	stop()
	if err != nil || ctx.Err() != nil {
		_ = socket.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	_ = socket.SetDeadline(time.Time{})
	ws.MaxPayloadBytes = 64 * 1024 * 1024
	c := &cdpConnection{ws: ws, socket: socket, write: make(chan struct{}, 1), done: make(chan struct{}),
		pending: map[int64]chan cdpMessage{}, events: map[*cdpSubscription]struct{}{}}
	c.write <- struct{}{}
	go c.receive()
	return c, nil
}

func (c *cdpConnection) receive() {
	defer c.Close()
	for {
		var message cdpMessage
		if err := websocket.JSON.Receive(c.ws, &message); err != nil {
			return
		}
		c.mu.Lock()
		if message.ID != 0 {
			if pending := c.pending[message.ID]; pending != nil {
				pending <- message
				delete(c.pending, message.ID)
			}
		} else {
			for sub := range c.events {
				if sub.session == message.SessionID && sub.methods[message.Method] {
					sub.mu.Lock()
					sub.queue = append(sub.queue, message)
					sub.mu.Unlock()
					select {
					case sub.ready <- struct{}{}:
					default:
					}
				}
			}
		}
		c.mu.Unlock()
	}
}

func (c *cdpConnection) Close() {
	c.close.Do(func() {
		close(c.done)
		// WebSocket の Close は close フレーム送信で待つ場合がある。実ソケットを閉じて Read/Write の両方を解除する。
		_ = c.socket.Close()
	})
}

func (c *cdpConnection) call(ctx context.Context, session, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	response := make(chan cdpMessage, 1)
	c.pending[id] = response
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	request := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		request["sessionId"] = session
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err := c.sendFrame(ctx, encoded); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrBrowserConnectionLost
	case message := <-response:
		if message.Error != nil {
			return fmt.Errorf("cdp %s: %d: %s", method, message.Error.Code, message.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(message.Result, result)
		}
		return nil
	}
}

func (c *cdpConnection) sendFrame(ctx context.Context, encoded []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrBrowserConnectionLost
	case <-c.write:
	}
	deadline := time.Now().Add(30 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = c.ws.SetWriteDeadline(deadline)
	// フレーム送信途中の中止は接続ごと終了し、壊れたフレームを次の要求へ引き継がない。
	stop := context.AfterFunc(ctx, c.Close)
	err := cdpTextFrame.Send(c.ws, encoded)
	stop()
	c.write <- struct{}{}
	if err != nil {
		c.Close()
		return fmt.Errorf("%w: %v", ErrBrowserConnectionLost, err)
	}
	return nil
}

// cdpSubscription は順序を保つイベント待ち行列。応答待ちによる受信ループの停止やイベントの黙殺を避ける。
type cdpSubscription struct {
	connection *cdpConnection
	session    string
	methods    map[string]bool
	mu         sync.Mutex
	queue      []cdpMessage
	ready      chan struct{}
	done       chan struct{}
	close      sync.Once
}

func (c *cdpConnection) subscribe(session string, methods ...string) *cdpSubscription {
	s := &cdpSubscription{connection: c, session: session, methods: map[string]bool{}, ready: make(chan struct{}, 1), done: make(chan struct{})}
	for _, method := range methods {
		s.methods[method] = true
	}
	c.mu.Lock()
	c.events[s] = struct{}{}
	c.mu.Unlock()
	return s
}

func (s *cdpSubscription) Close() {
	s.close.Do(func() {
		s.connection.mu.Lock()
		delete(s.connection.events, s)
		s.connection.mu.Unlock()
		s.mu.Lock()
		s.queue = nil
		close(s.done)
		s.mu.Unlock()
	})
}

func (s *cdpSubscription) next(ctx context.Context) (cdpMessage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return cdpMessage{}, err
		}
		s.mu.Lock()
		if len(s.queue) > 0 {
			message := s.queue[0]
			s.queue[0] = cdpMessage{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return message, nil
		}
		s.mu.Unlock()
		select {
		case <-s.ready:
		case <-ctx.Done():
			return cdpMessage{}, ctx.Err()
		case <-s.connection.done:
			return cdpMessage{}, ErrBrowserConnectionLost
		case <-s.done:
			return cdpMessage{}, context.Canceled
		}
	}
}

func (c *cdpConnection) evaluate(ctx context.Context, session, expression string, await bool, value any) error {
	var response struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": await, "returnByValue": true,
	}, &response); err != nil {
		return err
	}
	if len(response.ExceptionDetails) > 0 && string(response.ExceptionDetails) != "null" {
		return fmt.Errorf("failed to execute JavaScript: %s", response.ExceptionDetails)
	}
	if value == nil {
		return nil
	}
	if len(response.Result.Value) == 0 {
		return fmt.Errorf("browser returned no JavaScript value")
	}
	return json.Unmarshal(response.Result.Value, value)
}
