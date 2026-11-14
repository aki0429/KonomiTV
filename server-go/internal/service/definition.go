// Package service はGo単独運転のサービス登録と、Go本体・HTTPSの子プロセスを管理する。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

// Definition はこのインストールが所有するサービスと実行ファイルを記録する。
// Root内のデータ・録画・認証情報をサービス削除の対象には含めない。
type Definition struct {
	Format         int    `json:"format"`
	Name           string `json:"name"`
	Root           string `json:"root"`
	BackendFile    string `json:"backend_file"`
	ManagerBinary  string `json:"manager_binary"`
	ListenOverride string `json:"listen_override,omitempty"`
	ExternalHTTPS  bool   `json:"external_https"`
}

// Command はシェルを介さずに起動する実行ファイル・引数・作業ディレクトリ。
type Command struct {
	Path string
	Args []string
	Dir  string
}

// Launch は設定を再読込して得た一回分の起動構成。
type Launch struct {
	Backend      Command
	HTTPS        *Command
	HealthURL    string
	InternalHost string
}

// NewDefinition は準備済みのGo配布ルートを検査する。DBを作成・更新しない。
func NewDefinition(ctx context.Context, root, name, backend, listen string, externalHTTPS bool) (*Definition, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = filepath.Base(absolute)
	}
	if backend == "" {
		backend = "kgo"
		if runtime.GOOS == "windows" {
			backend += ".exe"
		}
	}
	d := &Definition{Format: 1, Name: name, Root: absolute, BackendFile: backend, ListenOverride: listen, ExternalHTTPS: externalHTTPS}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	for _, relative := range []string{backend, "client/dist/index.html", "server/static/interlaced.dat"} {
		info, err := os.Stat(filepath.Join(absolute, filepath.FromSlash(relative)))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("prepared Go installation file is missing: %s", relative)
		}
	}
	if _, err := d.Launch(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Validate は保存された所有情報が他のフォルダやサービスへ逸脱していないか検査する。
func (d *Definition) Validate() error {
	if d.Format != 1 || !validName.MatchString(d.Name) || !filepath.IsAbs(d.Root) {
		return fmt.Errorf("invalid native service definition")
	}
	if !filepath.IsLocal(d.BackendFile) || filepath.Base(d.BackendFile) != d.BackendFile {
		return fmt.Errorf("backend must be a file in the installation root")
	}
	if d.ManagerBinary != "" {
		relative, err := filepath.Rel(filepath.Join(d.Root, "server", "go-native", "Service"), d.ManagerBinary)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("service manager must be inside the native Service directory")
		}
	}
	if d.ListenOverride != "" {
		host, port, err := net.SplitHostPort(d.ListenOverride)
		value, numberErr := strconv.Atoi(port)
		if err != nil || numberErr != nil || value < 1 || value > 65535 {
			return fmt.Errorf("invalid internal listen address")
		}
		if !localListenHost(host) {
			return fmt.Errorf("internal listen host must belong to this machine")
		}
	}
	return nil
}

func localListenHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	if address == nil {
		return false
	}
	if address.IsUnspecified() || address.IsLoopback() {
		return true
	}
	interfaces, _ := net.InterfaceAddrs()
	for _, entry := range interfaces {
		if network, ok := entry.(*net.IPNet); ok && network.IP.Equal(address) {
			return true
		}
	}
	return false
}

// StatePath はサービス所有情報の保存場所。
func (d *Definition) StatePath() string {
	return filepath.Join(d.Root, "server", "data", "native-service.json")
}

// Launch は再起動のたびにconfig.yamlのポートとカスタム証明書設定を反映する。
func (d *Definition) Launch(ctx context.Context) (*Launch, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	settings, err := config.LoadValidated(ctx, filepath.Join(d.Root, "config.yaml"), config.ServerSettingsLoadOptions{
		AllowLegacyIPTV: true, Validation: config.ServerSettingsValidationOptions{BypassEnvironment: true},
	})
	if err != nil {
		return nil, err
	}
	if settings.Server.Port > 65525 {
		return nil, fmt.Errorf("server port leaves no room for the internal port")
	}
	internalHost := net.JoinHostPort("127.0.0.77", strconv.Itoa(settings.Server.Port+10))
	listen := d.ListenOverride
	if listen == "" {
		listen = internalHost
	}
	connectHost, port, _ := net.SplitHostPort(listen)
	if connectHost == "" || connectHost == "0.0.0.0" {
		connectHost = "127.0.0.1"
	} else if connectHost == "::" {
		connectHost = "::1"
	}
	launch := &Launch{
		Backend:   Command{Path: filepath.Join(d.Root, d.BackendFile), Args: []string{"-no-proxy", "-listen", listen, "-server-dir", filepath.Join(d.Root, "server")}, Dir: d.Root},
		HealthURL: "http://" + net.JoinHostPort(connectHost, port), InternalHost: internalHost,
	}
	if !d.ExternalHTTPS {
		name := "akebi-https-server.elf"
		if runtime.GOOS == "windows" {
			name = "akebi-https-server.exe"
		}
		binary := filepath.Join(d.Root, "server", "thirdparty", "Akebi", name)
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Akebi executable is missing: %s", binary)
		}
		args := []string{"--listen-address", net.JoinHostPort("0.0.0.0", strconv.Itoa(settings.Server.Port)), "--proxy-pass-url", launch.HealthURL + "/", "--keyless-server-url", "https://akebi.konomi.tv/"}
		if settings.Server.CustomHTTPSCertificate != nil && settings.Server.CustomHTTPSPrivateKey != nil {
			args = append(args, "--custom-certificate", *settings.Server.CustomHTTPSCertificate, "--custom-private-key", *settings.Server.CustomHTTPSPrivateKey)
		}
		launch.HTTPS = &Command{Path: binary, Args: args, Dir: d.Root}
	}
	return launch, nil
}

// Load は指定ルートの所有情報だけを読み込む。別ルートの状態ファイルの流用を拒否する。
func Load(root string) (*Definition, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(absolute, "server", "data", "native-service.json"))
	if err != nil {
		return nil, err
	}
	var d Definition
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	if filepath.Clean(d.Root) != filepath.Clean(absolute) {
		return nil, fmt.Errorf("native service belongs to a different installation root")
	}
	return &d, d.Validate()
}

func save(d *Definition) error {
	if err := d.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(d.StatePath(), append(data, '\n'), 0600)
}

func atomicWrite(target string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".native-service-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Chmod(mode)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), target)
}
