package reservations

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
)

// このファイルは NetworkTV モード (EDCB のライブ配信) 用に追加した CtrlCmd 操作の単体テスト。
// 実機の EDCB には接続せず、Transport / net.Listener をフェイクに差し替えて wire 形式と挙動を検証する。

// recordingTransport はリクエストを記録し、あらかじめ設定したレスポンスを返すテスト用 Transport。
type recordingTransport struct {
	requests [][]byte
	response Response
	err      error
}

func (t *recordingTransport) SendAndReceive(request []byte) (Response, error) {
	t.requests = append(t.requests, append([]byte(nil), request...))
	if t.err != nil {
		return Response{}, t.err
	}
	return t.response, nil
}

// littleEndianInt はバッファの指定位置から 32bit リトルエンディアンの符号付き整数を読み取る。
func littleEndianInt(buffer []byte, offset int) int {
	return int(int32(binary.LittleEndian.Uint32(buffer[offset:])))
}

// littleEndianUshort はバッファの指定位置から 16bit リトルエンディアンの符号なし整数を読み取る。
func littleEndianUshort(buffer []byte, offset int) int {
	return int(binary.LittleEndian.Uint16(buffer[offset:]))
}

// TestNwTVIDSetChWireFormat は NwTVIDSetCh が Python 版 __writeSetChInfo と同じ wire 形式で送信し、
// レスポンスのプロセス ID を返すことを検証する。
func TestNwTVIDSetChWireFormat(t *testing.T) {
	transport := &recordingTransport{}
	client := NewClient(transport)

	info := SetChInfo{
		Onid:      0x7881,
		Sid:       0x0400,
		Tsid:      0x7A76,
		SpaceOrID: 500,
		ChOrMode:  2,
		UseSid:    true,
		UseBonCh:  true,
	}

	// レスポンスのデータ部は EpgDataCap_Bon のプロセス ID (int) 。
	responseData := make([]byte, 4)
	binary.LittleEndian.PutUint32(responseData, uint32(4242))
	transport.response = Response{Code: cmdSuccess, Data: responseData}

	processID, ok := client.NwTVIDSetCh(info)
	if !ok {
		t.Fatalf("NwTVIDSetCh() ok = false, want true")
	}
	if processID != 4242 {
		t.Errorf("process id = %d, want 4242", processID)
	}

	if len(transport.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(transport.requests))
	}
	request := transport.requests[0]

	// コマンド本体: コマンド(4) + サイズ(4) + 構造体(4 + 22) = 34 バイト
	if len(request) != 34 {
		t.Fatalf("request length = %d, want 34", len(request))
	}
	if cmd := littleEndianInt(request, 0); cmd != 1073 {
		t.Errorf("command = %d, want 1073 (CMD_EPG_SRV_NWTV_ID_SET_CH)", cmd)
	}
	if size := littleEndianInt(request, 4); size != 26 {
		t.Errorf("header size = %d, want 26", size)
	}
	if size := littleEndianInt(request, 8); size != 26 {
		t.Errorf("struct size = %d, want 26", size)
	}
	if useSid := littleEndianInt(request, 12); useSid != 1 {
		t.Errorf("use_sid = %d, want 1", useSid)
	}
	if onid := littleEndianUshort(request, 16); onid != 0x7881 {
		t.Errorf("onid = %X, want 7881", onid)
	}
	if tsid := littleEndianUshort(request, 18); tsid != 0x7A76 {
		t.Errorf("tsid = %X, want 7A76", tsid)
	}
	if sid := littleEndianUshort(request, 20); sid != 0x0400 {
		t.Errorf("sid = %X, want 400", sid)
	}
	if useBonCh := littleEndianInt(request, 22); useBonCh != 1 {
		t.Errorf("use_bon_ch = %d, want 1", useBonCh)
	}
	if spaceOrID := littleEndianInt(request, 26); spaceOrID != 500 {
		t.Errorf("space_or_id = %d, want 500", spaceOrID)
	}
	if chOrMode := littleEndianInt(request, 30); chOrMode != 2 {
		t.Errorf("ch_or_mode = %d, want 2 (TCP)", chOrMode)
	}
}

// TestNwTVIDSetChFailure は NwTVIDSetCh がコマンド失敗時に ok=false を返すことを検証する。
func TestNwTVIDSetChFailure(t *testing.T) {
	transport := &recordingTransport{response: Response{Code: 0, Data: nil}}
	client := NewClient(transport)
	if _, ok := client.NwTVIDSetCh(SetChInfo{Onid: 1, Sid: 2, Tsid: 3}); ok {
		t.Errorf("NwTVIDSetCh() ok = true, want false when the command fails")
	}
}

