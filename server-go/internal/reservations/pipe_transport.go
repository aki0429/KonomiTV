package reservations

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/aki0429/KonomiTV/server-go/internal/edcbpipe"
)

// SendAndReceive は制御パイプ/Unix socket の一要求を送受信する。
// 分割応答でもヘッダーと本文を読み切り、キャンセル/期限で全 I/O を解除する。
func (t *pipeTransport) SendAndReceive(request []byte) (Response, error) {
	parent := t.Context
	if parent == nil {
		parent = context.Background()
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	connection, err := edcbpipe.DialControl(ctx, t.Path+t.Name)
	if err != nil {
		return Response{}, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return Response{}, fmt.Errorf("edcb: set pipe deadline: %w", err)
	}
	if _, err := io.Copy(connection, bytes.NewReader(request)); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	var header [8]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	code := int(int32(binary.LittleEndian.Uint32(header[:4])))
	size := int64(int32(binary.LittleEndian.Uint32(header[4:])))
	isSearch := len(request) >= 4 && binary.LittleEndian.Uint32(request[:4]) == 1025
	if size < 0 || ((!t.legacyResponseSizes || isSearch) && size > 64<<20) {
		return Response{}, fmt.Errorf("edcb: invalid response size %d", size)
	}
	// 申告長を先取り確保しない。旧 FileCopy のサイズ互換性は TCP と同じ扱いにする。
	payload, err := io.ReadAll(io.LimitReader(connection, size))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	if int64(len(payload)) != size {
		return Response{}, io.ErrUnexpectedEOF
	}
	return Response{Code: code, Data: payload}, nil
}
