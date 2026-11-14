package service

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strings"
)

func processExited(pid int) bool {
	if unix.Kill(pid, 0) == unix.ESRCH {
		return true
	}
	// 親が強制終了した子はinitによる回収まで一時的にzombieとして残ることがある。
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return os.IsNotExist(err)
	}
	index := strings.LastIndexByte(string(data), ')')
	if index < 0 {
		return false
	}
	fields := strings.Fields(string(data[index+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}