// TestNwTVIDCloseWireFormat は NwTVIDClose が NetworkTV ID を送信することを検証する。
func TestNwTVIDCloseWireFormat(t *testing.T) {
	transport := &recordingTransport{response: Response{Code: cmdSuccess}}
	client := NewClient(transport)

	if !client.NwTVIDClose(504) {
		t.Fatalf("NwTVIDClose() = false, want true")
	}
	request := transport.requests[0]
	// コマンド(4) + サイズ(4) + NetworkTV ID(4) = 12 バイト
	if len(request) != 12 {
		t.Fatalf("request length = %d, want 12", len(request))
	}
	if cmd := littleEndianInt(request, 0); cmd != 1074 {
		t.Errorf("command = %d, want 1074 (CMD_EPG_SRV_NWTV_ID_CLOSE)", cmd)
	}
	if size := littleEndianInt(request, 4); size != 4 {
		t.Errorf("header size = %d, want 4", size)
	}
	if nwtvID := littleEndianInt(request, 8); nwtvID != 504 {
		t.Errorf("nwtv_id = %d, want 504", nwtvID)
	}
}

// TestRelayViewStreamTCP は RelayViewStream が TCP モードでストリーム転送を要求し、
// 成功時は返された接続からそのまま TS を読み取れることを検証する。
func TestRelayViewStreamTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	// テスト用の偽 EDCB サーバー: リクエストを検証し、成功ヘッダーの後に TS を流す。
	serverError := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverError <- err
			return
		}
		defer connection.Close()

		request := make([]byte, 12)
		if _, err := io.ReadFull(connection, request); err != nil {
			serverError <- fmt.Errorf("failed to read the request: %w", err)
			return
		}
		if cmd := littleEndianInt(request, 0); cmd != 301 {
			serverError <- fmt.Errorf("command = %d, want 301", cmd)
			return
		}
		if size := littleEndianInt(request, 4); size != 4 {
			serverError <- fmt.Errorf("size = %d, want 4", size)
			return
		}
		if processID := littleEndianInt(request, 8); processID != 4242 {
			serverError <- fmt.Errorf("process id = %d, want 4242", processID)
			return
		}

		// 成功ヘッダー (戻り値 = 1, サイズ = 任意) を送る
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header[0:4], uint32(cmdSuccess))
		if _, err := connection.Write(header); err != nil {
			serverError <- err
			return
		}
		// そのまま TS を流す (188 バイトのパケット)
		packet := make([]byte, 188)
		for index := range packet {
			packet[index] = 0x47
		}
		if _, err := connection.Write(packet); err != nil {
			serverError <- err
			return
		}
		serverError <- nil
	}()

	client := NewClient(&TCPTransport{Address: listener.Addr().String(), Timeout: defaultConnectTimeout})
	connection, err := client.RelayViewStream(4242)
	if err != nil {
		t.Fatalf("RelayViewStream() = %v", err)
	}
	defer connection.Close()

	packet := make([]byte, 188)
	if _, err := io.ReadFull(connection, packet); err != nil {
		t.Fatalf("failed to read the TS packet: %v", err)
	}
	if packet[0] != 0x47 {
		t.Errorf("TS sync byte = %X, want 47", packet[0])
	}
	if err := <-serverError; err != nil {
		t.Errorf("server error: %v", err)
	}
}

// TestRelayViewStreamFailure は RelayViewStream がコマンド失敗時に接続を閉じてエラーを返すことを検証する。
func TestRelayViewStreamFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		request := make([]byte, 12)
		if _, err := io.ReadFull(connection, request); err != nil {
			return
		}
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header[0:4], uint32(0)) // 失敗 (戻り値 != 1)
		_, _ = connection.Write(header)
	}()

	client := NewClient(&TCPTransport{Address: listener.Addr().String(), Timeout: defaultConnectTimeout})
	if _, err := client.RelayViewStream(4242); err == nil {
		t.Errorf("RelayViewStream() error = nil, want an error when the command fails")
	}
}

// TestRelayViewStreamNamedPipeUnsupported は名前付きパイプモードでは RelayViewStream が未対応のエラーを返すことを検証する。
func TestRelayViewStreamNamedPipeUnsupported(t *testing.T) {
	client := NewClient(&pipeTransport{})
	if _, err := client.RelayViewStream(4242); err == nil {
		t.Errorf("RelayViewStream() error = nil, want an error in named pipe mode")
	}
}

// TestEDCBHost は edcb_url からホスト名を取り出せることを検証する。
func TestEDCBHost(t *testing.T) {
	host, err := EDCBHost("tcp://127.0.0.1:4510/")
	if err != nil || host != "127.0.0.1" {
		t.Errorf("EDCBHost() = (%q, %v), want (127.0.0.1, nil)", host, err)
	}
	host, err = EDCBHost("tcp://edcb-namedpipe/")
	if err != nil || host != "edcb-namedpipe" {
		t.Errorf("EDCBHost() = (%q, %v), want (edcb-namedpipe, nil)", host, err)
	}
	if _, err := EDCBHost(""); err == nil {
		t.Errorf("EDCBHost(\"\") error = nil, want an error")
	}
}
