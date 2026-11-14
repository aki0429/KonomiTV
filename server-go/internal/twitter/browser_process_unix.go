//go:build !windows

package twitter

import (
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

func configureBrowserProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func ownBrowserProcess(cmd *exec.Cmd) (func(), error) {
	// 専用のプロセスグループだけを終了し、既存のユーザーブラウザーを対象にしない。
	return func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, nil
}

func browserPlatformVersion() string {
	cmd := exec.Command("uname", "-r")
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("sw_vers", "-productVersion")
	}
	data, _ := cmd.Output()
	return strings.SplitN(strings.TrimSpace(string(data)), "-", 2)[0]
}
