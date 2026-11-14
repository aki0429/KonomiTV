package config

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func settingsIgnoredPathError(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_NAME) || errors.Is(err, windows.ERROR_CANT_RESOLVE_FILENAME) || errors.Is(err, windows.ERROR_DIRECTORY) || errors.Is(err, windows.ERROR_NOT_READY)
}

func runningAsLocalSystem() (bool, error) {
	sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false, err
	}
	return windows.Token(0).IsMember(sid)
}

// settingsListeningPorts は TCP の待ち受け PID と、その親 PID を取得する。
// 名前解決を行わず、IPv4/IPv6 の両方を対象にする。
func settingsListeningPorts(ctx context.Context) ([]SettingsListener, error) {
	output, err := exec.CommandContext(ctx, "netstat.exe", "-ano").Output()
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err == nil {
		defer windows.CloseHandle(snapshot)
		entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
		for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
			parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		}
	}
	result := []SettingsListener{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != "TCP" || fields[3] != "LISTENING" {
			continue
		}
		separator := strings.LastIndex(fields[1], ":")
		if separator < 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}
		parent, known := parents[pid]
		result = append(result, SettingsListener{Port: settingsPort(fields[1][separator+1:], 10), PID: pid, ParentPID: parent, ParentKnown: known})
	}
	return result, nil
}
