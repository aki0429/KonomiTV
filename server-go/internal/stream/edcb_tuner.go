package stream

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/utils/edcb/EDCBTuner.py の EDCBTuner を移植したもの。
//
// EDCB (EpgDataCap_Bon の NetworkTV モード) のチューナーを制御し、放送波の MPEG2-TS を
// 受け取るためのストリーミング接続を提供する。チューナーは閉じずに再利用するため、
// パッケージ変数のプールで全インスタンスを管理する。
//
// 実機の EDCB へ接続するテストは行わないため、CtrlCmd の通信層は EDCBCtrlCmd インターフェースで
// 抽象化し、テストではフェイクに差し替えられるようにしている。

// EDCBTunerState はチューナーの利用状態 (Python 版 EDCBTunerState) 。
type EDCBTunerState string

const (
	// EDCBTunerIdle は未使用の状態。
	EDCBTunerIdle EDCBTunerState = "Idle"
	// EDCBTunerStarting は起動・チャンネル切り替え中の状態。
	EDCBTunerStarting EDCBTunerState = "Starting"
	// EDCBTunerActive は受信中の状態。
	EDCBTunerActive EDCBTunerState = "Active"
	// EDCBTunerCancelling はキャンセル中 (別のライブストリームへ制御を移譲している途中) の状態。
	EDCBTunerCancelling EDCBTunerState = "Cancelling"
)

const (
	// edcbNetworkTVIDStart は NetworkTV モードのチューナーを識別する NetworkTV ID の開始値。
	// ほかのロケフリ系アプリと重複しないように 500 から増分する (Python 版 __next_networktv_id) 。
	edcbNetworkTVIDStart = 500
	// edcbSetChannelTimeout はチャンネル設定のリトライを打ち切るまでの時間 (Python 版の 5 秒) 。
	edcbSetChannelTimeout = 5 * time.Second
	// edcbSetChannelRetryInterval はチャンネル設定のリトライ間隔 (Python 版の 0.5 秒) 。
	edcbSetChannelRetryInterval = 500 * time.Millisecond
	// edcbTCPMode は EpgDataCap_Bon の起動モード (2:TCP) (Python 版の ch_or_mode=2) 。
	edcbTCPMode = 2
)

var (
	// edcbTunerPoolMu は edcbTunerInstances と edcbNextNetworkTVID を保護する。
	edcbTunerPoolMu sync.Mutex
	// edcbTunerInstances は生成済みのチューナーインスタンス (Python 版 EDCBTuner.__instances) 。
	// チューナーを閉じずに再利用するため、全てのチューナーインスタンスにアクセスできるようにする。
	edcbTunerInstances []*EDCBTuner
	// edcbNextNetworkTVID は次に払い出す NetworkTV ID。
	edcbNextNetworkTVID = edcbNetworkTVIDStart
)

// EDCBCtrlCmd は EDCBTuner が利用する EDCB CtrlCmd の操作。
//
// テストでは実機に接続しないフェイクに差し替えられるようにインターフェースにしている。
// reservations.Client がそのままこのインターフェースを満たす。
type EDCBCtrlCmd interface {
	// NwTVIDSetCh はチャンネルを設定し、EpgDataCap_Bon のプロセス ID を返す。
	NwTVIDSetCh(info reservations.SetChInfo) (int, bool)
	// NwTVIDClose は NetworkTV モードで起動中の View アプリを終了する。
	NwTVIDClose(nwtvID int) bool
	// RelayViewStream は放送波の TS ストリームの転送を開始し、生の接続を返す。
	RelayViewStream(processID int) (net.Conn, error)
}

// EDCBTunerBackend は EDCBTuner が EDCB と通信するための依存関係。
type EDCBTunerBackend struct {
	// NewCtrlCmd は CtrlCmd クライアントを生成する (Python 版が各メソッドで CtrlCmdUtil() を都度生成するのと同じ) 。
	NewCtrlCmd func() (EDCBCtrlCmd, error)
	// NamedPipe は名前付きパイプモードで動作しているかどうか (真の場合は TCP モードではない) 。
	NamedPipe bool
}

