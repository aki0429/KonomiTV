package reservations

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// このファイルは server/app/utils/edcb/CtrlCmdUtil.py の CtrlCmdUtil を移植したもの。
//
// 実機の EDCB (EpgTimerSrv) へ接続するネットワーク層は Transport インターフェースとして
// 切り出してあり、テストでは差し替えられる。実機への接続を伴うテストは行わない。

// EDCB の CtrlCmd プロトコル定数 (Python 版 CtrlCmdUtil の __CMD_* と同じ値) 。
const (
	cmdSuccess = 1
	cmdVersion = 5

	cmdEnumReserve2 = 2011
	cmdAddReserve2  = 2013
	cmdChgReserve2  = 2015
	cmdDelReserve   = 1014
	cmdEnumPgInfoEx = 1029
	cmdFileCopy     = 1060
	cmdFileCopy2    = 2060
	cmdNWPlayTFOpen = 1087
	cmdNWPlayClose  = 1081
	cmdEnumAutoAdd2 = 2131
	cmdAddAutoAdd2  = 2132
	cmdChgAutoAdd2  = 2134
	cmdDelAutoAdd   = 1033
)

// defaultConnectTimeout は Python 版 CtrlCmdUtil の既定タイムアウト (15 秒) 。
const defaultConnectTimeout = 15 * time.Second

// Response は EDCB からの 1 コマンド分のレスポンス。
type Response struct {
	// Code はコマンドの戻り値 (cmdSuccess なら成功) 。
	Code int
	// Data はレスポンス本体。
	Data []byte
}

// Transport は EDCB との 1 コマンド分の送受信を行う。
//
// 実機接続 (TCP / 名前付きパイプ / UNIX ドメインソケット) とテスト用のフェイクを差し替えられるように
// インターフェースにしている。
type Transport interface {
	SendAndReceive(request []byte) (Response, error)
}

// TCPTransport は TCP/IP で EDCB の CtrlCmd インターフェースへ接続する Transport。
type TCPTransport struct {
	Address string
	Timeout time.Duration
}

// SendAndReceive はリクエストを送信し、8 バイトのヘッダー (戻り値 + サイズ) と本体を読み取る。
func (t *TCPTransport) SendAndReceive(request []byte) (Response, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	connection, err := net.DialTimeout("tcp", t.Address, timeout)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, err
	}
	if _, err := connection.Write(request); err != nil {
		return Response{}, err
	}
	header := make([]byte, 8)
	if _, err := readFull(connection, header); err != nil {
		return Response{}, err
	}
	code := int(int32(binary.LittleEndian.Uint32(header[0:4])))
	size := int(int32(binary.LittleEndian.Uint32(header[4:8])))
	if size < 0 {
		return Response{}, fmt.Errorf("edcb: invalid response size %d", size)
	}
	payload := make([]byte, size)
	if size > 0 {
		if _, err := readFull(connection, payload); err != nil {
			return Response{}, err
		}
	}
	return Response{Code: code, Data: payload}, nil
}

