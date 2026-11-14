package metadata

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

// recordingWatchPlan は一つの保存先の監視方式と、他の監視へ任せる子ルートを保持する。
// folder は設定値の絶対パス、canonical はリンク解決後の実体のパス。
type recordingWatchPlan struct {
	folder, canonical string
	polling           bool
	pollInterval      time.Duration
	excluded          []string
}

func (p recordingWatchPlan) excludes(file string) bool {
	for _, root := range p.excluded {
		if recordedWithin(file, root) {
			return true
		}
	}
	return false
}

type recordingMount struct{ point, filesystem string }

// recordingWatchEnvironment は本家の Linux 判定・watchfiles の環境変数をまとめる。
// mounted は読み取り順を保ち、同一マウントポイントの最後のエントリーを優先する。
type recordingWatchEnvironment struct {
	platform, forcePolling, pollDelay, kernelRelease string
	mounted                                          []recordingMount
	mountError                                       error
	resolve                                          func(string) (string, error)
}

func (s *Service) recordingWatchPlans() []recordingWatchPlan {
	environment := recordingWatchEnvironment{platform: runtime.GOOS, forcePolling: os.Getenv("WATCHFILES_FORCE_POLLING"), pollDelay: os.Getenv("WATCHFILES_POLL_DELAY_MS"), resolve: filepath.EvalSymlinks}
	if runtime.GOOS == "linux" {
		if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			environment.kernelRelease = string(kernel)
		}
		if environment.forcePolling == "" {
			environment.mounted, environment.mountError = recordingMounts()
			if environment.mountError != nil {
				s.logger().Warn("Failed to detect recording filesystems; using default file watching.", "error", environment.mountError)
			}
		}
	}
	folders := []string{}
	for _, folder := range s.Config.RecordedFolders {
		if absolute, err := filepath.Abs(folder); err == nil {
			folder = absolute
		}
		folders = append(folders, filepath.Clean(folder))
	}
	return groupRecordingWatchPaths(folders, s.Config.RecordedFoldersPollingInterval, environment)
}

// groupRecordingWatchPaths は固定した RecordedScanTask.__getRecordingWatchPaths と同じ分類を行う。
// Windows は SMB の変更通知を使い、Linux のネットワーク FS とそのサブマウントだけを自動巡回へ回す。
func groupRecordingWatchPaths(folders []string, minutes float64, environment recordingWatchEnvironment) []recordingWatchPlan {
	if minutes <= 0 || math.IsNaN(minutes) || math.IsInf(minutes, 0) {
		minutes = 5
	}
	milliseconds := math.RoundToEven(minutes * 60 * 1000)
	if milliseconds > float64(math.MaxInt64/int64(time.Millisecond)) {
		milliseconds = float64(math.MaxInt64 / int64(time.Millisecond))
	}
	if override, ok := recordingPollingMilliseconds(environment.pollDelay); ok {
		milliseconds = override
	}
	period := time.Duration(milliseconds) * time.Millisecond
	// watchfiles の 0ms 指定も受け付ける。Go の ticker は正値が必要なので最小単位を使う。
	if period <= 0 {
		period = time.Nanosecond
	}
	force := environment.platform == "linux" && strings.Contains(strings.ToLower(environment.kernelRelease), "microsoft-standard")
	if environment.forcePolling != "" {
		value := strings.ToLower(environment.forcePolling)
		force = value != "false" && value != "disable" && value != "disabled"
	}
	autoNetwork := environment.platform == "linux" && environment.forcePolling == "" && environment.mountError == nil
	mounted := map[string]string{}
	for _, mount := range environment.mounted {
		mounted[path.Clean(mount.point)] = strings.ToLower(mount.filesystem)
	}
	result := make([]recordingWatchPlan, 0, len(folders))
	nativeRoots := []string{}
	networkPlans := map[int]bool{}
	for _, folder := range folders {
		canonical, err := environment.resolve(folder)
		if err != nil {
			canonical = folder
		}
		network := false
		if autoNetwork && err == nil {
			best := -1
			for point, filesystem := range mounted {
				if recordingPOSIXWithin(canonical, point) && len(point) > best {
					best = len(point)
					network = recordingNetworkFilesystem(filesystem)
				}
			}
			for point, filesystem := range mounted {
				if recordingPOSIXWithin(point, canonical) && recordingNetworkFilesystem(filesystem) {
					network = true
				}
			}
		}
		if network {
			networkPlans[len(result)] = true
		} else {
			nativeRoots = append(nativeRoots, folder, canonical)
		}
		result = append(result, recordingWatchPlan{folder: folder, canonical: canonical, polling: force || network, pollInterval: period})
	}
	for index := range networkPlans {
		result[index].excluded = append([]string{}, nativeRoots...)
	}
	return result
}