// EDCBTuner は EDCB バックエンドのチューナーを制御する。
//
// 状態は EDCBTunerIdle → EDCBTunerStarting → EDCBTunerActive → (再利用時は EDCBTunerCancelling) と遷移する。
// 各メソッドは mu で状態を保護し、mu を保持したままプールのロックを取得しないことでデッドロックを避ける。
type EDCBTuner struct {
	// backend は EDCB と通信するための依存関係。生成時に固定される。
	backend EDCBTunerBackend
	// networkTVID は EpgDataCap_Bon の NetworkTV ID。生成時に払い出され、以後変わらない。
	networkTVID int

	// mu はこのチューナーの状態 (state / ownerLiveStreamID / locked / edcbProcessID / streamConn / closing) を保護する。
	mu sync.Mutex
	// networkID は setChannel で設定するネットワーク ID。
	networkID int
	// serviceID は setChannel で設定するサービス ID。
	serviceID int
	// transportStreamID は setChannel で設定するトランスポートストリーム ID。
	transportStreamID int
	// locked はチューナーがロックされているかどうか (ONAir 時はロック、Idling 時はアンロックされる) 。
	locked bool
	// edcbProcessID は EpgDataCap_Bon のプロセス ID (nil のときはチューナーが起動していない) 。
	edcbProcessID *int
	// streamConn はチューナーとのストリーミング接続 (未接続時は nil) 。
	streamConn *edcbStreamConn
	// state はこのチューナーの利用状態。
	state EDCBTunerState
	// ownerLiveStreamID はこのチューナーの制御権限を持つライブストリーム ID (空文字は未割り当て) 。
	ownerLiveStreamID string
	// closing は close 処理中かどうか (Go 版固有) 。
	// プールからの削除と再利用の競合で、閉じかけのチューナーが再利用されないようにするためのガード。
	closing bool
}

// newEDCBTuner はチューナーインスタンスを初期化する。
// 直接呼び出してはいけない (必ず GetOrCreateEDCBTuner() を経由すること) 。
func newEDCBTuner(networkTVID int, ownerLiveStreamID string, backend EDCBTunerBackend) *EDCBTuner {
	return &EDCBTuner{
		backend:           backend,
		networkTVID:       networkTVID,
		state:             EDCBTunerIdle,
		ownerLiveStreamID: ownerLiveStreamID,
	}
}

// GetOrCreateEDCBTuner は再利用可能なチューナーインスタンスを取得する。
// 再利用できるチューナーがない場合は新規に生成する。
//
// 移植元: EDCBTuner.getOrCreate()
// backend は新規生成時にのみ使われ、既存インスタンスを再利用する場合は無視される。
func GetOrCreateEDCBTuner(ownerLiveStreamID string, backend EDCBTunerBackend) *EDCBTuner {
	// インスタンスの再利用/生成は排他制御する (競合した場合に同じチューナーを二重取得しないようにする) 。
	edcbTunerPoolMu.Lock()
	defer edcbTunerPoolMu.Unlock()

	// 再利用可能なチューナーインスタンスがあれば、それを返す。
	for _, instance := range edcbTunerInstances {
		if instance.tryTake(ownerLiveStreamID) {
			return instance
		}
	}

	// NetworkTV ID を払い出す。
	networkTVID := edcbNextNetworkTVID
	edcbNextNetworkTVID++

	// 新しいチューナーインスタンスを生成して登録する。
	instance := newEDCBTuner(networkTVID, ownerLiveStreamID, backend)
	edcbTunerInstances = append(edcbTunerInstances, instance)
	return instance
}

// removeEDCBTunerInstance はチューナーインスタンスの登録を削除する。
func removeEDCBTunerInstance(target *EDCBTuner) {
	edcbTunerPoolMu.Lock()
	defer edcbTunerPoolMu.Unlock()
	for index, instance := range edcbTunerInstances {
		if instance == target {
			edcbTunerInstances = append(edcbTunerInstances[:index], edcbTunerInstances[index+1:]...)
			return
		}
	}
}

