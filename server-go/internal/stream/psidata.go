package stream

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
)

// LivePSIDataArchiver はライブストリーミング (データ放送) 用の PSI/SI データアーカイバー。
// psisiarc (https://github.com/xtne6f/psisiarc) を起動し、放送波の TS パケットを流し込んで
// アーカイブデータ (.psc) を HTTP レスポンスとして配信する。
// 移植元: server/app/streams/LivePSIDataArchiver.py
type LivePSIDataArchiver struct {
	// serviceID は視聴対象のチャンネルのサービス ID。
	serviceID int
	// psisiarcPath は psisiarc のパス。
	psisiarcPath string
	// logger はログ出力に使うロガー。
	logger *slog.Logger

	// mu は processes を保護する。
	mu sync.Mutex
	// processes は起動中の psisiarc のプロセス。
	processes []*psisiarcProcess
}

// psisiarcProcess は起動中の psisiarc のプロセスを表す。
type psisiarcProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	// closed は既に終了処理を行ったかどうか。
	closed bool
}

// NewLivePSIDataArchiver は PSI/SI データアーカイバーを生成する。
func NewLivePSIDataArchiver(serviceID int, psisiarcPath string, logger *slog.Logger) *LivePSIDataArchiver {
	return &LivePSIDataArchiver{
		serviceID:    serviceID,
		psisiarcPath: psisiarcPath,
		logger:       logger,
	}
}

// PushTSPacketData は生の放送波の MPEG2-TS パケットを受け取り、登録されている psisiarc プロセスに送信する。
// 188 bytes ぴったりで送信する必要はない。
func (a *LivePSIDataArchiver) PushTSPacketData(packet []byte) {
	a.mu.Lock()
	processes := append([]*psisiarcProcess{}, a.processes...)
	a.mu.Unlock()
	for _, process := range processes {
		process.write(packet)
	}
}

// write は psisiarc の標準入力に TS パケットを書き込む。
// 書き込みでブロックしないよう、書き込みは非同期で行う (書き込みバッファが満杯の場合はデータを捨てる) 。
func (p *psisiarcProcess) write(packet []byte) {
	if p.closed {
		return
	}
	_, _ = p.stdin.Write(packet)
}

// Destroy は PSI/SI データアーカイバーを破棄する。
func (a *LivePSIDataArchiver) Destroy() {
	a.mu.Lock()
	processes := a.processes
	a.processes = nil
	a.mu.Unlock()
	for _, process := range processes {
		process.kill()
	}
}

// kill は psisiarc のプロセスを終了する。
func (p *psisiarcProcess) kill() {
	if p.closed {
		return
	}
	p.closed = true
	if p.command.Process != nil {
		_ = p.command.Process.Kill()
	}
	_ = p.stdin.Close()
	_ = p.stdout.Close()
}

// StartPSIArchivedData は psisiarc プロセスを起動し、受信した PSI/SI アーカイブデータを返すチャネルを返す。
// 移植元: LivePSIDataArchiver.getPSIArchivedData()
func (a *LivePSIDataArchiver) StartPSIArchivedData(ctx context.Context) (<-chan []byte, error) {
	// psisiarc のオプション (ref: https://github.com/xtne6f/psisiarc) 。
	// Python 版と同じく '-n ' (末尾に空白) とサービス ID を別々の引数として渡す。
	options := []string{
		"-r", "arib-data", // 番組情報とデータ放送を抽出する
		"-n ", fmt.Sprintf("%d", a.serviceID), // 特定サービスのみを選択して出力するフィルタを有効にする
		"-i", "1", // PCR を基準に 1 秒間隔でアーカイブデータを出力する
		"-", // 標準入力から放送波を入力する
		"-", // 標準出力にアーカイブデータを出力する
	}

	command := exec.CommandContext(ctx, a.psisiarcPath, options...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &psisiarcProcess{command: command, stdin: stdin, stdout: stdout}
	a.mu.Lock()
	a.processes = append(a.processes, process)
	a.mu.Unlock()
	a.logger.Debug(fmt.Sprintf("[LivePSIDataArchiver] psisiarc started. (PID: %d)", command.Process.Pid))

	// 受信した PSI/SI アーカイブデータをチャネルで返す。
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		trailerSize := 0
		for {
			archive, nextTrailerSize, ok := readPSIArchivedDataChunk(ctx, stdout, trailerSize)
			if !ok {
				break
			}
			select {
			case chunks <- archive:
			case <-ctx.Done():
				process.kill()
				a.removeProcess(process)
				a.logger.Debug(fmt.Sprintf("[LivePSIDataArchiver] psisiarc terminated. (Disconnected / PID: %d)", command.Process.Pid))
				return
			}
			trailerSize = nextTrailerSize
		}
		process.kill()
		a.removeProcess(process)
		a.logger.Debug(fmt.Sprintf("[LivePSIDataArchiver] psisiarc terminated. (Destroyed / PID: %d)", command.Process.Pid))
	}()
	return chunks, nil
}

// removeProcess は終了した psisiarc プロセスを一覧から削除する。
func (a *LivePSIDataArchiver) removeProcess(process *psisiarcProcess) {
	a.mu.Lock()
	for i, p := range a.processes {
		if p == process {
			a.processes = append(a.processes[:i], a.processes[i+1:]...)
			break
		}
	}
	a.mu.Unlock()
}

// readPSIArchivedDataChunk は psisiarc からの出力ストリームから PSI/SI アーカイブデータ (.psc) を適切に読み取る。
// EDCB Legacy WebUI の実装 (ini/HttpPublic/legacy/view.lua) をそのまま移植したもの。
// 移植元: LivePSIDataArchiver.__readPSIArchivedDataChunk()
func readPSIArchivedDataChunk(ctx context.Context, stdout io.Reader, trailerSize int) ([]byte, int, bool) {
	if ctx.Err() != nil {
		return nil, 0, false
	}
	if trailerSize > 0 {
		if _, err := io.ReadFull(stdout, make([]byte, trailerSize)); err != nil {
			return nil, 0, false
		}
	}
	header := make([]byte, 32)
	if _, err := io.ReadFull(stdout, header); err != nil {
		return nil, 0, false
	}
	timeListLen := int(binary.LittleEndian.Uint16(header[10:12]))
	dictionaryLen := int(binary.LittleEndian.Uint16(header[12:14]))
	dictionaryDataSize := int(binary.LittleEndian.Uint32(header[16:20]))
	codeListLen := int(binary.LittleEndian.Uint32(header[24:28]))
	payloadSize := timeListLen*4 + dictionaryLen*2 + ((dictionaryDataSize+1)/2)*2 + codeListLen*2
	payload := []byte{}
	if payloadSize > 0 {
		payload = make([]byte, payloadSize)
		if _, err := io.ReadFull(stdout, payload); err != nil {
			return nil, 0, false
		}
	}
	// trailer 分の '=' を先頭に付与したデータを返す (EDCB の実装と同じ) 。
	archive := append(make([]byte, trailerSize), header...)
	archive = append(archive, payload...)
	for i := 0; i < trailerSize; i++ {
		archive[i] = '='
	}
	return archive, 2 + (2+len(payload))%4, true
}