func recordingPOSIXWithin(file, root string) bool {
	file = path.Clean(file)
	root = path.Clean(root)
	return file == root || strings.HasPrefix(file, strings.TrimRight(root, "/")+"/")
}

func recordingNetworkFilesystem(filesystem string) bool {
	switch filesystem {
	case "cifs", "smb", "smbfs", "smb3", "nfs", "nfs4", "fuse.sshfs":
		return true
	default:
		return false
	}
}

// recordingPollingMilliseconds は Python str.isdecimal()/int() と同じ十進数字だけを受理する。
func recordingPollingMilliseconds(text string) (float64, bool) {
	if text == "" {
		return 0, false
	}
	value := 0.0
	for _, character := range text {
		digit := -1
		if unicode.Is(unicode.Nd, character) {
			for _, span := range unicode.Nd.R16 {
				if uint32(character) >= uint32(span.Lo) && uint32(character) <= uint32(span.Hi) && (uint32(character)-uint32(span.Lo))%uint32(span.Stride) == 0 {
					digit = int((uint32(character) - uint32(span.Lo)) / uint32(span.Stride) % 10)
					break
				}
			}
			if digit < 0 {
				for _, span := range unicode.Nd.R32 {
					if uint32(character) >= span.Lo && uint32(character) <= span.Hi && (uint32(character)-span.Lo)%span.Stride == 0 {
						digit = int((uint32(character) - span.Lo) / span.Stride % 10)
						break
					}
				}
			}
		}
		if digit < 0 {
			return 0, false
		}
		value = value*10 + float64(digit)
	}
	if value > float64(math.MaxInt64/int64(time.Millisecond)) {
		value = float64(math.MaxInt64 / int64(time.Millisecond))
	}
	return value, true
}

// compactRecordingWatchPlans は重なった同じ実体を二重に解析しないよう監視の担当範囲を絞る。
// ネットワーク親ルートが通常監視の子を除外している場合は、その子の担当を必ず残す。
func compactRecordingWatchPlans(plans []recordingWatchPlan) []recordingWatchPlan {
	result := []recordingWatchPlan{}
	for i, plan := range plans {
		covered := false
		for j, other := range plans {
			if i == j || other.polling != plan.polling || other.excludes(plan.canonical) {
				continue
			}
			if recordedWithin(plan.canonical, other.canonical) && (recordedPathKey(plan.canonical) != recordedPathKey(other.canonical) || j < i) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, plan)
		}
	}
	return result
}

// recordingMounts は bind mount を含む現在の名前空間を読む。ファイル名内の空白等も戻す。
func recordingMounts() ([]recordingMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	result := []recordingMount{}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid mountinfo record")
		}
		fields := strings.Fields(parts[0])
		tail := strings.Fields(parts[1])
		if len(fields) < 6 || len(tail) < 3 {
			return nil, fmt.Errorf("invalid mountinfo fields")
		}
		result = append(result, recordingMount{point: unescape.Replace(fields[4]), filesystem: tail[0]})
	}
	return result, scanner.Err()
}
