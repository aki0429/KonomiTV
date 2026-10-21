package stream

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/edcbpipe"
)

// Connect はチューナーの TCP/ローカルパイプ受信を開始する。
func (t *EDCBTuner) Connect(ownerLiveStreamID string) (io.ReadCloser, error) {
	return t.ConnectContext(context.Background(), ownerLiveStreamID)
}

// ConnectContext は接続中のキャンセルと制御権限の変更を処理する。
// 移植元: EDCBTuner.connect() / EDCBUtil.openPipeStream()
func (t *EDCBTuner) ConnectContext(parent context.Context, owner string) (io.ReadCloser, error) {
	t.mu.Lock()
	if !t.isOwner(owner) || t.state == EDCBTunerCancelling || t.closing || t.edcbProcessID == nil {
		t.mu.Unlock()
		return nil, nil
	}
	processID := *t.edcbProcessID
	backend := t.backend
	if t.connectCancel != nil {
		t.connectCancel()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	t.connectCancel = cancel
	t.connectGeneration++
	generation := t.connectGeneration
	t.mu.Unlock()
	defer cancel()
	defer func() {
		t.mu.Lock()
		if t.connectGeneration == generation {
			t.connectCancel = nil
		}
		t.mu.Unlock()
	}()

	var connection io.ReadCloser
	var err error
	if backend.NamedPipe {
		opener := backend.OpenPipeStream
		if opener == nil {
			opener = func(ctx context.Context, pid int) (io.ReadCloser, error) {
				return edcbpipe.OpenStream(ctx, pid, "")
			}
		}
		connection, err = opener(ctx, processID)
	} else if backend.NewCtrlCmd == nil {
		err = fmt.Errorf("ctrl cmd client is not configured")
	} else {
		var client EDCBCtrlCmd
		client, err = backend.NewCtrlCmd()
		if err == nil {
			connection, err = client.RelayViewStream(processID)
		}
	}
	if err == nil && connection == nil {
		err = fmt.Errorf("stream connection is nil")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 待機中に handoff/close/disconnect された接続を旧所有者へ返さない。
	if ctx.Err() != nil || !t.isOwner(owner) || t.closing || t.state == EDCBTunerCancelling ||
		t.edcbProcessID == nil || *t.edcbProcessID != processID || t.connectGeneration != generation {
		if connection != nil {
			_ = connection.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	if err != nil {
		t.state = EDCBTunerIdle
		return nil, fmt.Errorf("edcb: failed to connect to tuner stream: %w", err)
	}
	stream := &edcbStreamConn{ReadCloser: connection}
	t.streamConn = stream
	t.state = EDCBTunerActive
	return stream, nil
}