// readFull は conn から len(buffer) バイトを読み切る。
func readFull(connection net.Conn, buffer []byte) (int, error) {
	total := 0
	for total < len(buffer) {
		read, err := connection.Read(buffer[total:])
		total += read
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// pipeTransport は Windows の名前付きパイプ / 非 Windows の UNIX ドメインソケットで接続する Transport。
//
// Python 版の名前付きパイプモード (EDCB の edcb_url に edcb-namedpipe を指定した場合) に対応する。
// 実機の EDCB がない環境では検証できていないため、動作確認は未実施。
type pipeTransport struct {
	Path    string
	Name    string
	Timeout time.Duration
}

// namedPipePath は Python 版 CtrlCmdUtil の既定パスと同じ値を返す。
func namedPipePath() string {
	if isWindows() {
		return `\\.\pipe\`
	}
	return "/var/local/edcb/"
}

// namedPipeName は Windows ならそのまま、それ以外は 'NoWait' を除いた名前を返す (Python 版と同じ) 。
func namedPipeName(name string) string {
	if isWindows() {
		return name
	}
	return strings.ReplaceAll(name, "NoWait", "")
}

// SendAndReceive は名前付きパイプ / UNIX ドメインソケット経由で送受信する。
func (t *pipeTransport) SendAndReceive(request []byte) (Response, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	path := t.Path + t.Name
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = file.Close() }()
	if err := file.SetDeadline(time.Now().Add(timeout)); err != nil {
		// 名前付きパイプ経由の SetDeadline は環境によっては未対応のため、失敗しても続行する
		_ = err
	}
	if _, err := file.Write(request); err != nil {
		return Response{}, err
	}
	header := make([]byte, 8)
	if _, err := file.Read(header); err != nil {
		return Response{}, err
	}
	code := int(int32(binary.LittleEndian.Uint32(header[0:4])))
	size := int(int32(binary.LittleEndian.Uint32(header[4:8])))
	if size < 0 {
		return Response{}, fmt.Errorf("edcb: invalid response size %d", size)
	}
	payload := make([]byte, size)
	if size > 0 {
		if _, err := file.Read(payload); err != nil {
			return Response{}, err
		}
	}
	return Response{Code: code, Data: payload}, nil
}

// Client は EDCB の CtrlCmd インターフェースのクライアント。
type Client struct {
	transport Transport
}

// EDCBClient は録画予約系 API が必要とする EDCB の操作。
//
// 各メソッドの戻り値の bool は Python 版の None / False と同じく「失敗」を表す。
type EDCBClient interface {
	EnumReserve() ([]ReserveData, bool)
	AddReserve(reserveList []ReserveData) bool
	ChgReserve(reserveList []ReserveData) bool
	DelReserve(reserveIDList []int) bool
	EnumPgInfoEx(serviceTimeList []int64) ([]ServiceEventInfo, bool)
	FileCopy(name string) ([]byte, bool)
	FileCopy2(nameList []string) ([]FileData, bool)
	GetRecFilePath(reserveID int) *string
	EnumAutoAdd() ([]AutoAddData, bool)
	AddAutoAdd(dataList []AutoAddData) bool
	ChgAutoAdd(dataList []AutoAddData) bool
	DelAutoAdd(idList []int) bool
}

// NewClient は Transport を指定して Client を生成する (主にテスト用) 。
func NewClient(transport Transport) *Client {
	return &Client{transport: transport}
}

// NewClientFromURL は edcb_url (例: "tcp://127.0.0.1:4510/") から Client を生成する。
//
// ホスト名が "edcb-namedpipe" の場合は名前付きパイプ / UNIX ドメインソケットモードになる
// (Python 版 CtrlCmdUtil.__init__ と同じ判定) 。
func NewClientFromURL(edcbURL string) (*Client, error) {
	parsed, err := url.Parse(edcbURL)
	if err != nil {
		return nil, fmt.Errorf("edcb: failed to parse edcb_url %q: %w", edcbURL, err)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("edcb: edcb_url %q has no host", edcbURL)
	}
	if host == "edcb-namedpipe" {
		return &Client{transport: &pipeTransport{
			Path:    namedPipePath(),
			Name:    namedPipeName("EpgTimerSrvNoWaitPipe"),
			Timeout: defaultConnectTimeout,
		}}, nil
	}
	port := parsed.Port()
	if port == "" {
		return nil, fmt.Errorf("edcb: edcb_url %q has no port", edcbURL)
	}
	return &Client{transport: &TCPTransport{
		Address: net.JoinHostPort(host, port),
		Timeout: defaultConnectTimeout,
	}}, nil
}

// sendAndReceive はリクエストを送信する。送受信に失敗した場合は ok=false を返す。
func (c *Client) sendAndReceive(request []byte) (Response, bool) {
	if c.transport == nil {
		return Response{}, false
	}
	response, err := c.transport.SendAndReceive(request)
	if err != nil {
		return Response{}, false
	}
	return response, true
}

// sendCmd は CtrlCmd の通常コマンドを送信する (write はヘッダーの後ろに追記する) 。
func (c *Client) sendCmd(command int, write func(*wireWriter)) (Response, bool) {
	writer := &wireWriter{buf: make([]byte, 0, 64)}
	writer.writeInt(command)
	writer.writeInt(0)
	if write != nil {
		write(writer)
	}
	writer.writeIntInplace(4, len(writer.buf)-8)
	return c.sendAndReceive(writer.buf)
}

// sendCmd2 はバージョン番号付きのコマンドを送信する (write はヘッダーの後ろに追記する) 。
func (c *Client) sendCmd2(command int, write func(*wireWriter)) (Response, bool) {
	writer := &wireWriter{buf: make([]byte, 0, 64)}
	writer.writeInt(command)
	writer.writeInt(0)
	writer.writeUshort(cmdVersion)
	if write != nil {
		write(writer)
	}
	writer.writeIntInplace(4, len(writer.buf)-8)
	return c.sendAndReceive(writer.buf)
}

// decodeResponse は panic を使う readError を捕捉しつつデコード処理を実行する。
//
// Python 版が __ReadError を捕捉して None を返す挙動に対応する。
func decodeResponse[T any](decode func() T) (value T, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if _, isReadError := recovered.(readError); isReadError {
				var zero T
				value = zero
				ok = false
				return
			}
			panic(recovered)
		}
	}()
	return decode(), true
}

// EnumReserve は予約一覧を取得する (CMD_EPG_SRV_ENUM_RESERVE2) 。
func (c *Client) EnumReserve() ([]ReserveData, bool) {
	response, ok := c.sendCmd2(cmdEnumReserve2, nil)
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return decodeResponse(func() []ReserveData {
		reader := &wireReader{buf: response.Data}
		size := len(response.Data)
		if reader.readUshort(size) < cmdVersion {
			panic(readError{})
		}
		return readVector(reader, size, readReserveData)
	})
}

// AddReserve は予約を追加する (CMD_EPG_SRV_ADD_RESERVE2) 。
func (c *Client) AddReserve(reserveList []ReserveData) bool {
	response, ok := c.sendCmd2(cmdAddReserve2, func(writer *wireWriter) {
		writer.writeVector(len(reserveList), func() {
			for _, reserve := range reserveList {
				writeReserveData(writer, reserve)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}

// ChgReserve は予約を変更する (CMD_EPG_SRV_CHG_RESERVE2) 。
func (c *Client) ChgReserve(reserveList []ReserveData) bool {
	response, ok := c.sendCmd2(cmdChgReserve2, func(writer *wireWriter) {
		writer.writeVector(len(reserveList), func() {
			for _, reserve := range reserveList {
				writeReserveData(writer, reserve)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}

// DelReserve は予約を削除する (CMD_EPG_SRV_DEL_RESERVE) 。
func (c *Client) DelReserve(reserveIDList []int) bool {
	response, ok := c.sendCmd(cmdDelReserve, func(writer *wireWriter) {
		writer.writeVector(len(reserveIDList), func() {
			for _, reserveID := range reserveIDList {
				writer.writeInt(reserveID)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}

// EnumPgInfoEx はサービス指定と時間指定で番組情報一覧を取得する (CMD_EPG_SRV_ENUM_PG_INFO_EX) 。
func (c *Client) EnumPgInfoEx(serviceTimeList []int64) ([]ServiceEventInfo, bool) {
	response, ok := c.sendCmd(cmdEnumPgInfoEx, func(writer *wireWriter) {
		writer.writeVector(len(serviceTimeList), func() {
			for _, value := range serviceTimeList {
				writer.writeLong(value)
			}
		})
	})
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return decodeResponse(func() []ServiceEventInfo {
		reader := &wireReader{buf: response.Data}
		return readVector(reader, len(response.Data), readServiceEventInfo)
	})
}

// FileCopy は指定ファイルを転送する (CMD_EPG_SRV_FILE_COPY) 。
func (c *Client) FileCopy(name string) ([]byte, bool) {
	response, ok := c.sendCmd(cmdFileCopy, func(writer *wireWriter) {
		writer.writeString(name)
	})
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return response.Data, true
}

// FileCopy2 は指定ファイルをまとめて転送する (CMD_EPG_SRV_FILE_COPY2) 。
func (c *Client) FileCopy2(nameList []string) ([]FileData, bool) {
	response, ok := c.sendCmd2(cmdFileCopy2, func(writer *wireWriter) {
		writer.writeVector(len(nameList), func() {
			for _, name := range nameList {
				writer.writeString(name)
			}
		})
	})
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return decodeResponse(func() []FileData {
		reader := &wireReader{buf: response.Data}
		size := len(response.Data)
		if reader.readUshort(size) < cmdVersion {
			panic(readError{})
		}
		return readVector(reader, size, readFileData)
	})
}

// GetRecFilePath は録画中かつ視聴予約でない予約の録画ファイルパスを取得する
// (CMD_EPG_SRV_NWPLAY_TF_OPEN) 。取得できなかった場合は nil を返す。
func (c *Client) GetRecFilePath(reserveID int) *string {
	response, ok := c.sendCmd(cmdNWPlayTFOpen, func(writer *wireWriter) {
		writer.writeInt(reserveID)
	})
	if !ok || response.Code != cmdSuccess {
		return nil
	}
	info, decoded := decodeResponse(func() NWPlayTimeShiftInfo {
		reader := &wireReader{buf: response.Data}
		return readNWPlayTimeShiftInfo(reader, len(response.Data))
	})
	if !decoded {
		return nil
	}
	// Python 版と同じく、読み取った ctrl_id でクローズを送信してからファイルパスを返す (結果は見ない)
	_, _ = c.sendCmd(cmdNWPlayClose, func(writer *wireWriter) {
		writer.writeInt(info.CtrlID)
	})
	return &info.FilePath
}

// EnumAutoAdd は自動予約登録情報一覧を取得する (CMD_EPG_SRV_ENUM_AUTO_ADD2) 。
func (c *Client) EnumAutoAdd() ([]AutoAddData, bool) {
	response, ok := c.sendCmd2(cmdEnumAutoAdd2, nil)
	if !ok || response.Code != cmdSuccess {
		return nil, false
	}
	return decodeResponse(func() []AutoAddData {
		reader := &wireReader{buf: response.Data}
		size := len(response.Data)
		if reader.readUshort(size) < cmdVersion {
			panic(readError{})
		}
		return readVector(reader, size, readAutoAddData)
	})
}

// AddAutoAdd は自動予約登録情報を追加する (CMD_EPG_SRV_ADD_AUTO_ADD2) 。
func (c *Client) AddAutoAdd(dataList []AutoAddData) bool {
	response, ok := c.sendCmd2(cmdAddAutoAdd2, func(writer *wireWriter) {
		writer.writeVector(len(dataList), func() {
			for _, data := range dataList {
				writeAutoAddData(writer, data)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}

// ChgAutoAdd は自動予約登録情報を変更する (CMD_EPG_SRV_CHG_AUTO_ADD2) 。
func (c *Client) ChgAutoAdd(dataList []AutoAddData) bool {
	response, ok := c.sendCmd2(cmdChgAutoAdd2, func(writer *wireWriter) {
		writer.writeVector(len(dataList), func() {
			for _, data := range dataList {
				writeAutoAddData(writer, data)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}

// DelAutoAdd は自動予約登録情報を削除する (CMD_EPG_SRV_DEL_AUTO_ADD) 。
func (c *Client) DelAutoAdd(idList []int) bool {
	response, ok := c.sendCmd(cmdDelAutoAdd, func(writer *wireWriter) {
		writer.writeVector(len(idList), func() {
			for _, id := range idList {
				writer.writeInt(id)
			}
		})
	})
	return ok && response.Code == cmdSuccess
}
