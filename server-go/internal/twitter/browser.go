package twitter

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed browser_assets/zendriver_setup.js
var twitterSetupScript string

const browserMissingMessage = "ヘッドレスブラウザの起動に必要な Chrome または Brave が KonomiTV サーバーにインストールされていません。"
const browserConnectionMessage = "ヘッドレスブラウザとの接続に失敗しました。Chrome または Brave が KonomiTV サーバーにインストールされているかどうかを確認してください。"
const browserTimeoutMessage = "ヘッドレスブラウザのセットアップがタイムアウトしました。Cookie セッションの有効期限が切れたか、アカウントがロック・一時制限されている可能性があります。ブラウザで Twitter (x.com) にアクセスして状態を確認してください。"

// BrowserOptions はブラウザーの実行先とデータ保存先。空の BinaryPath はインストール済み Chrome / Brave を探す。
type BrowserOptions struct {
	BinaryPath string
	DataDir    string
	Logger     *slog.Logger
	// 以下はローカルのブラウザー検証で実サービスへの通信を置き換えるための設定。
	startURL        string
	setupScript     string
	filterURLs      []string
	setupTimeout    time.Duration
	shutdownTimeout time.Duration
	additionalArgs  []string
}

// BrowserBackend は TwitterScrapeBrowser を Go の CDP 接続と専用ブラウザープロセスで実行する。
type BrowserBackend struct {
	// lifecycle は setup / shutdown のみを直列化し、イベント処理中の CDP コマンドを妨げない。
	lifecycle sync.Mutex
	mu        sync.RWMutex
	account   Account
	options   BrowserOptions
	complete  bool
	cdp       *cdpConnection
	session   string
	process   *exec.Cmd
	exited    chan struct{}
	killTree  func()
	profile   string
}

// NewBrowserBackend はアカウント専用のブラウザー処理を生成する。ブラウザーは Setup まで起動しない。
func NewBrowserBackend(account *Account, options BrowserOptions) Backend {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.startURL == "" {
		options.startURL = "https://x.com/"
	}
	if options.setupScript == "" {
		options.setupScript = twitterSetupScript
	}
	if options.setupTimeout == 0 {
		options.setupTimeout = 15 * time.Second
	}
	if options.shutdownTimeout == 0 {
		options.shutdownTimeout = 5 * time.Second
	}
	backend := &BrowserBackend{options: options}
	backend.SetAccount(account)
	return backend
}

// SetAccount は以後の setup と Cookie 同期に使うアカウントを更新する。
func (b *BrowserBackend) SetAccount(account *Account) {
	b.mu.Lock()
	b.account = *account
	b.mu.Unlock()
}

// IsSetupComplete はページ内の API セットアップが成功したかを返す。
func (b *BrowserBackend) IsSetupComplete() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.complete
}

// IsBrowserProcessAlive は自分が起動したブラウザー親プロセスの終了を確認する。
func (b *BrowserBackend) IsBrowserProcessAlive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.exited == nil {
		return false
	}
	select {
	case <-b.exited:
		return false
	default:
		return true
	}
}