// CloseAllEDCBTuners は現在起動中の全てのチューナーを終了する。
//
// 移植元: EDCBTuner.closeAll()
// 明示的に終了しないといつまでも起動してしまうため、アプリケーション終了時に実行する。
func CloseAllEDCBTuners() {
	edcbTunerPoolMu.Lock()
	instances := append([]*EDCBTuner{}, edcbTunerInstances...)
	edcbTunerPoolMu.Unlock()

	for _, instance := range instances {
		instance.mu.Lock()
		owner := instance.ownerLiveStreamID
		instance.mu.Unlock()
		instance.Close(owner, true)
	}
}

// tryTake はチューナーが再利用可能なら owner を設定して true を返す (呼び出し元でプールのロックを取得すること) 。
func (t *EDCBTuner) tryTake(ownerLiveStreamID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 未使用かつ未割り当てかつ未ロックかつ close 処理中でなければ再利用する。
	if t.state != EDCBTunerIdle || t.ownerLiveStreamID != "" || t.locked || t.closing {
		return false
	}
	t.ownerLiveStreamID = ownerLiveStreamID
	return true
}

// isOwner は指定されたライブストリームがこのチューナーの制御権限を持っているかどうかを返す。
// (呼び出し元で mu を取得すること)
func (t *EDCBTuner) isOwner(liveStreamID string) bool {
	return t.ownerLiveStreamID != "" && t.ownerLiveStreamID == liveStreamID
}

// GetEDCBNetworkTVID は EpgDataCap_Bon の NetworkTV ID を取得する。
func (t *EDCBTuner) GetEDCBNetworkTVID() int {
	return t.networkTVID
}

// GetState はチューナーの現在の状態を取得する。
func (t *EDCBTuner) GetState() EDCBTunerState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// SetState はチューナーの状態を設定する (制御権限のチェックは行わない) 。
func (t *EDCBTuner) SetState(state EDCBTunerState) {
	t.mu.Lock()
	t.state = state
	t.mu.Unlock()
}

