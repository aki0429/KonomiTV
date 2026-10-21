package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockBrowser struct {
	mu            sync.Mutex
	calls         []string
	scripts       []string
	done          chan struct{}
	events        chan BrowserEvent
	closeOnce     sync.Once
	fail          string
	posts         int
	subscriptions int
}

func newMockBrowser() *mockBrowser {
	return &mockBrowser{done: make(chan struct{}), events: make(chan BrowserEvent, 32)}
}
func (m *mockBrowser) Done() <-chan struct{} { return m.done }
func (m *mockBrowser) Close(context.Context) error {
	m.closeOnce.Do(func() { close(m.done) })
	return nil
}
func (m *mockBrowser) Subscribe(...string) (<-chan BrowserEvent, func()) {
	m.subscriptions++
	return m.events, func() { m.subscriptions-- }
}
func (m *mockBrowser) emit(method string, params any) {
	data, _ := json.Marshal(params)
	m.events <- BrowserEvent{method, data}
}
func (m *mockBrowser) Call(ctx context.Context, method string, params, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, method)
	if method == m.fail {
		return errors.New("synthetic failure")
	}
	result := any(map[string]any{})
	switch method {
	case "Browser.getVersion":
		result = map[string]any{"userAgent": "Mozilla/5.0 (Windows NT 10.0) HeadlessChrome/123.0.0.0"}
	case "Debugger.setBreakpointByUrl":
		result = map[string]any{"breakpointId": "main"}
	case "Page.navigate":
		m.emit("Debugger.paused", map[string]any{"hitBreakpoints": []string{"other"}})
		m.emit("Debugger.paused", map[string]any{"hitBreakpoints": []string{"main"}})
	case "Runtime.evaluate":
		expression := params.(map[string]any)["expression"].(string)
		m.scripts = append(m.scripts, expression)
		value := any(true)
		if strings.Contains(expression, "const requestPayload") {
			value = map[string]any{"statusCode": 200, "parsedResponse": map[string]any{"data": map[string]any{"viewer": "synthetic"}}, "headers": map[string]string{"x-test": "ok"}}
		}
		if strings.Contains(expression, "const images=") {
			m.emit("Network.responseReceived", map[string]any{"requestId": "preflight", "type": "Preflight", "response": map[string]any{"url": "https://upload.x.com/i/media/upload.json?command=FINALIZE", "status": 200}})
			m.emit("Network.responseReceived", map[string]any{"requestId": "upload", "type": "XHR", "response": map[string]any{"url": "https://upload.x.com/i/media/upload.json?command=FINALIZE", "status": 200}})
		}
		result = map[string]any{"result": map[string]any{"value": value}}
	case "Input.dispatchKeyEvent":
		input := params.(map[string]any)
		if input["key"] == "Enter" && input["type"] == "keyDown" {
			m.posts++
			m.emit("Network.responseReceived", map[string]any{"requestId": "tweet", "type": "XHR", "response": map[string]any{"url": "https://x.com/i/api/graphql/hash/CreateTweet", "status": 200, "headers": map[string]string{"Content-Type": "application/json"}}})
			m.emit("Network.loadingFinished", map[string]any{"requestId": "tweet"})
		}
	case "Network.getResponseBody":
		result = map[string]any{"body": base64.StdEncoding.EncodeToString([]byte(`{"data":{"create_tweet":{"id":"123"}}}`)), "base64Encoded": true}
	case "Network.getAllCookies":
		result = map[string]any{"cookies": []map[string]any{{"domain": ".x.com", "path": "/", "name": "synthetic", "value": "test", "secure": true, "session": true}, {"domain": "unrelated.test", "name": "ignored", "value": "ignored"}}}
	}
	if output != nil {
		data, _ := json.Marshal(result)
		return json.Unmarshal(data, output)
	}
	return nil
}
func mockBackend(m *mockBrowser) *BrowserBackend {
	return NewBrowserBackendWithOptions(&Account{PlainCookies: "# Netscape HTTP Cookie File\n"}, BrowserBackendOptions{Launcher: func(context.Context, BrowserLaunchOptions) (BrowserSession, error) { return m, nil }, HomeURL: "http://127.0.0.1/synthetic", SetupScript: "window.__invokeGraphQLAPISetupPromise=Promise.resolve(true);"})
}

