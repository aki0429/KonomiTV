package api

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/fsnotify/fsnotify"
)

// captureNetworkRoot は Linux のマウント情報から、通知の届かない保存先を見分ける。
// 保存先の下にネットワークのマウントがある場合も一括スキャンの対象にする。
func captureNetworkRoot(root string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	defer func() { _ = file.Close() }()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	best := -1
	network := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[0])
		tail := strings.Fields(parts[1])
		if len(fields) < 6 || len(tail) < 3 {
			continue
		}
		mount := unescape.Replace(fields[4])
		fstype := strings.ToLower(tail[0])
		remote := false
		for _, kind := range []string{"cifs", "smb", "smbfs", "smb3", "nfs", "nfs4", "fuse.sshfs"} {
			if fstype == kind {
				remote = true
				break
			}
		}
		if remote && captureWithin(mount, root) {
			return true
		}
		if captureWithin(root, mount) && len(mount) > best {
			best = len(mount)
			network = remote
		}
	}
	return network || scanner.Err() != nil
}

type captureWatchJob struct {
	paths []string
	scan  bool
}

// RunCaptureFolderMonitoring は起動時スキャンと OS 通知を並行させ、終了時は全処理の完了を待つ。
func (s *Server) RunCaptureFolderMonitoring(ctx context.Context) {
	c := s.captureCatalog
	if len(c.folders) == 0 {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.logger.Warn("Capture watcher unavailable; using folder polling.", "error", err)
	}
	if watcher != nil {
		defer func() { _ = watcher.Close() }()
	}
	localRoots := []string{}
	hasNetwork := false
	seen := map[string]bool{}
	for _, root := range c.folders {
		root = normalizeCapturePath(root)
		key := capturePathKey(root)
		if seen[key] {
			continue
		}
		seen[key] = true
		if captureNetworkRoot(root) {
			hasNetwork = true
		} else {
			localRoots = append(localRoots, root)
		}
	}
	// 通知を読む処理と画像のデコードを分け、重い画像解析中も通知を溜め込ませない。
	jobs := make(chan captureWatchJob, 1)
	rescan := make(chan struct{}, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	requestScan := func() {
		select {
		case rescan <- struct{}{}:
		default:
		}
	}
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-rescan:
				if _, err := c.runBatch(ctx); err != nil && ctx.Err() == nil {
					if errors.Is(err, errCaptureScanRunning) {
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Second):
							requestScan()
						}
					} else {
						s.logger.Warn("Capture batch scan failed.", "error", err)
					}
				}
			case job := <-jobs:
				for _, path := range job.paths {
					if ctx.Err() != nil {
						return
					}
					if _, err := os.Stat(path); os.IsNotExist(err) {
						if err = c.markMissing(ctx, path); err != nil && ctx.Err() == nil {
							s.logger.Warn("Failed to mark missing capture.", "path", path, "error", err)
						}
					} else if err == nil {
						if _, err = c.register(ctx, path, nil, false); err != nil && ctx.Err() == nil {
							s.logger.Warn("Failed to register capture change.", "path", path, "error", err)
						}
					}
				}
				if job.scan {
					requestScan()
				}
			}
		}
	}()
	defer workers.Wait()
	watched := map[string]bool{}
	rootsReady := map[string]bool{}
	addTree := func(root string) bool {
		if watcher == nil {
			return false
		}
		complete := true
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				complete = false
				return nil
			}
			if !entry.IsDir() {
				return nil
			}
			if strings.HasPrefix(entry.Name(), "._") {
				return filepath.SkipDir
			}
			key := capturePathKey(path)
			if !watched[key] {
				if err := watcher.Add(path); err != nil {
					complete = false
					s.logger.Warn("Failed to watch capture directory.", "path", path, "error", err)
				} else {
					watched[key] = true
				}
			}
			return nil
		})
		return complete && err == nil
	}
	for _, root := range localRoots {
		rootsReady[capturePathKey(root)] = addTree(root)
	}
	requestScan()
	flush := time.NewTicker(500 * time.Millisecond)
	defer flush.Stop()
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	var poll <-chan time.Time
	var pollTimer *time.Ticker
	interval := s.config.Capture.UploadFoldersPollingInterval
	if watcher == nil && interval <= 0 {
		interval = 1
	}
	if (hasNetwork || watcher == nil) && interval > 0 {
		duration := time.Duration(interval * float64(time.Minute))
		if duration < time.Millisecond {
			duration = time.Millisecond
		}
		pollTimer = time.NewTicker(duration)
		poll = pollTimer.C
		defer pollTimer.Stop()
	}
	pending := map[string]time.Time{}
	scanPending := false
	var events <-chan fsnotify.Event
	var watcherErrors <-chan error
	if watcher != nil {
		events = watcher.Events
		watcherErrors = watcher.Errors
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			path := normalizeCapturePath(event.Name)
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				key := capturePathKey(path)
				if watched[key] {
					// OS が子ディレクトリの監視も解除するため、同じ場所の再作成に備えて全て除く。
					for directory := range watched {
						if captureWithin(directory, path) {
							delete(watched, directory)
							_ = watcher.Remove(directory)
						}
					}
					scanPending = true
					for _, root := range localRoots {
						if captureWithin(root, path) {
							rootsReady[capturePathKey(root)] = false
						}
					}
				}
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					_ = addTree(path)
					scanPending = true
					continue
				}
			}
			if captureImagePath(path) {
				pending[path] = time.Now()
				if len(pending) > 4096 {
					pending = map[string]time.Time{}
					scanPending = true
				}
			}
		case err, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			s.logger.Warn("Capture watcher reported an error; requesting rescan.", "error", err)
			scanPending = true
		case now := <-flush.C:
			paths := []string{}
			for path, changed := range pending {
				if now.Sub(changed) >= 500*time.Millisecond {
					paths = append(paths, path)
				}
			}
			if len(paths) > 0 || scanPending {
				select {
				case jobs <- captureWatchJob{paths: paths, scan: scanPending}:
					for _, path := range paths {
						delete(pending, path)
					}
					scanPending = false
				default:
				}
			}
		case <-poll:
			requestScan()
		case <-retry.C:
			for _, root := range localRoots {
				key := capturePathKey(root)
				if !rootsReady[key] {
					if rootsReady[key] = addTree(root); rootsReady[key] {
						requestScan()
					}
				}
			}
		}
	}
}

func (c *captureCatalog) markMissing(ctx context.Context, path string) error {
	lock := &c.locks[c.lockIndex(path)]
	lock.Lock()
	defer lock.Unlock()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return nil
	}
	_, err := c.writeDB.ExecContext(ctx, `UPDATE capture_files SET status='Missing',missing_since=COALESCE(missing_since,?) WHERE file_path=?`, database.FormatDBTime(c.clock()), normalizeCapturePath(path))
	if err == nil {
		c.sectionsGeneration.Add(1)
	}
	return err
}