func findBrowser(binary, dataDir string) (string, error) {
	if binary != "" {
		if resolved, err := exec.LookPath(binary); err == nil {
			return resolved, nil
		}
		return "", NewBrowserSetupError(browserMissingMessage)
	}
	candidates := []string{"google-chrome-stable", "google-chrome", "brave-browser", "brave", "chromium", "chromium-browser"}
	if runtime.GOOS == "windows" {
		for _, directory := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if directory != "" {
				candidates = append(candidates, filepath.Join(directory, "Google", "Chrome", "Application", "chrome.exe"),
					filepath.Join(directory, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"))
			}
		}
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser")
	}
	// システムにない場合は、KonomiTV 専用に配置したブラウザーも利用できる。
	if dataDir != "" {
		filename := "chrome"
		if runtime.GOOS == "windows" {
			filename = "chrome.exe"
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(dataDir), "go-native", "Chrome", filename))
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", NewBrowserSetupError(browserMissingMessage)
}

// Setup は一時プロファイルでブラウザーを起動し、本家の JS を main.js の停止位置へ注入する。
func (b *BrowserBackend) Setup(ctx context.Context) (resultErr error) {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	if b.IsSetupComplete() && b.IsBrowserProcessAlive() {
		return nil
	}
	b.shutdownLocked()
	binary, err := findBrowser(b.options.BinaryPath, b.options.DataDir)
	if err != nil {
		return err
	}
	profile, err := os.MkdirTemp("", "konomitv-twitter-")
	if err != nil {
		return err
	}
	args := []string{"--headless=new", "--user-data-dir=" + profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0",
		"--remote-allow-origins=http://localhost", "--no-first-run", "--no-default-browser-check", "--bwsi", "--disable-gpu", "--use-gl=swiftshader",
		"--renderer-process-limit=1", "--disk-cache-size=1048576", "--media-cache-size=1048576", "--disable-client-side-phishing-detection",
		"--disable-component-extensions-with-background-pages", "--disable-domain-reliability", "--disable-default-apps", "--disable-extensions",
		"--disable-notifications", "--disable-print-preview", "--disable-speech-api", "--disable-sync", "--disable-translate", "about:blank"}
	args = append(args, b.options.additionalArgs...)
	cmd := exec.Command(binary, args...)
	configureBrowserProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return NewBrowserSetupError(browserConnectionMessage)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	owner, err := ownBrowserProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		<-exited
		_ = os.RemoveAll(profile)
		return fmt.Errorf("own browser process: %w", err)
	}
	b.mu.Lock()
	b.process, b.exited, b.killTree, b.profile = cmd, exited, owner, profile
	b.mu.Unlock()
	defer func() {
		if resultErr != nil {
			b.shutdownLocked()
		}
	}()
	connectionCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var endpoint string
	for endpoint == "" {
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		parts := strings.Split(strings.TrimSpace(string(data)), "\n")
		if err == nil && len(parts) == 2 {
			port, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err == nil && port > 0 && port < 65536 && strings.HasPrefix(parts[1], "/devtools/browser/") {
				endpoint = fmt.Sprintf("ws://127.0.0.1:%d%s", port, strings.TrimSpace(parts[1]))
				break
			}
		}
		select {
		case <-exited:
			return NewBrowserSetupError(browserConnectionMessage)
		case <-connectionCtx.Done():
			return NewBrowserSetupError(browserConnectionMessage)
		case <-time.After(50 * time.Millisecond):
		}
	}
	connection, err := dialCDP(connectionCtx, endpoint)
	if err != nil {
		b.options.Logger.Warn("Failed to connect to browser", "error", err)
		return NewBrowserSetupError(browserConnectionMessage)
	}
	b.mu.Lock()
	b.cdp = connection
	b.mu.Unlock()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := connection.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := connection.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return err
	}
	session := attached.SessionID
	var window struct {
		WindowID int `json:"windowId"`
	}
	if connection.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": target.TargetID}, &window) == nil {
		_ = connection.call(ctx, "", "Browser.setWindowBounds", map[string]any{"windowId": window.WindowID, "bounds": map[string]any{"width": 1920, "height": 1080}}, nil)
	}
	b.mu.Lock()
	b.session = session
	account := b.account
	b.mu.Unlock()
	if err := b.applyEnvironment(ctx, connection, session, account.CookieBrowserInfo); err != nil {
		b.options.Logger.Warn("Failed to apply browser environment; keeping browser defaults", "error", err)
	}
	if err := b.restoreCookies(ctx, connection, account); err != nil {
		return err
	}
	events := connection.subscribe(session, "Debugger.paused")
	defer events.Close()
	if err := connection.call(ctx, session, "Debugger.enable", map[string]any{}, nil); err != nil {
		return err
	}
	if err := connection.call(ctx, session, "Debugger.setBreakpointByUrl", map[string]any{"lineNumber": 0, "urlRegex": `^.*?main\.[a-fA-F0-9]+\.js$`}, nil); err != nil {
		return err
	}
	filters := b.analyticsFilters(ctx)
	filtersJSON, _ := json.Marshal(filters)
	script := "window.__dynamicAnalyticsBlockFilters = " + string(filtersJSON) + ";\n" + b.options.setupScript
	setupCtx, setupCancel := context.WithTimeout(ctx, b.options.setupTimeout)
	defer setupCancel()
	var navigation struct {
		ErrorText string `json:"errorText"`
	}
	if err := connection.call(setupCtx, session, "Page.navigate", map[string]any{"url": b.options.startURL}, &navigation); err != nil {
		return err
	}
	if navigation.ErrorText != "" {
		return fmt.Errorf("browser navigation failed: %s", navigation.ErrorText)
	}
	if _, err := events.next(setupCtx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return NewBrowserSetupError(browserTimeoutMessage)
		}
		return err
	}
	err = connection.evaluate(setupCtx, session, script, false, nil)
	resumeErr := connection.call(setupCtx, session, "Debugger.resume", map[string]any{}, nil)
	if err != nil {
		return err
	}
	if resumeErr != nil {
		return resumeErr
	}
	if err := browserWait(setupCtx, time.Second); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return NewBrowserSetupError(browserTimeoutMessage)
	}
	var ready bool
	if err := connection.evaluate(setupCtx, session, "window.__invokeGraphQLAPISetupPromise", true, &ready); err != nil || !ready {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if setupCtx.Err() != nil {
			return NewBrowserSetupError(browserTimeoutMessage)
		}
		return fmt.Errorf("browser setup promise did not resolve to true: %v", err)
	}
	_ = connection.call(ctx, session, "Debugger.disable", map[string]any{}, nil)
	// Following タブへの切り替え失敗は本家と同じくセットアップ失敗にはしない。
	if err := connection.evaluate(ctx, session, browserScripts["following"].Template, true, nil); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.options.Logger.Warn("Failed to switch to Following tab", "error", err)
	} else {
		if err := browserWait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.complete = true
	b.mu.Unlock()
	return nil
}

