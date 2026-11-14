//go:build !linux && !windows

package service

import "os/exec"

func configureChild(cmd *exec.Cmd) {}
func ownChild(cmd *exec.Cmd) (func(), error) {
	return func() { _ = cmd.Process.Kill() }, nil
}
func signalChild(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
