package stream

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/edcbpipe"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"github.com/aki0429/KonomiTV/server-go/internal/testutil/edcbipc"
)

// このファイルは EDCBTuner の単体テスト。実機の EDCB には接続せず、
// EDCBCtrlCmd をフェイクに差し替えて挙動を検証する。

// fakeEDCBCtrlCmd は実機に接続しないテスト用の CtrlCmd クライアント。
type fakeEDCBCtrlCmd struct {
	mu sync.Mutex

	// setChFunc は NwTVIDSetCh の戻り値 (nil の場合は失敗を返す) 。
	setChFunc func() (int, bool)
	// closeFunc は NwTVIDClose の戻り値 (nil の場合は常に true) 。
	closeFunc func() bool
	// relayFunc は RelayViewStream の戻り値。
	relayFunc func() (net.Conn, error)

	// setChCalls は NwTVIDSetCh の呼び出し回数。
	setChCalls int
	// lastSetChInfo は最後に渡されたチャンネル設定情報。
	lastSetChInfo reservations.SetChInfo
	// closeIDs は NwTVIDClose に渡された NetworkTV ID の履歴。
	closeIDs []int
}

func (f *fakeEDCBCtrlCmd) NwTVIDSetCh(info reservations.SetChInfo) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setChCalls++
	f.lastSetChInfo = info
	if f.setChFunc == nil {
		return 0, false
	}
	return f.setChFunc()
}

func (f *fakeEDCBCtrlCmd) NwTVIDClose(nwtvID int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeIDs = append(f.closeIDs, nwtvID)
	if f.closeFunc == nil {
		return true
	}
	return f.closeFunc()
}

func (f *fakeEDCBCtrlCmd) RelayViewStream(processID int) (net.Conn, error) {
	f.mu.Lock()
	relay := f.relayFunc
	f.mu.Unlock()
	if relay == nil {
		return nil, errors.New("the relay view stream is not configured")
	}
	return relay()
}

// fakeEDCBTunerBackend はフェイクの CtrlCmd を返すバックエンドを生成する。
func fakeEDCBTunerBackend(ctrl *fakeEDCBCtrlCmd) EDCBTunerBackend {
	return EDCBTunerBackend{
		NewCtrlCmd: func() (EDCBCtrlCmd, error) { return ctrl, nil },
	}
}

// resetEDCBTunerPool はテスト間でチューナープールを初期化する。
func resetEDCBTunerPool() {
	edcbTunerPoolMu.Lock()
	edcbTunerInstances = nil
	edcbNextNetworkTVID = edcbNetworkTVIDStart
	edcbTunerPoolMu.Unlock()
}

// TestEDCBTunerSetChannelRetries は setChannel が失敗時にリトライし、成功したら状態が Active になることを検証する。
func TestEDCBTunerSetChannelRetries(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{}
	attempts := 0
	ctrl.setChFunc = func() (int, bool) {
		attempts++
		if attempts < 3 {
			return 0, false
		}
		return 4242, true
	}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	if !tuner.SetChannel(0x7881, 0x0400, 0x7A76, "stream-a") {
		t.Fatalf("SetChannel() = false, want true after retries")
	}
	if ctrl.setChCalls != 3 {
		t.Errorf("NwTVIDSetCh calls = %d, want 3", ctrl.setChCalls)
	}
	if tuner.GetState() != EDCBTunerActive {
		t.Errorf("state = %q, want Active", tuner.GetState())
	}

	// Python 版 __writeSetChInfo と同じ値 (space_or_id / ch_or_mode / use_sid / use_bon_ch) が渡る
	info := ctrl.lastSetChInfo
	if info.Onid != 0x7881 || info.Sid != 0x0400 || info.Tsid != 0x7A76 {
		t.Errorf("set ch info NID/SID/TSID = %X/%X/%X", info.Onid, info.Sid, info.Tsid)
	}
	if info.SpaceOrID != tuner.GetEDCBNetworkTVID() {
		t.Errorf("space_or_id = %d, want %d", info.SpaceOrID, tuner.GetEDCBNetworkTVID())
	}
	if info.ChOrMode != 2 {
		t.Errorf("ch_or_mode = %d, want 2 (TCP)", info.ChOrMode)
	}
	if !info.UseSid || !info.UseBonCh {
		t.Errorf("use_sid = %v, use_bon_ch = %v, want both true", info.UseSid, info.UseBonCh)
	}
}