func TestBrowserBackendLifecycleAndGraphQL(t *testing.T) {
	m := newMockBrowser()
	b := mockBackend(m)
	ctx := context.Background()
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.IsSetupComplete() || m.subscriptions != 0 {
		t.Fatal("setup lifecycle invalid")
	}
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	response, err := b.InvokeGraphQLAPI(ctx, "Viewer", NewOrderedMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == nil || *response.StatusCode != 200 || response.ParsedResponse.(map[string]any)["data"] == nil {
		t.Fatalf("bad response: %+v", response)
	}
	cookies, err := b.SaveCookiesToNetscapeFormat(ctx)
	if err != nil || !strings.Contains(cookies, ".x.com\tTRUE\t/\tTRUE\t0\tsynthetic\ttest") || strings.Contains(cookies, "unrelated") {
		t.Fatalf("cookies invalid %q %v", cookies, err)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if b.IsSetupComplete() || b.IsBrowserProcessAlive() {
		t.Fatal("browser leaked")
	}
	resumes := 0
	for _, call := range m.calls {
		if call == "Debugger.resume" {
			resumes++
		}
	}
	if resumes != 2 {
		t.Fatalf("unrelated pause was not resumed: %d", resumes)
	}
}
func TestBrowserBackendSetupFailureClosesProcess(t *testing.T) {
	m := newMockBrowser()
	m.fail = "Debugger.enable"
	b := mockBackend(m)
	if err := b.Setup(context.Background()); err == nil {
		t.Fatal("expected setup failure")
	}
	select {
	case <-m.done:
	default:
		t.Fatal("failed setup leaked browser")
	}
	if b.IsSetupComplete() || m.subscriptions != 0 {
		t.Fatal("failed state retained")
	}
}
func TestBrowserComposeMockTextReplyImageAndNoRetry(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "reply"}[reply], func(t *testing.T) {
			m := newMockBrowser()
			b := mockBackend(m)
			defer b.Shutdown(context.Background())
			if err := b.Setup(context.Background()); err != nil {
				t.Fatal(err)
			}
			request := ComposeRequest{TweetText: "synthetic ' text\n日本語", Images: []UploadedImage{{Filename: "synthetic.png", ContentType: "image/png", Data: []byte("fake-image")}}}
			if reply {
				id := "456"
				request.InReplyToStatusID = &id
			}
			result, err := b.PostTweetViaComposeUI(context.Background(), request)
			if err != nil || !result.IsSuccess || result.ComposeSubmittedAt == nil || result.GraphQLAPIResult == nil {
				t.Fatalf("compose failed: %+v %v", result, err)
			}
			if m.posts != 1 || m.subscriptions != 0 {
				t.Fatalf("unexpected posts/subscriptions %d/%d", m.posts, m.subscriptions)
			}
			if result.GraphQLAPIResult.ParsedResponse.(map[string]any)["data"] == nil {
				t.Fatal("base64 response not parsed")
			}
		})
	}
}
func TestBrowserComposeLostResultDoesNotResend(t *testing.T) {
	m := newMockBrowser()
	b := mockBackend(m)
	defer b.Shutdown(context.Background())
	if err := b.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.fail = "Network.getResponseBody"
	result, err := b.PostTweetViaComposeUI(context.Background(), ComposeRequest{TweetText: "synthetic"})
	if err != nil || result.IsSuccess || result.ComposeSubmittedAt == nil || m.posts != 1 || m.subscriptions != 0 {
		t.Fatalf("uncertain submission handling: %+v %v posts=%d", result, err, m.posts)
	}
}

// opt-in: about:blank のみ。認証・Cookie 設定・外部ページ遷移は行わない。
func TestBrowserChromiumIsolatedSmoke(t *testing.T) {
	if os.Getenv("KONOMITV_TEST_BROWSER") != "1" {
		t.Skip("isolated Chromium opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	browser, err := launchBrowser(ctx, BrowserLaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close(context.Background())
	value, err := browserEvaluate(ctx, browser, "JSON.stringify({url:location.href,value:6*7})", false)
	if err != nil || !strings.Contains(string(value), "about:blank") || !strings.Contains(string(value), "42") {
		t.Fatalf("CDP evaluation failed: %s %v", value, err)
	}
	if err := browser.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-browser.Done():
	case <-time.After(time.Second):
		t.Fatal("browser did not close")
	}
}

func TestBrowserChromiumLocalSetup(t *testing.T) {
	if os.Getenv("KONOMITV_TEST_BROWSER") != "1" {
		t.Skip("isolated Chromium opt-in")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/main.abcdef.js" {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte("window.syntheticMainRan=true;"))
			return
		}
		_, _ = w.Write([]byte(`<html><body>synthetic<script src="/main.abcdef.js"></script></body></html>`))
	}))
	defer page.Close()
	b := NewBrowserBackendWithOptions(&Account{}, BrowserBackendOptions{HomeURL: page.URL, SetupScript: `window.__invokeGraphQLAPISetupPromise=Promise.resolve(true); window.__invokeGraphQLAPI=async()=>({statusCode:200,parsedResponse:{data:{synthetic:true}}});`})
	defer b.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	response, err := b.InvokeGraphQLAPI(ctx, "Viewer", NewOrderedMap(), nil)
	if err != nil || response.StatusCode == nil || *response.StatusCode != 200 {
		t.Fatalf("local setup/GraphQL failed: %+v %v", response, err)
	}
}
