package twitter

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Python と同じ webpack/GraphQL hook。Python アプリの起動やソース読み込みは不要。
// 元ファイル: server/static/zendriver_setup.js
//
//go:embed browser_setup.js
var browserSetupScript string

// BrowserBackendOptions はブラウザ依存と隔離テスト用の差し替え指定。
type BrowserBackendOptions struct {
	Launcher      BrowserLauncher
	Launch        BrowserLaunchOptions
	HomeURL       string
	SetupScript   string
	ScreenshotDir string
}

// BrowserBackend は Python/Zendriver を介さず Chromium CDP で既存 Web App を操作する。
// ブラウザは初回 API 呼び出しで起動し、一時 profile と所有プロセスを Shutdown で回収する。
type BrowserBackend struct {
	mu         sync.Mutex
	account    *Account
	options    BrowserBackendOptions
	browser    BrowserSession
	generation uint64
	ready      atomic.Bool
	operations chan struct{}
}

// NewBrowserBackend は既定の Go CDP backend を生成する。
func NewBrowserBackend(account *Account) Backend {
	return NewBrowserBackendWithOptions(account, BrowserBackendOptions{})
}

// NewBrowserBackendWithOptions は隔離された browser session を差し込める backend を生成する。
func NewBrowserBackendWithOptions(account *Account, options BrowserBackendOptions) *BrowserBackend {
	if options.Launcher == nil {
		options.Launcher = launchBrowser
	}
	if options.HomeURL == "" {
		options.HomeURL = "https://x.com/"
	}
	if options.SetupScript == "" {
		options.SetupScript = browserSetupScript
	}
	return &BrowserBackend{account: account, options: options, operations: make(chan struct{}, 1)}
}

func (b *BrowserBackend) SetAccount(account *Account) {
	b.mu.Lock()
	b.account = account
	b.mu.Unlock()
}
func (b *BrowserBackend) IsSetupComplete() bool { return b.ready.Load() && b.IsBrowserProcessAlive() }
func (b *BrowserBackend) IsBrowserProcessAlive() bool {
	b.mu.Lock()
	browser := b.browser
	b.mu.Unlock()
	if browser == nil {
		return false
	}
	select {
	case <-browser.Done():
		return false
	default:
		return true
	}
}

