package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	urlerrors "github.com/nlnwa/whatwg-url/errors"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"gopkg.in/yaml.v3"
)

// SettingsListener は設定検証時の待ち受けポートと所有プロセス。
// PID が 0 のエントリーは本家の pid=None と同じく検査対象外にする。
type SettingsListener struct {
	Port, PID, ParentPID int
	ParentKnown          bool
}

// ServerSettingsValidationOptions は設定に明示された項目の実機検査に使う依存関係。
// BypassEnvironment は起動前の移行・検査用で、型・パスの存在検証は省略しない。
type ServerSettingsValidationOptions struct {
	BypassEnvironment     bool
	Logger                *slog.Logger
	Architecture          string
	LibraryPath           func(string) string
	EDCBStatus            func(context.Context, string) string
	HTTPClient            *http.Client
	RequestHeaders        http.Header
	RunEncoder            func(context.Context, string, string, bool) ([]byte, error)
	Listeners             func(context.Context) ([]SettingsListener, error)
	CurrentPID, ParentPID int
	Stat                  func(string) (fs.FileInfo, error)
	LocalSystem           func() (bool, error)
}

type serverSettingsValidation struct {
	ctx     context.Context
	options ServerSettingsValidationOptions
}

// ServerSettings は本文の型・項目間の依存関係・外部機器を本家の宣言順に検証する。
// 項目の省略と明示指定を区別し、既定値に接続確認等の副作用を発生させない。
func (body *SettingsBody) ServerSettings(ctx context.Context, options ServerSettingsValidationOptions) (map[string]any, []SettingsValidationError, error) {
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.Architecture == "" {
		options.Architecture = runtime.GOARCH
		if runtime.GOARCH == "amd64" {
			options.Architecture = "x86_64"
		}
		if runtime.GOARCH == "arm64" {
			options.Architecture = "aarch64"
		}
	}
	if options.Stat == nil {
		options.Stat = os.Stat
	}
	if options.LocalSystem == nil {
		options.LocalSystem = runningAsLocalSystem
	}
	if options.Listeners == nil {
		options.Listeners = settingsListeningPorts
	}
	if options.CurrentPID == 0 {
		options.CurrentPID = os.Getpid()
		options.ParentPID = os.Getppid()
	}
	if options.RunEncoder == nil {
		options.RunEncoder = runSettingsEncoder
	}
	if options.HTTPClient == nil {
		// HTTPX の既定値と同じくリダイレクトは追跡しない。
		options.HTTPClient = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	validator := settingsValidator{model: settingsValidationModels["ServerSettings"], order: body.order,
		server: &serverSettingsValidation{ctx: ctx, options: options}}
	result, details := body.validate(&validator)
	return result, details, validator.fatal
}

func (s *serverSettingsValidation) valueError(v *settingsValidator, path []any, input any, message string) {
	v.add("value_error", "Value error, "+message, path, input, map[string]any{"error": map[string]any{}})
}

func (s *serverSettingsValidation) formattedValue(v *settingsValidator, input any, schema *settingsFieldSchema, path []any) any {
	text, ok := input.(string)
	if !ok {
		kind, message := "path_type", "Input is not a valid path for <class 'pathlib._local.Path'>"
		if schema.Format == "uri" {
			kind, message = "url_type", "URL input should be a string or URL"
		}
		v.add(kind, message, path, input, nil)
		return nil
	}
	if schema.Format == "uri" {
		parsed, err := whatwg.Parse(text)
		if err != nil {
			message := settingsURLError(err, text)
			v.add("url_parsing", "Input should be a valid URL, "+message, path, input, map[string]any{"error": message})
			return nil
		}
		allowed := []string{"http", "https"}
		if path[len(path)-1] == "edcb_url" {
			allowed = []string{"tcp"}
		}
		if !containsString(allowed, parsed.Scheme()) {
			expected := settingLiteralExpected(allowed)
			v.add("url_scheme", "URL scheme should be "+expected, path, input, map[string]any{"expected_schemes": expected})
			return nil
		}
		return parsed.Href(false)
	}
	// pathlib の字句正規化は .. を解決しない。実ファイルへのアクセス時だけ OS に解決させる。
	normalized := settingsPath(text)
	info, err := s.options.Stat(normalized)
	if err != nil && !(errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP) || settingsIgnoredPathError(err)) {
		v.fatal = err
		return nil
	}
	directory := schema.Format == "directory-path"
	if err != nil || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		kind, message := "path_not_file", "Path does not point to a file"
		if directory {
			kind, message = "path_not_directory", "Path does not point to a directory"
		}
		v.add(kind, message, path, input, nil)
		return nil
	}
	return normalized
}

