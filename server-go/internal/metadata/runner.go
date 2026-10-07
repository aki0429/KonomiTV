package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ProcessResult は外部プロセスの実行結果。
type ProcessResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ProcessRunner は外部プロセス (FFprobe / FFmpeg など) の起動を抽象化する。
// テストでは実プロセスを起動しないフェイク実装に差し替える。
// プロセスを起動できなかった場合のみ error を返し、非 0 終了は ExitCode で表す。
type ProcessRunner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error)
}

// ExecRunner は os/exec による ProcessRunner の実装。
type ExecRunner struct{}

// Run は外部プロセスを実行し、標準出力・標準エラー出力を取得する。
func (ExecRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (*ProcessResult, error) {
	command := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := &ProcessResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			result.ExitCode = exitError.ExitCode()
			return result, nil
		}
		return nil, fmt.Errorf("failed to run %s: %w", name, err)
	}
	return result, nil
}