func (b *BrowserBackend) acquire(ctx context.Context) error {
	select {
	case b.operations <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *BrowserBackend) release() { <-b.operations }

// Setup は保存 Cookie と環境情報を専用プロファイルへ設定し、main.js の実行直前に hook を注入する。
func (b *BrowserBackend) Setup(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()
	if b.IsSetupComplete() {
		return nil
	}
	b.mu.Lock()
	account, generation := b.account, b.generation
	old := b.browser
	b.browser = nil
	b.mu.Unlock()
	if old != nil {
		_ = old.Close(ctx)
	}
	if account == nil {
		return errors.New("twitter account is missing")
	}
	cookiesText := account.PlainCookies
	if account.LoadCookies != nil {
		var err error
		cookiesText, err = account.LoadCookies()
		if err != nil {
			return NewBrowserSetupError("保存された Twitter Cookie を復号できませんでした。アカウントを再連携してください。")
		}
	}
	cookies, err := ParseNetscapeCookieFile(cookiesText)
	if err != nil {
		return fmt.Errorf("parse browser cookies: %w", err)
	}
	browser, err := b.options.Launcher(ctx, b.options.Launch)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.generation != generation {
		b.mu.Unlock()
		_ = browser.Close(ctx)
		return context.Canceled
	}
	b.browser = browser
	b.mu.Unlock()
	success := false
	defer func() {
		if !success {
			b.ready.Store(false)
			closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			_ = browser.Close(closeCtx)
			stop()
		}
	}()
	if err := applyBrowserEnvironment(ctx, browser, account.CookieBrowserInfo); err != nil {
		return err
	}
	parameters := make([]map[string]any, 0, len(cookies))
	for _, cookie := range cookies {
		value := map[string]any{"name": cookie.Name, "value": cookie.Value, "domain": cookie.Domain, "path": cookie.Path, "secure": cookie.Secure}
		if cookie.Expires != nil {
			value["expires"] = *cookie.Expires
		}
		parameters = append(parameters, value)
	}
	if len(parameters) > 0 {
		if err := browser.Call(ctx, "Network.setCookies", map[string]any{"cookies": parameters}, nil); err != nil {
			return err
		}
	}
	events, unsubscribe := browser.Subscribe("Debugger.paused")
	defer unsubscribe()
	if err := browser.Call(ctx, "Debugger.enable", nil, nil); err != nil {
		return err
	}
	var breakpoint struct {
		ID string `json:"breakpointId"`
	}
	if err := browser.Call(ctx, "Debugger.setBreakpointByUrl", map[string]any{"lineNumber": 0, "urlRegex": `^.*?main\.[a-fA-F0-9]+\.js$`}, &breakpoint); err != nil {
		return err
	}
	if err := browser.Call(ctx, "Page.navigate", map[string]any{"url": b.options.HomeURL}, nil); err != nil {
		return err
	}
	setupCtx, stopSetup := context.WithTimeout(ctx, 15*time.Second)
	defer stopSetup()
	for {
		select {
		case <-setupCtx.Done():
			return NewBrowserSetupError("ヘッドレスブラウザのセットアップがタイムアウトしました。Cookie の期限切れやアカウントの状態を確認してください。")
		case <-browser.Done():
			return ErrBrowserConnectionLost
		case event := <-events:
			var paused struct {
				Hit []string `json:"hitBreakpoints"`
			}
			if err := json.Unmarshal(event.Params, &paused); err != nil {
				return err
			}
			matched := false
			for _, id := range paused.Hit {
				if id == breakpoint.ID {
					matched = true
				}
			}
			if !matched {
				if err := browser.Call(setupCtx, "Debugger.resume", nil, nil); err != nil {
					return err
				}
				continue
			}
			// 停止中に hook を設置してから resume。Promise を停止中に await しない。
			_, injectErr := browserEvaluate(setupCtx, browser, "window.__dynamicAnalyticsBlockFilters = [];\n"+b.options.SetupScript, false)
			resumeErr := browser.Call(setupCtx, "Debugger.resume", nil, nil)
			if injectErr != nil {
				return injectErr
			}
			if resumeErr != nil {
				return resumeErr
			}
			value, err := browserEvaluate(setupCtx, browser, `(async () => { while (!window.__invokeGraphQLAPISetupPromise) await new Promise(r => setTimeout(r, 25)); return await window.__invokeGraphQLAPISetupPromise; })()`, true)
			if err != nil || string(value) != "true" {
				return NewBrowserSetupError("Twitter Web App のセットアップを完了できませんでした。Cookie とブラウザの状態を確認してください。")
			}
			if err := browser.Call(setupCtx, "Debugger.disable", nil, nil); err != nil {
				return err
			}
			b.mu.Lock()
			if b.generation != generation {
				b.mu.Unlock()
				return context.Canceled
			}
			b.ready.Store(true)
			b.mu.Unlock()
			success = true
			return nil
		}
	}
}

func browserEvaluate(ctx context.Context, browser BrowserSession, script string, await bool) (json.RawMessage, error) {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := browser.Call(ctx, "Runtime.evaluate", map[string]any{"expression": script, "awaitPromise": await, "returnByValue": true, "userGesture": true}, &result)
	if err != nil {
		return nil, err
	}
	if len(result.Exception) > 0 && string(result.Exception) != "null" {
		return nil, errors.New("browser JavaScript evaluation failed")
	}
	return result.Result.Value, nil
}

func (b *BrowserBackend) activeBrowser() (BrowserSession, error) {
	b.mu.Lock()
	browser := b.browser
	b.mu.Unlock()
	if !b.ready.Load() || browser == nil {
		return nil, ErrBrowserConnectionLost
	}
	select {
	case <-browser.Done():
		return nil, ErrBrowserConnectionLost
	default:
		return browser, nil
	}
}

// InvokeGraphQLAPI は main.js の内部クライアントを使い、ブラウザ側のヘッダー生成を保つ。
func (b *BrowserBackend) InvokeGraphQLAPI(ctx context.Context, endpoint string, variables, flags *OrderedMap) (*RawResponse, error) {
	if err := b.acquire(ctx); err != nil {
		return nil, err
	}
	defer b.release()
	browser, err := b.activeBrowser()
	if err != nil {
		return nil, err
	}
	script, err := BuildInvokeGraphQLScript(endpoint, variables, flags)
	if err != nil {
		return nil, err
	}
	data, err := browserEvaluate(ctx, browser, script, true)
	if err != nil {
		return nil, err
	}
	return decodeBrowserResponse(data)
}

func decodeBrowserResponse(data []byte) (*RawResponse, error) {
	var raw struct {
		Parsed  json.RawMessage   `json:"parsedResponse"`
		Status  *int              `json:"statusCode"`
		Text    *string           `json:"responseText"`
		Headers map[string]string `json:"headers"`
		Error   string            `json:"requestError"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("invalid browser GraphQL response")
	}
	response := &RawResponse{StatusCode: raw.Status, ResponseText: raw.Text, Headers: raw.Headers, RequestError: raw.Error}
	if len(raw.Parsed) > 0 && string(raw.Parsed) != "null" {
		var value any
		if err := json.Unmarshal(raw.Parsed, &value); err != nil {
			return nil, err
		}
		response.ParsedResponse = value
	}
	return response, nil
}

// Shutdown は処理中の接続も閉じ、所有した browser だけを終了する。
func (b *BrowserBackend) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	browser := b.browser
	b.browser = nil
	b.generation++
	b.ready.Store(false)
	b.mu.Unlock()
	if browser != nil {
		return browser.Close(ctx)
	}
	return nil
}
func (b *BrowserBackend) MarkForRestart(ctx context.Context) error { return b.Shutdown(ctx) }

// SaveCookiesToNetscapeFormat は Chromium の Cookie を Python と共有可能な形式で返す。
func (b *BrowserBackend) SaveCookiesToNetscapeFormat(ctx context.Context) (string, error) {
	if err := b.acquire(ctx); err != nil {
		return "", err
	}
	defer b.release()
	browser, err := b.activeBrowser()
	if err != nil {
		return "", err
	}
	var result struct {
		Cookies []struct {
			Domain, Path, Name, Value string
			Secure                    bool
			Expires                   float64
			Session                   bool
		} `json:"cookies"`
	}
	if err := browser.Call(ctx, "Network.getAllCookies", nil, &result); err != nil {
		return "", err
	}
	var output strings.Builder
	output.WriteString("# Netscape HTTP Cookie File\n")
	for _, cookie := range result.Cookies {
		domain := strings.TrimPrefix(cookie.Domain, ".")
		if domain != "x.com" && !strings.HasSuffix(domain, ".x.com") && domain != "twitter.com" && !strings.HasSuffix(domain, ".twitter.com") {
			continue
		}
		secure, subdomain := "FALSE", "FALSE"
		if cookie.Secure {
			secure = "TRUE"
		}
		if strings.HasPrefix(cookie.Domain, ".") {
			subdomain = "TRUE"
		}
		expires := int64(cookie.Expires)
		if cookie.Session || expires < 0 {
			expires = 0
		}
		fmt.Fprintf(&output, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", cookie.Domain, subdomain, cookie.Path, secure, expires, cookie.Name, cookie.Value)
	}
	return output.String(), nil
}

// CaptureDebugScreenshot は明示設定された private ディレクトリだけに保存する。
func (b *BrowserBackend) CaptureDebugScreenshot(ctx context.Context, reason string) (string, error) {
	if b.options.ScreenshotDir == "" {
		return "", nil
	}
	if err := b.acquire(ctx); err != nil {
		return "", err
	}
	defer b.release()
	browser, err := b.activeBrowser()
	if err != nil {
		return "", err
	}
	return saveBrowserScreenshot(ctx, browser, filepath.Clean(b.options.ScreenshotDir))
}