// TestEDCBTunerSetChannelFailsAfterTimeout は 5 秒間リトライしてもプロセス ID が取得できなければ
// 失敗を返し、状態が Idle に戻ることを検証する。
func TestEDCBTunerSetChannelFailsAfterTimeout(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 0, false }}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	if tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = true, want false when the tuner never starts")
	}
	if tuner.GetState() != EDCBTunerIdle {
		t.Errorf("state = %q, want Idle after failure", tuner.GetState())
	}
	// 5 秒間リトライするため複数回呼ばれる
	if ctrl.setChCalls < 2 {
		t.Errorf("NwTVIDSetCh calls = %d, want >= 2 (retried)", ctrl.setChCalls)
	}
}

// TestEDCBTunerSetChannelOwnerGuard は制御権限がない場合・キャンセル中の場合に setChannel が弾かれることを検証する。
func TestEDCBTunerSetChannelOwnerGuard(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 1, true }}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	// 制御権限が一致していない
	if tuner.SetChannel(1, 2, 3, "stream-b") {
		t.Errorf("SetChannel() with a different owner = true, want false")
	}
	if ctrl.setChCalls != 0 {
		t.Errorf("NwTVIDSetCh calls = %d, want 0", ctrl.setChCalls)
	}

	// キャンセル中
	tuner.SetState(EDCBTunerCancelling)
	if tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Errorf("SetChannel() while cancelling = true, want false")
	}
	if ctrl.setChCalls != 0 {
		t.Errorf("NwTVIDSetCh calls = %d, want 0", ctrl.setChCalls)
	}
}

// TestEDCBTunerConnectFailure は connect に失敗した場合に nil を返し、状態が Idle に戻ることを検証する。
func TestEDCBTunerConnectFailure(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{
		setChFunc: func() (int, bool) { return 9000, true },
		relayFunc: func() (net.Conn, error) { return nil, errors.New("boom") },
	}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	if !tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = false, want true")
	}
	reader, err := tuner.Connect("stream-a")
	if reader != nil {
		t.Errorf("Connect() reader = %v, want nil on failure", reader)
	}
	if err == nil {
		t.Errorf("Connect() error = nil, want an error")
	}
	if tuner.GetState() != EDCBTunerIdle {
		t.Errorf("state = %q, want Idle after a failed connect", tuner.GetState())
	}
	// 接続していないので切断扱いになる
	if !tuner.IsDisconnected() {
		t.Errorf("IsDisconnected() = false, want true when not connected")
	}
}

// TestEDCBTunerConnectAndDisconnect は connect の成功と IsDisconnected / disconnect の挙動を検証する。
func TestEDCBTunerConnectAndDisconnect(t *testing.T) {
	resetEDCBTunerPool()
	client, server := net.Pipe()
	defer server.Close()

	ctrl := &fakeEDCBCtrlCmd{
		setChFunc: func() (int, bool) { return 9000, true },
		relayFunc: func() (net.Conn, error) { return client, nil },
	}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	if !tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = false, want true")
	}
	reader, err := tuner.Connect("stream-a")
	if reader == nil || err != nil {
		t.Fatalf("Connect() = (%v, %v), want a connection", reader, err)
	}
	if tuner.GetState() != EDCBTunerActive {
		t.Errorf("state = %q, want Active", tuner.GetState())
	}
	if tuner.IsDisconnected() {
		t.Errorf("IsDisconnected() = true, want false right after connecting")
	}

	// 制御権限がない場合は切断しない
	tuner.Disconnect("stream-b")
	if tuner.IsDisconnected() {
		t.Errorf("IsDisconnected() = true, want false when a non-owner disconnects")
	}

	// 制御権限のあるオーナーが切断すると切断扱いになる
	tuner.Disconnect("stream-a")
	if !tuner.IsDisconnected() {
		t.Errorf("IsDisconnected() = true, want true after disconnecting")
	}
}