// SetChannel はチューナーのチャンネルを設定し、チューナーが起動していない場合は起動する。
//
// 移植元: EDCBTuner.setChannel()
// 起動は空きチューナー不足などで失敗することがあるため、失敗した場合は 0.5 秒間隔で
// 最大 5 秒間リトライする。成功した場合のみプロセス ID を保持して true を返す。
func (t *EDCBTuner) SetChannel(networkID int, serviceID int, transportStreamID int, ownerLiveStreamID string) bool {
	t.mu.Lock()
	// 制御権限が一致していない場合、キャンセル中の場合は処理しない。
	if !t.isOwner(ownerLiveStreamID) || t.state == EDCBTunerCancelling {
		t.mu.Unlock()
		return false
	}
	// NID・SID・TSID を更新する。
	t.networkID = networkID
	t.serviceID = serviceID
	t.transportStreamID = transportStreamID
	t.state = EDCBTunerStarting
	creator := t.backend.NewCtrlCmd
	networkTVID := t.networkTVID
	t.mu.Unlock()

	// EpgDataCap_Bon に渡すチャンネル・NetworkTV モード変更情報。
	setChInfo := reservations.SetChInfo{
		// NID・SID・TSID を設定する。
		Onid: networkID,
		Sid:  serviceID,
		Tsid: transportStreamID,
		// NetworkTV ID をセットする。
		SpaceOrID: networkTVID,
		// TCP 送信を有効にする (EpgDataCap_Bon の起動モード) 。
		ChOrMode: edcbTCPMode,
		// onid・tsid・sid の値が使用できるかどうか (起動確認とプロセス ID の取得に必要) 。
		UseSid: true,
		// space_or_id・ch_or_mode の値が使用できるかどうか。
		UseBonCh: true,
	}

	// チューナーを起動する (ほかのタスクがチューナーを閉じている最中などで失敗する場合があるのでリトライする) 。
	deadline := time.Now().Add(edcbSetChannelTimeout)
	var processID *int
	for {
		if creator != nil {
			if client, err := creator(); err == nil {
				if value, ok := client.NwTVIDSetCh(setChInfo); ok {
					processID = &value
				} else {
					processID = nil
				}
			} else {
				processID = nil
			}
		} else {
			processID = nil
		}

		// チューナーが起動できた、もしくはリトライ時間をタイムアウトした。
		if processID != nil || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(edcbSetChannelRetryInterval)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// 起動結果 (プロセス ID) を保持する (Python 版と同じく、リトライ中に状態が変わっても結果はそのまま採用する) 。
	t.edcbProcessID = processID

	// チューナーの起動に失敗した。
	if processID == nil {
		t.state = EDCBTunerIdle
		return false
	}

	// 起動済みとして状態を更新する。
	t.state = EDCBTunerActive
	return true
}

// Connect はチューナーに接続し、放送波を受け取るためのストリームを返す。
//
// 移植元: EDCBTuner.connect()
// 接続できなかった場合は nil を返す (Python 版は None を返す) 。
// 返された io.ReadCloser はそのまま放送波の MPEG2-TS ストリームになる。
func (t *EDCBTuner) Connect(ownerLiveStreamID string) (io.ReadCloser, error) {
	t.mu.Lock()
	// 制御権限が一致していない場合、キャンセル中の場合は処理しない。
	if !t.isOwner(ownerLiveStreamID) || t.state == EDCBTunerCancelling {
		t.mu.Unlock()
		return nil, nil
	}
	// プロセス ID が取得できている (チューナーが起動している) ことが前提。
	processID := t.edcbProcessID
	namedPipe := t.backend.NamedPipe
	creator := t.backend.NewCtrlCmd
	t.mu.Unlock()

	if processID == nil {
		return nil, nil
	}

	// 名前付きパイプモードは Go 版では未対応 (放送波ストリームの取得は TCP モードのみ) 。
	if namedPipe {
		t.setIdleAfterFailedConnect()
		return nil, fmt.Errorf("edcb: named pipe streaming is not supported")
	}

	if creator == nil {
		t.setIdleAfterFailedConnect()
		return nil, fmt.Errorf("edcb: ctrl cmd client is not configured")
	}
	client, err := creator()
	if err != nil {
		t.setIdleAfterFailedConnect()
		return nil, fmt.Errorf("edcb: failed to create the ctrl cmd client: %w", err)
	}

	// EpgDataCap_Bon で受信した放送波を受け取るための TCP 接続を取得する。
	connection, err := client.RelayViewStream(*processID)
	if err != nil {
		t.setIdleAfterFailedConnect()
		return nil, fmt.Errorf("edcb: failed to connect to the tuner: %w", err)
	}

	// 接続を保持して、接続済みとして状態を更新する。
	streamConn := &edcbStreamConn{Conn: connection}
	t.mu.Lock()
	t.streamConn = streamConn
	t.state = EDCBTunerActive
	t.mu.Unlock()
	return streamConn, nil
}

// setIdleAfterFailedConnect は接続失敗時に状態を Idle に戻す (Python 版 connect() の失敗時と同じ) 。
func (t *EDCBTuner) setIdleAfterFailedConnect() {
	t.mu.Lock()
	t.state = EDCBTunerIdle
	t.mu.Unlock()
}

// Disconnect はチューナーとのストリーミング接続を閉じる。
//
// 移植元: EDCBTuner.disconnect()
// ストリーミングが終了した際に必ず呼び出す必要がある。チューナー自体は閉じない (チャンネル切り替え時に再利用するため) 。
func (t *EDCBTuner) Disconnect(ownerLiveStreamID string) {
	t.mu.Lock()
	// 制御権限が一致していない場合は処理しない。
	if !t.isOwner(ownerLiveStreamID) {
		t.mu.Unlock()
		return
	}
	connection := t.streamConn
	t.streamConn = nil
	// 切断済みとして状態を更新する。
	// Cancelling 状態の場合は handoff シーケンスの途中であるため、状態を維持する。
	if t.state != EDCBTunerCancelling {
		t.state = EDCBTunerIdle
	}
	t.mu.Unlock()

	if connection != nil {
		_ = connection.Close()
	}
}

// IsDisconnected はチューナーとのストリーミング接続が閉じられているかどうかを返す。
//
// 移植元: EDCBTuner.isDisconnected()
// 接続していない場合、閉じた後、相手側から切断された (読み取りが EOF になった) 場合は true を返す。
func (t *EDCBTuner) IsDisconnected() bool {
	t.mu.Lock()
	connection := t.streamConn
	t.mu.Unlock()
	if connection == nil {
		return true
	}
	return connection.isDisconnected()
}

// Lock はチューナーをロックする。
//
// 移植元: EDCBTuner.lock()
// ロックしておかないとチューナーの制御を横取りされてしまうので、基本はロック状態にする。
func (t *EDCBTuner) Lock(ownerLiveStreamID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 制御権限が一致していない場合は処理しない。
	if !t.isOwner(ownerLiveStreamID) {
		return false
	}
	t.locked = true
	return true
}

// Unlock はチューナーをアンロックする。
//
// 移植元: EDCBTuner.unlock()
// チューナーがアンロックされている場合、起動中の EpgDataCap_Bon は次のチューナーインスタンスの初期化時に再利用される。
func (t *EDCBTuner) Unlock(ownerLiveStreamID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 制御権限が一致していない場合は処理しない。
	if !t.isOwner(ownerLiveStreamID) {
		return false
	}
	t.locked = false
	return true
}

// Handoff はチューナーの制御権限を別のライブストリームに移譲する。
//
// 移植元: EDCBTuner.handoff()
// ストリーミング接続のクリーンアップは呼び出し元で行う。
func (t *EDCBTuner) Handoff(currentOwnerLiveStreamID string, nextOwnerLiveStreamID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 制御権限が一致していない場合は処理しない。
	if !t.isOwner(currentOwnerLiveStreamID) {
		return false
	}
	// 再利用可能な状態に戻す。
	t.state = EDCBTunerIdle
	t.locked = false
	// 制御権限を移譲する。
	t.ownerLiveStreamID = nextOwnerLiveStreamID
	return true
}

// Close はチューナーを終了する。
//
// 移植元: EDCBTuner.close()
// ownerLiveStreamID が空文字の場合は未割り当てとして扱う。force が true の場合は制御権限の一致を無視する。
// 制御できるかどうかによらず、チューナーのインスタンスはプールから削除される。
func (t *EDCBTuner) Close(ownerLiveStreamID string, force bool) bool {
	t.mu.Lock()
	// 制御権限が一致していない場合は処理しない。
	if !force && !t.isOwner(ownerLiveStreamID) {
		t.mu.Unlock()
		return false
	}
	// プールからの削除と再利用の競合を防ぐため、close 処理中にする。
	t.closing = true
	creator := t.backend.NewCtrlCmd
	networkTVID := t.networkTVID
	connection := t.streamConn
	t.mu.Unlock()

	// チューナーを閉じ、実行結果を取得する。
	result := false
	if creator != nil {
		if client, err := creator(); err == nil {
			result = client.NwTVIDClose(networkTVID)
		}
	}
	if connection != nil {
		_ = connection.Close()
	}

	// インスタンスの登録を削除する (以降 tryTake からは見えなくなる) 。
	removeEDCBTunerInstance(t)

	// チューナーが閉じられたので、状態を初期化する。
	t.mu.Lock()
	t.edcbProcessID = nil
	t.streamConn = nil
	t.state = EDCBTunerIdle
	t.ownerLiveStreamID = ""
	t.locked = false
	t.closing = false
	t.mu.Unlock()

	return result
}

// edcbStreamConn はチューナーとのストリーミング接続。
//
// 相手側からの切断 (読み取りの EOF) や自分からの close を検出できるように net.Conn をラップする。
// Python 版の StreamWriter.is_closing() に相当する判定に使う。
type edcbStreamConn struct {
	net.Conn
	// disconnected は接続が切断されたかどうか。
	disconnected atomic.Bool
}

// Read はストリームから読み取り、エラー (EOF を含む) が発生したら切断済みとして記録する。
func (c *edcbStreamConn) Read(buffer []byte) (int, error) {
	read, err := c.Conn.Read(buffer)
	if err != nil {
		c.disconnected.Store(true)
	}
	return read, err
}

// Close はストリームを閉じ、切断済みとして記録する。
func (c *edcbStreamConn) Close() error {
	c.disconnected.Store(true)
	return c.Conn.Close()
}

// isDisconnected は接続が切断されているかどうかを返す。
func (c *edcbStreamConn) isDisconnected() bool {
	return c.disconnected.Load()
}
