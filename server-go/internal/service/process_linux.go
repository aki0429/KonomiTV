package service

import (
	"os/exec"
	"syscall"
)

func configureChild(cmd *exec.Cmd) {
	// 親が異常終了した場合にもGo本体へ終了を通知し、その配下の処理を停止する。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

func ownChild(cmd *exec.Cmd) (func(), error) {
	return func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, nil
}

func signalChild(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
