//go:build !linux && !windows

package service

func processExited(int) bool { return false }