func settingsURLError(err error, input string) string {
	switch urlerrors.Type(err) {
	case urlerrors.PortInvalid, urlerrors.PortOutOfRange:
		return "invalid port number"
	case urlerrors.HostMissing:
		return "empty host"
	case urlerrors.MissingSchemeNonRelativeURL:
		if strings.TrimSpace(input) == "" {
			return "input is empty"
		}
		return "relative URL without a base"
	case urlerrors.IPv4TooManyParts, urlerrors.IPv4NonNumericPart, urlerrors.IPv4OutOfRangePart:
		return "invalid IPv4 address"
	case urlerrors.DomainInvalidCodePoint, urlerrors.DomainToASCII:
		return "invalid international domain name"
	case urlerrors.HostInvalidCodePoint:
		return "invalid domain character"
	default:
		if strings.Contains(string(urlerrors.Type(err)), "IPv6") {
			return "invalid IPv6 address"
		}
		return "invalid international domain name"
	}
}

func settingsPath(text string) string {
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "/", `\`)
	}
	separator := string(filepath.Separator)
	volume := filepath.VolumeName(text)
	tail := strings.TrimPrefix(text, volume)
	root := ""
	if strings.HasPrefix(tail, separator) {
		root = separator
		// POSIX の先頭の二重スラッシュだけは pathlib も保持する。
		if runtime.GOOS != "windows" && strings.HasPrefix(tail, "//") && !strings.HasPrefix(tail, "///") {
			root = "//"
		}
	}
	parts := []string{}
	for _, part := range strings.Split(tail, separator) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	result := volume + root + strings.Join(parts, separator)
	if result == "" {
		return "."
	}
	return result
}

func (s *serverSettingsValidation) beforeField(v *settingsValidator, input any, path []any) bool {
	if s.options.BypassEnvironment || len(path) != 3 {
		return true
	}
	section, name := path[1], path[2]
	label := ""
	if section == "video" && name == "recorded_folders" {
		label = "録画フォルダ"
	}
	if section == "capture" && name == "upload_folders" {
		label = "キャプチャ画像の保存先フォルダ"
	}
	if label == "" {
		return true
	}
	array, ok := input.([]any)
	if !ok {
		return true
	}
	for _, item := range array {
		folder, ok := item.(string)
		if !ok {
			continue
		}
		if strings.ContainsRune(folder, 0) {
			s.valueError(v, path, input, "stat: embedded null character in path")
			return false
		}
		if _, err := s.options.Stat(settingsPath(folder)); err == nil {
			continue
		} else {
			denied := errors.Is(err, fs.ErrPermission)
			drive := filepath.VolumeName(folder)
			network := strings.HasPrefix(folder, `\\`)
			if drive != "" {
				if _, err := s.options.Stat(drive + `\`); err != nil {
					network = true
				}
			}
			if denied || network {
				localSystem, err := s.options.LocalSystem()
				if err != nil {
					v.fatal = err
					return false
				}
				if localSystem {
					s.valueError(v, path, input, fmt.Sprintf("%s (%s) に、KonomiTV のサービスからアクセスできません。\nKonomiTV のサービスは LocalSystem アカウントで実行されているため、ネットワークドライブや NAS 上のフォルダにはアクセスできません。\nインストーラーの [サービスの実行アカウントを変更] から、実行アカウントを User に切り替えてください。", label, folder))
					return false
				}
			}
			if denied {
				s.valueError(v, path, input, fmt.Sprintf("%s (%s) にアクセスする権限がありません。\nKonomiTV を実行しているユーザーに、このフォルダを読み取る権限があるかを確認してください。", label, folder))
				return false
			}
		}
	}
	return true
}

func (s *serverSettingsValidation) afterField(v *settingsValidator, input, value any, path []any, siblings map[string]any) any {
	if len(path) != 3 {
		return value
	}
	section, name := path[1], path[2]
	if section == "general" && (name == "edcb_url" || name == "mirakurun_url") {
		parsed, err := whatwg.Parse(strings.TrimRight(value.(string), "/") + "/")
		if err != nil {
			v.fatal = err
			return nil
		}
		value = parsed.Href(false)
		if s.options.BypassEnvironment {
			return value
		}
		if name == "edcb_url" && siblings["backend"] == "EDCB" {
			if parsed.Hostname() == "" || (parsed.Port() == "" && parsed.Hostname() != "edcb-namedpipe") {
				s.valueError(v, path, input, "URL 内にホスト名またはポートが指定されていません。\nEDCB の URL を間違えている可能性があります。")
				return nil
			}
			status := "Unknown"
			if s.options.EDCBStatus != nil {
				ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
				status = s.options.EDCBStatus(ctx, value.(string))
				cancel()
			}
			if status == "Unknown" {
				s.valueError(v, path, input, fmt.Sprintf("EDCB (%s) にアクセスできませんでした。\nEDCB が起動していないか、URL を間違えている可能性があります。", value))
				return nil
			}
			s.options.Logger.Info(fmt.Sprintf("Backend: EDCB (%s) Status: %s", value, status))
		}
		if name == "mirakurun_url" && (siblings["backend"] == "Mirakurun" || siblings["always_receive_tv_from_mirakurun"] == true) {
			if err := s.validateMirakurun(value.(string)); err != nil {
				s.valueError(v, path, input, err.Error())
				return nil
			}
		}
		return value
	}
	if s.options.BypassEnvironment {
		return value
	}
	if section == "general" && name == "encoder" {
		if message, err := s.validateEncoder(value.(string)); err != nil {
			v.fatal = err
		} else if message != "" {
			s.valueError(v, path, input, message)
		}
	}
	if section == "server" && name == "port" {
		port, conversionErr := value.(json.Number).Int64()
		if conversionErr != nil || port < 1024 || port > 65525 {
			s.valueError(v, path, input, "ポート番号の設定が不正なため、KonomiTV を起動できません。\n設定したポート番号が 1024 ~ 65525 (65535 ではない) の間に収まっているかを確認してください。")
			return nil
		}
		listeners, err := s.options.Listeners(s.ctx)
		if err != nil {
			v.fatal = err
			return nil
		}
		used := map[int]bool{}
		for _, listener := range listeners {
			if listener.PID == 0 {
				continue
			}
			if listener.ParentKnown && (listener.PID == s.options.CurrentPID || listener.PID == s.options.ParentPID || listener.ParentPID == s.options.CurrentPID || listener.ParentPID == s.options.ParentPID) {
				continue
			}
			used[listener.Port] = true
		}
		if used[int(port)] {
			s.valueError(v, path, input, fmt.Sprintf("ポート %d は他のプロセスで使われているため、KonomiTV を起動できません。\n重複して KonomiTV を起動していないか、他のソフトでポート %d を使っていないかを確認してください。", port, port))
		} else if used[int(port)+10] {
			s.valueError(v, path, input, fmt.Sprintf("ポート %d (%d + 10) は他のプロセスで使われているため、KonomiTV を起動できません。\n重複して KonomiTV を起動していないか、他のソフトでポート %d を使っていないかを確認してください。", port+10, port, port+10))
		}
	}
	return value
}

func (s *serverSettingsValidation) validateMirakurun(base string) error {
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/tuners", nil)
	if err != nil {
		return err
	}
	request.Header = s.options.RequestHeaders.Clone()
	response, err := s.options.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("Mirakurun / mirakc (%s) にアクセスできませんでした。\nMirakurun / mirakc が起動していないか、URL を間違えている可能性があります。", base)
	}
	defer response.Body.Close()
	header := strings.ToLower(response.Header.Get("Server"))
	name, version := "Mirakurun", "unknown"
	if strings.Contains(header, "mirakc") {
		name = "mirakc"
	}
	if parts := strings.Split(header, "/"); len(parts) > 1 {
		version = parts[len(parts)-1]
	}
	var data any
	decoder := json.NewDecoder(response.Body)
	decodeErr := decoder.Decode(&data)
	var extra any
	trailingErr := decoder.Decode(&extra)
	_, array := data.([]any)
	if response.StatusCode != http.StatusOK || decodeErr != nil || trailingErr != io.EOF || !array || version == "unknown" {
		return fmt.Errorf("%s は %s の URL ではありません。\n%s の URL を間違えている可能性があります。", base, name, name)
	}
	s.options.Logger.Info(fmt.Sprintf("Backend: %s %s (%s)", name, version, base))
	return nil
}

var settingsEncoderCopyright = regexp.MustCompile(` Copyright.*$| by rigaya.*$`)

func (s *serverSettingsValidation) validateEncoder(encoder string) (string, error) {
	arch := s.options.Architecture
	if (arch == "AMD64" || arch == "x86_64") && encoder == "rkmppenc" {
		return "x64 アーキテクチャでは rkmppenc は使用できません。\n利用するエンコーダーを FFmpeg・QSVEncC・NVEncC・VCEEncC のいずれかに変更してください。", nil
	}
	if arch == "aarch64" && containsString([]string{"QSVEncC", "NVEncC", "VCEEncC"}, encoder) {
		return "arm64 アーキテクチャでは QSVEncC・NVEncC・VCEEncC は使用できません。\n利用するエンコーダーを FFmpeg・rkmppenc のいずれかに変更してください。", nil
	}
	path := encoder
	if s.options.LibraryPath != nil {
		path = s.options.LibraryPath(encoder)
	}
	if encoder != "FFmpeg" {
		output, err := s.options.RunEncoder(s.ctx, path, "--check-hw", false)
		if err != nil {
			return "", err
		}
		if !utf8.Valid(output) {
			return "", fmt.Errorf("encoder output is not valid UTF-8")
		}
		lines := []string{}
		for _, line := range strings.Split(string(output), "\n") {
			if !strings.Contains(line, "reader:") {
				lines = append(lines, line)
			}
		}
		text := strings.Join(lines, "\n")
		if strings.Contains(text, "unavailable.") {
			return fmt.Sprintf("お使いの環境では %s がサポートされていないため、KonomiTV を起動できません。\n別のエンコーダーを選択するか、%s の動作環境を整備してください。", encoder, encoder), nil
		}
		if !strings.Contains(text, "H.265/HEVC") {
			s.options.Logger.Warn("H.265/HEVC encoding is unavailable.", "encoder", encoder)
		}
	}
	output, err := s.options.RunEncoder(s.ctx, path, "--version", true)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(output) {
		return "", fmt.Errorf("encoder output is not valid UTF-8")
	}
	version := settingsEncoderCopyright.ReplaceAllString(strings.Split(string(output), "\n")[0], "")
	version = strings.TrimSpace(strings.ReplaceAll(version, "ffmpeg", "FFmpeg"))
	s.options.Logger.Info("Encoder: " + version)
	return "", nil
}

func runSettingsEncoder(ctx context.Context, path, argument string, mergeStderr bool) ([]byte, error) {
	command := exec.CommandContext(ctx, path, argument)
	// Linux 配布物の共有ライブラリーも、エンコーダー自身と同じディレクトリーから解決する。
	if runtime.GOOS != "windows" {
		command.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(path)+":"+os.Getenv("LD_LIBRARY_PATH"))
	}
	var output []byte
	var err error
	if mergeStderr {
		output, err = command.CombinedOutput()
	} else {
		output, err = command.Output()
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && ctx.Err() == nil {
		return output, nil
	}
	return output, err
}

// MarshalServerSettingsResponse は本家の応答モデルの順序・数値形式で出力する。
func MarshalServerSettingsResponse(settings map[string]any) (string, error) {
	model := settingsValidationModels["ServerSettings"]
	order := map[string][]string{"": model.FieldOrder["ServerSettings"]}
	for name, schema := range model.Schema.Properties {
		definition := model.Schema.Definitions[strings.TrimPrefix(schema.Ref, "#/$defs/")]
		order[name] = model.FieldOrder[definition.Title]
	}
	return encodePythonJSON(settingsResponseValue(settings), order, "")
}

// ServerSettingsSnapshot は起動時にロードした設定のうち本家の公開項目だけを返す。
// 設定の保存後もこの値を変更しない。本家と同様、反映には再起動が必要になる。
func (config *Config) ServerSettingsSnapshot() map[string]any {
	return map[string]any{
		"general": map[string]any{"backend": config.General.Backend, "always_receive_tv_from_mirakurun": config.General.AlwaysReceiveTVFromMirakurun, "edcb_url": config.General.EDCBURL, "mirakurun_url": config.General.MirakurunURL, "encoder": config.General.Encoder, "program_update_interval": config.General.ProgramUpdateInterval, "debug": config.General.Debug, "debug_encoder": config.General.DebugEncoder},
		"server":  map[string]any{"port": config.Server.Port, "custom_https_certificate": config.Server.CustomHTTPSCertificate, "custom_https_private_key": config.Server.CustomHTTPSPrivateKey},
		"tv":      map[string]any{"preferred_terrestrial_region": config.TV.PreferredTerrestrialRegion, "max_alive_time": int(config.TV.MaxAliveTime), "debug_mode_ts_path": config.TV.DebugModeTSPath},
		"video":   map[string]any{"recorded_folders": config.Video.RecordedFolders, "exclude_scan_paths": config.Video.ExcludeScanPaths, "recorded_folders_polling_interval": config.Video.RecordedFoldersPollingInterval},
		"capture": map[string]any{"upload_folders": config.Capture.UploadFolders, "upload_folders_polling_interval": config.Capture.UploadFoldersPollingInterval},
	}
}

// stripDockerSettingsPaths は本家 SaveConfig と同じホスト側パスへの変換を行う。
func stripDockerSettingsPaths(settings map[string]any) {
	for _, entry := range [][2]string{{"video", "recorded_folders"}, {"video", "exclude_scan_paths"}, {"capture", "upload_folders"}} {
		section := settings[entry[0]].(map[string]any)
		for i, item := range section[entry[1]].([]any) {
			section[entry[1]].([]any)[i] = strings.ReplaceAll(item.(string), "/host-rootfs", "")
		}
	}
	tv := settings["tv"].(map[string]any)
	if path, ok := tv["debug_mode_ts_path"].(string); ok {
		tv["debug_mode_ts_path"] = strings.ReplaceAll(path, "/host-rootfs", "")
	}
}

// SaveValidatedServerSettings は検証済みの本家の五セクションだけを保存する。
func SaveValidatedServerSettings(path string, settings map[string]any, docker bool) error {
	if docker {
		settings = deepCopyMap(settings)
		stripDockerSettingsPaths(settings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("settings YAML must contain a mapping")
	}
	root := document.Content[0]
	model := settingsValidationModels["ServerSettings"]
	for _, section := range model.FieldOrder["ServerSettings"] {
		values := settings[section].(map[string]any)
		node := settingsYAMLField(root, section)
		if node.Kind != yaml.MappingNode {
			node.Kind = yaml.MappingNode
			node.Tag = "!!map"
			node.Value = ""
			node.Content = nil
			node.Style = 0
		}
		definition := model.Schema.Definitions[strings.TrimPrefix(model.Schema.Properties[section].Ref, "#/$defs/")]
		for _, key := range model.FieldOrder[definition.Title] {
			target := settingsYAMLField(node, key)
			next, err := settingsYAMLValue(values[key])
			if err != nil {
				return err
			}
			// キーの位置・未知の項目と、値に付いていたコメントを保持する。
			next.HeadComment = target.HeadComment
			next.LineComment = target.LineComment
			next.FootComment = target.FootComment
			*target = *next
		}
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(4)
	if err := encoder.Encode(&document); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	// 設定ファイルがシンボリックリンクでも、リンクを残して実体を原子的に置き換える。
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".konomitv-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(output.Bytes()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

func settingsYAMLField(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	value := &yaml.Node{}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value
}

func settingsYAMLValue(value any) (*yaml.Node, error) {
	if number, ok := value.(json.Number); ok {
		tag := "!!int"
		if strings.ContainsAny(number.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: number.String()}, nil
	}
	if number, ok := value.(float64); ok {
		text := formatPythonFloat(number)
		switch {
		case math.IsNaN(number):
			text = ".nan"
		case math.IsInf(number, 1):
			text = ".inf"
		case math.IsInf(number, -1):
			text = "-.inf"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: text}, nil
	}
	if array, ok := value.([]any); ok {
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range array {
			child, err := settingsYAMLValue(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	}
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	return node, nil
}

// settingsPort は /proc と netstat に共通する安全な数値変換。
func settingsPort(text string, base int) int {
	number, _ := strconv.ParseUint(text, base, 16)
	return int(number)
}
