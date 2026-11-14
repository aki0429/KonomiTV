// Package worklimit は本家 DriveIOLimiter / ProcessLimiter の共有実行枠を管理する。
package worklimit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type mount struct{ point, device, filesystem, options string }
type driveKey struct {
	id          string
	concurrency int
}

var drives = struct {
	sync.Mutex
	semaphores map[driveKey]chan struct{}
	mounts     []mount
	fetched    time.Time
}{semaphores: make(map[driveKey]chan struct{})}
var background = make(chan struct{}, max(1, runtime.NumCPU()/2))

// AcquireDrive はローカル保存先では録画とキャプチャを共通の1枠にする。
// ネットワーク保存先だけは呼び出し元の指定した同時実行数ごとに枠を共有する。
func AcquireDrive(ctx context.Context, path string, networkConcurrency int) (func(), error) {
	drives.Lock()
	id, network := driveID(path, runtime.GOOS, currentMounts())
	concurrency := 1
	if network {
		concurrency = max(1, networkConcurrency)
	}
	key := driveKey{id, concurrency}
	semaphore := drives.semaphores[key]
	if semaphore == nil {
		semaphore = make(chan struct{}, concurrency)
		drives.semaphores[key] = semaphore
	}
	drives.Unlock()
	return acquire(ctx, semaphore)
}

// AcquireBackground は録画の重い解析をCPU論理コア数の半分に制限する。
// 1コア環境でも処理を停止させないよう、最小1枠を確保する。
func AcquireBackground(ctx context.Context, path string) (func(), error) {
	releaseCPU, err := acquire(ctx, background)
	if err != nil {
		return nil, err
	}
	releaseDrive, err := AcquireDrive(ctx, path, 1)
	if err != nil {
		releaseCPU()
		return nil, err
	}
	return func() { releaseDrive(); releaseCPU() }, nil
}

func acquire(ctx context.Context, semaphore chan struct{}) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case semaphore <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-semaphore
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { <-semaphore }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// currentMounts は呼び出し元のロック内で60秒キャッシュを更新する。
func currentMounts() []mount {
	if runtime.GOOS != "linux" {
		return nil
	}
	if drives.fetched.IsZero() || time.Since(drives.fetched) >= 60*time.Second {
		drives.fetched = time.Now()
		drives.mounts = nil
		if file, err := os.Open("/proc/self/mountinfo"); err == nil {
			drives.mounts, _ = readMounts(file)
			_ = file.Close()
		}
	}
	return drives.mounts
}

func readMounts(reader io.Reader) ([]mount, error) {
	var mounts []mount
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid mountinfo record")
		}
		fields, tail := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(fields) < 6 || len(tail) < 3 {
			return nil, fmt.Errorf("invalid mountinfo fields")
		}
		mounts = append(mounts, mount{unescape.Replace(fields[4]), unescape.Replace(tail[1]), tail[0], fields[5] + "," + tail[2]})
	}
	return mounts, scanner.Err()
}

func driveID(path, platform string, mounts []mount) (string, bool) {
	if platform == "windows" {
		if strings.HasPrefix(path, `\\`) {
			parts := strings.Split(path, `\`)
			return strings.ToLower(strings.Join(parts[:min(4, len(parts))], `\`)), true
		}
		if path != "" {
			return strings.ToUpper(path[:1]) + ":", false
		}
		return path, false
	}
	var best *mount
	for i := range mounts {
		entry := &mounts[i]
		if (path == entry.point || strings.HasPrefix(path, strings.TrimRight(entry.point, "/")+"/")) && (best == nil || len(entry.point) > len(best.point)) {
			best = entry
		}
	}
	if best == nil {
		return path, false
	}
	network := false
	switch strings.ToLower(best.filesystem) {
	case "cifs", "smb", "smbfs", "smb3", "nfs", "nfs4", "fuse.sshfs":
		network = true
	}
	for _, option := range strings.Split(best.options, ",") {
		if option == "remote" {
			network = true
		}
	}
	return best.device, network
}

// ResolvePath は存在するリンクを辿り、録画と同じ実体のドライブを選ぶ。
func ResolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
