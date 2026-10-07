package videostream

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// ProcessKind は録画エンコードパイプラインで起動する外部プロセスの種類。
type ProcessKind string

const (
	// ProcessPsisimux は MP4 録画から MPEG-TS を合成する psisimux 。
	ProcessPsisimux ProcessKind = "psisimux"
	// ProcessTSReadEx は放送波の前処理を行う tsreadex 。
	ProcessTSReadEx ProcessKind = "tsreadex"
	// ProcessEncoder は FFmpeg / QSVEncC / NVEncC / VCEEncC / rkmppenc のいずれか。
	ProcessEncoder ProcessKind = "encoder"
)

// ProcessSpec は起動する外部プロセスの指定。
type ProcessSpec struct {
	// Kind はプロセスの種類 (テストのフェイクが振り分けに使う) 。
	Kind ProcessKind
	// Name はログ用の名前 (psisimux / tsreadex / FFmpeg / NVEncC など) 。
	Name string
	// Path は実行ファイルのパス。
	Path string
	// Args は引数列。
	Args []string
	// Stdin は標準入力に繋ぐ入力 (上流プロセスの Stdout() など) 。nil なら標準入力は空。
	// ProcessRunner は *os.File の場合、子プロセスの起動後に親側の複製を Close する (Python の os.close(read_pipe) 相当) 。
	Stdin io.Reader
	// CloseStdinAfterStart が true かつ Stdin が *os.File の場合、子プロセスの起動後に親側で Stdin を Close する。
	// 上流プロセスの Stdout() を渡す場合に true にし、録画ファイルを渡す場合は false にして複数回の試行で使い回す。
	CloseStdinAfterStart bool
	// StdinPipe が true の場合、Process.Stdin() から標準入力へ書き込める。
	StdinPipe bool
	// CaptureStdout が true の場合、Process.Stdout() から標準出力を読める。false なら捨てる。
	CaptureStdout bool
	// CaptureStderr が true の場合、Process.Stderr() から標準エラー出力を読める。false なら捨てる。
	CaptureStderr bool
}

// Process は起動済みの外部プロセス。
type Process interface {
	// Stdin は StdinPipe 指定時の書き込み側 (プロセス終了または Close で子プロセスに EOF が届く) 。それ以外は nil 。
	Stdin() io.WriteCloser
	// Stdout は CaptureStdout 指定時の読み取り側。それ以外は nil 。
	Stdout() io.ReadCloser
	// Stderr は CaptureStderr 指定時の読み取り側。それ以外は nil 。
	Stderr() io.ReadCloser
	// Wait はプロセスの終了を待つ (1 回だけ呼ぶこと) 。
	Wait() error
	// Kill はプロセスを強制終了する。
	Kill() error
}

// ProcessRunner は外部プロセスを起動する関数。テストではフェイクに差し替える (os/exec を直接使わない) 。
type ProcessRunner func(spec ProcessSpec) (Process, error)

// execProcess は os/exec による Process 実装。
// 標準入出力は os.Pipe で自前に作り、Cmd.Wait() が読み取り側を閉じてしまわないようにしている。
type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	waitOnce sync.Once
	waitErr  error
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *execProcess) Wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// ExecProcessRunner は os/exec で実際にプロセスを起動する既定の ProcessRunner 。
func ExecProcessRunner(spec ProcessSpec) (Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	process := &execProcess{cmd: cmd}

	// 子プロセスへ渡して親側で閉じるファイル (起動後に必ず閉じる) と、失敗時にすべて閉じるファイル
	var childSide []*os.File
	var parentSide []*os.File
	closeAll := func(files []*os.File) {
		for _, file := range files {
			_ = file.Close()
		}
	}

	switch {
	case spec.StdinPipe:
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdin = reader
		childSide = append(childSide, reader)
		parentSide = append(parentSide, writer)
		process.stdin = writer
	case spec.Stdin != nil:
		cmd.Stdin = spec.Stdin
		if file, ok := spec.Stdin.(*os.File); ok && spec.CloseStdinAfterStart {
			// 上流プロセスの Stdout() やパイプの読み取り側は、起動後に親側で不要になる
			childSide = append(childSide, file)
		}
	}
	if spec.CaptureStdout {
		reader, writer, err := os.Pipe()
		if err != nil {
			closeAll(childSide)
			closeAll(parentSide)
			return nil, err
		}
		cmd.Stdout = writer
		childSide = append(childSide, writer)
		parentSide = append(parentSide, reader)
		process.stdout = reader
	}
	if spec.CaptureStderr {
		reader, writer, err := os.Pipe()
		if err != nil {
			closeAll(childSide)
			closeAll(parentSide)
			return nil, err
		}
		cmd.Stderr = writer
		childSide = append(childSide, writer)
		parentSide = append(parentSide, reader)
		process.stderr = reader
	}

	startErr := cmd.Start()
	// 子プロセスに渡した側は、起動の成否にかかわらず親側で閉じる (閉じ忘れると FD がリークし EOF も届かない)
	closeAll(childSide)
	if startErr != nil {
		closeAll(parentSide)
		return nil, fmt.Errorf("failed to start %s: %w", spec.Name, startErr)
	}
	return process, nil
}
