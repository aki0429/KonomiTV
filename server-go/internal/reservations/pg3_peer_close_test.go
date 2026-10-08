package reservations

import (
	"errors"
	"io"
	"net"
)

// pg3PeerClosed は試験用 peer の Read が「相手 (被試験側) が接続を閉じた」ことを示すかを判定する。
//
// 被試験側が未読データを残したまま Close すると、Linux/Windows は FIN ではなく RST を送るため、
// peer の Read は io.EOF ではなく connection reset になる (OS とタイミング依存) 。
// どちらも「閉じられた」証拠として扱い、peer 自身の deadline 切れ (= 被試験側が閉じずに
// 待ち続けた) だけを失敗として区別する。
func pg3PeerClosed(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && !netErr.Timeout()
}