// Shutdown は専用プロセスと子プロセス、一時プロファイルを回収する。
func (b *BrowserBackend) Shutdown(ctx context.Context) error {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	b.shutdownLocked()
	return nil
}

// MarkForRestart は次の要求でセットアップをやり直せるよう現在のブラウザーを終了する。
func (b *BrowserBackend) MarkForRestart(ctx context.Context) error { return b.Shutdown(ctx) }

func (b *BrowserBackend) shutdownLocked() {
	cleanup, cancel := context.WithTimeout(context.Background(), b.options.shutdownTimeout)
	defer cancel()
	b.mu.Lock()
	connection, exited, kill, profile := b.cdp, b.exited, b.killTree, b.profile
	b.complete, b.cdp, b.session, b.process, b.exited, b.killTree, b.profile = false, nil, "", nil, nil, nil, ""
	b.mu.Unlock()
	if connection != nil {
		_ = connection.call(cleanup, "", "Browser.close", map[string]any{}, nil)
		connection.Close()
	}
	if exited != nil {
		select {
		case <-exited:
		case <-cleanup.Done():
		}
	}
	if kill != nil {
		kill()
	}
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(time.Second):
		}
	}
	if profile != "" {
		// このインスタンスが MkdirTemp で作成したディレクトリだけを削除する。
		var err error
		for range 10 {
			err = os.RemoveAll(profile)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			b.options.Logger.Warn("Failed to remove temporary browser profile", "error", err)
		}
	}
}

func (b *BrowserBackend) page() (*cdpConnection, string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cdp == nil || b.session == "" {
		return nil, "", fmt.Errorf("browser or page is not initialized")
	}
	return b.cdp, b.session, nil
}

func browserWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InvokeGraphQLAPI は本家と同じ JS 式をページ内で実行し、返された通信結果を復元する。
func (b *BrowserBackend) InvokeGraphQLAPI(ctx context.Context, endpoint string, variables, flags *OrderedMap) (*RawResponse, error) {
	connection, session, err := b.page()
	if err != nil {
		return nil, err
	}
	script, err := BuildInvokeGraphQLScript(endpoint, variables, flags)
	if err != nil {
		return nil, err
	}
	var response struct {
		ParsedResponse any               `json:"parsedResponse"`
		StatusCode     *int              `json:"statusCode"`
		ResponseText   *string           `json:"responseText"`
		Headers        map[string]string `json:"headers"`
		RequestError   string            `json:"requestError"`
	}
	var value json.RawMessage
	if err := connection.evaluate(ctx, session, script, true, &value); err != nil {
		return nil, err
	}
	if value = bytes.TrimSpace(value); len(value) == 0 || value[0] != '{' {
		return nil, fmt.Errorf("browser GraphQL response is not an object")
	}
	if err := json.Unmarshal(value, &response); err != nil {
		return nil, err
	}
	return &RawResponse{ParsedResponse: response.ParsedResponse, StatusCode: response.StatusCode,
		ResponseText: response.ResponseText, Headers: response.Headers, RequestError: response.RequestError}, nil
}
