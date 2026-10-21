package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BrowserLaunchOptions は CDP browser の起動指定。テストでは localhost の合成ページだけを使う。
type BrowserLaunchOptions struct {
	Executable string
}

// BrowserLauncher は Go のブラウザ起動とモックを切り替えるための依存。
type BrowserLauncher func(context.Context, BrowserLaunchOptions) (BrowserSession, error)

func findBrowser(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if path := os.Getenv("KONOMITV_BROWSER_PATH"); path != "" {
		return path
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
		for _, relative := range []string{"Google/Chrome/Application/chrome.exe", "BraveSoftware/Brave-Browser/Application/brave.exe"} {
			path := filepath.Join(root, filepath.FromSlash(relative))
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

// launchBrowser は既存のユーザープロファイルに触れず、専用の一時 profile を起動する。
func launchBrowser(parent context.Context, options BrowserLaunchOptions) (BrowserSession, error) {
	executable := findBrowser(options.Executable)
	if executable == "" {
		return nil, NewBrowserSetupError(UnavailableBrowserMessage)
	}
	profile, err := os.MkdirTemp("", "konomitv-twitter-")
	if err != nil {
		return nil, fmt.Errorf("create browser profile: %w", err)
	}
	// 削除対象を起動前に検証。既存 profile や作業ディレクトリは削除しない。
	profile, err = filepath.Abs(profile)
	if err != nil || !strings.HasPrefix(filepath.Base(profile), "konomitv-twitter-") || filepath.Dir(profile) != filepath.Clean(os.TempDir()) {
		return nil, fmt.Errorf("invalid temporary browser profile")
	}
	command := exec.Command(executable,
		"--headless=new", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update",
		"--disable-gpu", "--renderer-process-limit=1", "--disk-cache-size=1048576", "--media-cache-size=1048576",
		"--disable-sync", "--disable-extensions", "--disable-notifications", "--disable-default-apps", "about:blank")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	configureBrowserProcess(command)
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return nil, NewBrowserSetupError(UnavailableBrowserMessage)
	}
	terminateTree, err := ownBrowserProcess(command)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("own browser process: %w", err)
	}
	processDone := make(chan struct{})
	go func() { _ = command.Wait(); close(processDone) }()
	var stopOnce sync.Once
	stop := func(ctx context.Context) error {
		stopOnce.Do(func() {
			select {
			case <-processDone:
			case <-time.After(time.Second):
				_ = terminateTree()
				_ = command.Process.Kill()
				select {
				case <-processDone:
				case <-time.After(time.Second):
				}
			}
			_ = terminateTree()
			// profile はこの関数が MkdirTemp で作った絶対パスだけを削除する。
			_ = os.RemoveAll(profile)
		})
		return nil
	}
	success := false
	defer func() {
		if !success {
			_ = stop(context.Background())
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, readErr := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		lines := strings.Fields(string(data))
		if readErr == nil && len(lines) >= 1 {
			port, parseErr := strconv.Atoi(lines[0])
			if parseErr == nil && port > 0 && port < 65536 {
				address := "http://" + net.JoinHostPort("127.0.0.1", lines[0]) + "/json/list"
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
				response, requestErr := client.Do(request)
				if requestErr == nil {
					var pages []struct {
						Type string `json:"type"`
						URL  string `json:"webSocketDebuggerUrl"`
					}
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&pages)
					_ = response.Body.Close()
					if decodeErr == nil {
						for _, page := range pages {
							if page.Type == "page" {
								session, err := newCDPSession(ctx, page.URL)
								if err != nil {
									return nil, err
								}
								session.stopProcess = stop
								success = true
								return session, nil
							}
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, NewBrowserSetupError("ヘッドレスブラウザとの接続がタイムアウトしました。Chrome または Brave の起動状態を確認してください。")
		case <-processDone:
			return nil, NewBrowserSetupError("ヘッドレスブラウザとの接続に失敗しました。Chrome または Brave の起動状態を確認してください。")
		case <-ticker.C:
		}
	}
}