// TestEDCBTunerDetectsRemoteClose は相手側からの切断 (読み取りの EOF) を IsDisconnected が検出することを検証する。
func TestEDCBTunerDetectsRemoteClose(t *testing.T) {
	resetEDCBTunerPool()
	client, server := net.Pipe()

	ctrl := &fakeEDCBCtrlCmd{
		setChFunc: func() (int, bool) { return 9000, true },
		relayFunc: func() (net.Conn, error) { return client, nil },
	}

	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	if !tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = false, want true")
	}
	reader, err := tuner.Connect("stream-a")
	if reader == nil || err != nil {
		t.Fatalf("Connect() = (%v, %v), want a connection", reader, err)
	}

	// 相手側 (チューナー) が切断する
	server.Close()

	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); err == nil {
		t.Fatalf("Read() after the remote close should fail")
	}
	if !tuner.IsDisconnected() {
		t.Errorf("IsDisconnected() = false, want true after the remote close")
	}
}

// TestEDCBTunerLockUnlockHandoff は lock / unlock / handoff の挙動を検証する。
func TestEDCBTunerLockUnlockHandoff(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{}
	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	defer tuner.Close("stream-a", true)

	// 制御権限が一致しない場合はロック/アンロックできない
	if tuner.Lock("stream-b") {
		t.Errorf("Lock() with a different owner = true, want false")
	}
	if tuner.Unlock("stream-b") {
		t.Errorf("Unlock() with a different owner = true, want false")
	}
	if !tuner.Lock("stream-a") {
		t.Errorf("Lock() = false, want true")
	}
	if !tuner.Unlock("stream-a") {
		t.Errorf("Unlock() = false, want true")
	}

	// 制御権限が一致しない場合は移譲できない
	if tuner.Handoff("stream-b", "stream-c") {
		t.Errorf("Handoff() with a different owner = true, want false")
	}
	tuner.SetState(EDCBTunerActive)
	if !tuner.Handoff("stream-a", "stream-b") {
		t.Fatalf("Handoff() = false, want true")
	}
	// 移譲後は Idle に戻り、新しいオーナーで操作できる
	if tuner.GetState() != EDCBTunerIdle {
		t.Errorf("state = %q, want Idle after handoff", tuner.GetState())
	}
	if !tuner.Lock("stream-b") {
		t.Errorf("Lock() by the new owner = false, want true")
	}
}

// TestEDCBTunerClose は close が制御権限を確認し、NwTVIDClose を送ってインスタンスをプールから削除することを検証する。
func TestEDCBTunerClose(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 9000, true }}
	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	networkTVID := tuner.GetEDCBNetworkTVID()

	if !tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = false, want true")
	}

	// 制御権限が一致しない場合は閉じない
	if tuner.Close("stream-b", false) {
		t.Errorf("Close() with a different owner = true, want false")
	}
	if len(ctrl.closeIDs) != 0 {
		t.Errorf("NwTVIDClose calls = %v, want none", ctrl.closeIDs)
	}

	// 制御権限が一致すれば閉じる
	if !tuner.Close("stream-a", false) {
		t.Errorf("Close() = false, want true")
	}
	if len(ctrl.closeIDs) != 1 || ctrl.closeIDs[0] != networkTVID {
		t.Errorf("NwTVIDClose IDs = %v, want [%d]", ctrl.closeIDs, networkTVID)
	}
	if tuner.GetState() != EDCBTunerIdle {
		t.Errorf("state = %q, want Idle after close", tuner.GetState())
	}

	// プールから削除されたので、次の採番は進んだ値になる
	next := GetOrCreateEDCBTuner("stream-c", fakeEDCBTunerBackend(ctrl))
	defer next.Close("stream-c", true)
	if next.GetEDCBNetworkTVID() != networkTVID+1 {
		t.Errorf("next NetworkTV ID = %d, want %d", next.GetEDCBNetworkTVID(), networkTVID+1)
	}
}

// TestEDCBTunerCloseForce は force 指定時に制御権限を無視して閉じられることを検証する。
func TestEDCBTunerCloseForce(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{}
	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))

	if !tuner.Close("", true) {
		t.Errorf("Close(force) = false, want true")
	}
	if len(ctrl.closeIDs) != 1 {
		t.Errorf("NwTVIDClose calls = %v, want 1 call", ctrl.closeIDs)
	}
}

// TestEDCBTunerCloseFailureKeepsResult は NwTVIDClose が失敗した場合に false を返すことを検証する。
func TestEDCBTunerCloseFailureKeepsResult(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{closeFunc: func() bool { return false }}
	tuner := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))

	if tuner.Close("stream-a", false) {
		t.Errorf("Close() = true, want false when NwTVIDClose fails")
	}
}

