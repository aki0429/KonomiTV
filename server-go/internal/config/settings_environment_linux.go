package config

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func runningAsLocalSystem() (bool, error) { return false, nil }
func settingsIgnoredPathError(error) bool { return false }

// settingsListeningPorts は待ち受けソケットの inode を /proc の所有プロセスと照合する。
// 所有者を特定できないものは psutil と同様に pid=None として検査から除外する。
func settingsListeningPorts(ctx context.Context) ([]SettingsListener, error) {
	ports := map[string]int{}
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile("/proc/net/" + name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			address := strings.Split(fields[1], ":")
			ports[fields[9]] = settingsPort(address[len(address)-1], 16)
		}
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := []SettingsListener{}
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(process.Name())
		if err != nil || !process.IsDir() {
			continue
		}
		base := filepath.Join("/proc", process.Name())
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if err != nil {
			continue
		}
		parent, known := 0, false
		if data, err := os.ReadFile(filepath.Join(base, "status")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PPid:") {
					parent, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
					known = err == nil
					break
				}
			}
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if port, ok := ports[inode]; ok {
				result = append(result, SettingsListener{Port: port, PID: pid, ParentPID: parent, ParentKnown: known})
			}
		}
	}
	return result, nil
}
