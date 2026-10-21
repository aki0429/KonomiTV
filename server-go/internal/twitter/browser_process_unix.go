//go:build !windows

package twitter

import (
	"os/exec"
	"syscall"
)

func configureBrowserProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func ownBrowserProcess(command *exec.Cmd) (func() error, error) {
	return func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }, nil
}