// TestEDCBTunerNamedPipe は OS の実パイプ経由で受信し、旧所有者からの切断を拒否することを検証する。
func TestEDCBTunerNamedPipe(t *testing.T) {
	resetEDCBTunerPool()
	directory := edcbipc.Directory(t)
	accept := edcbipc.StreamListener(t, edcbipc.StreamPath(directory, 3, 9000, 0))
	ctrl := &fakeEDCBCtrlCmd{setChFunc: func() (int, bool) { return 9000, true }}
	backend := fakeEDCBTunerBackend(ctrl)
	backend.NamedPipe = true
	backend.OpenPipeStream = func(ctx context.Context, processID int) (io.ReadCloser, error) {
		return edcbpipe.OpenStream(ctx, processID, directory)
	}
	tuner := GetOrCreateEDCBTuner("stream-a", backend)
	defer tuner.Close("stream-a", true)

	if !tuner.SetChannel(1, 2, 3, "stream-a") {
		t.Fatalf("SetChannel() = false, want true")
	}
	reader, err := tuner.Connect("stream-a")
	if reader == nil || err != nil {
		t.Fatalf("Connect() in named pipe mode = (%v, %v)", reader, err)
	}
	writer, err := accept()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tuner.Disconnect("stream-b")
	if tuner.IsDisconnected() {
		t.Fatal("another owner disconnected the pipe")
	}
	if _, err := writer.Write([]byte{0x47, 0, 0x10}); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3)
	if _, err := io.ReadFull(reader, data); err != nil || data[0] != 0x47 {
		t.Fatalf("stream read = %v, %v", data, err)
	}
	done := make(chan error, 1)
	go func() { _, err := reader.Read(data); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Read returned before Disconnect: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	tuner.Disconnect("stream-a")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnected read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not interrupt pipe read")
	}
	if !tuner.IsDisconnected() || tuner.GetState() != EDCBTunerIdle {
		t.Fatal("disconnect state was not recorded")
	}
}

// TestEDCBTunerNetworkTVIDAllocationAndReuse は NetworkTV ID が 500 から採番され、
// 再利用可能なチューナーが使い回されることを検証する。
func TestEDCBTunerNetworkTVIDAllocationAndReuse(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{}

	first := GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	if first.GetEDCBNetworkTVID() != 500 {
		t.Errorf("first NetworkTV ID = %d, want 500", first.GetEDCBNetworkTVID())
	}
	second := GetOrCreateEDCBTuner("stream-b", fakeEDCBTunerBackend(ctrl))
	if second.GetEDCBNetworkTVID() != 501 {
		t.Errorf("second NetworkTV ID = %d, want 501", second.GetEDCBNetworkTVID())
	}
	if second == first {
		t.Fatalf("a tuner owned by another live stream must not be reused")
	}

	// first を再利用可能な状態 (Idle・未割り当て・未ロック) にする
	first.mu.Lock()
	first.state = EDCBTunerIdle
	first.ownerLiveStreamID = ""
	first.locked = false
	first.mu.Unlock()

	reused := GetOrCreateEDCBTuner("stream-c", fakeEDCBTunerBackend(ctrl))
	if reused != first {
		t.Errorf("GetOrCreateEDCBTuner() should reuse the idle tuner instance")
	}
	if reused.GetEDCBNetworkTVID() != first.GetEDCBNetworkTVID() {
		t.Errorf("a reused tuner must keep its NetworkTV ID")
	}

	// 後始末 (プールはテストごとに初期化されるが、実機へは接続しない)
	_ = first.Close("", true)
	_ = second.Close("", true)
}

// TestEDCBTunerCloseAll は CloseAllEDCBTuners が起動中のチューナーを全て終了することを検証する。
func TestEDCBTunerCloseAll(t *testing.T) {
	resetEDCBTunerPool()
	ctrl := &fakeEDCBCtrlCmd{}
	GetOrCreateEDCBTuner("stream-a", fakeEDCBTunerBackend(ctrl))
	GetOrCreateEDCBTuner("stream-b", fakeEDCBTunerBackend(ctrl))

	CloseAllEDCBTuners()

	edcbTunerPoolMu.Lock()
	remaining := len(edcbTunerInstances)
	edcbTunerPoolMu.Unlock()
	if remaining != 0 {
		t.Errorf("remaining tuner instances = %d, want 0", remaining)
	}
	if len(ctrl.closeIDs) != 2 {
		t.Errorf("NwTVIDClose calls = %d, want 2", len(ctrl.closeIDs))
	}
}
