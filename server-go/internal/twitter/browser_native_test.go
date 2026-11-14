package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeBrowserRequest struct {
	endpoint string
	body     map[string]any
}

func TestNativeTwitterBrowser(t *testing.T) {
	if os.Getenv("KONOMITV_TWITTER_BROWSER_TEST") != "1" {
		t.Skip("set KONOMITV_TWITTER_BROWSER_TEST=1 for isolated real browser verification")
	}
	var mu sync.Mutex
	var requests []nativeBrowserRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			mu.Lock()
			requests = append(requests, nativeBrowserRequest{r.URL.Path, body})
			mu.Unlock()
			if strings.HasPrefix(r.URL.Path, "/upload.x.com/") && r.URL.Query().Get("mode") == "upload-fail" {
				http.Error(w, "fixture upload failed", 500)
				return
			}
			if r.URL.Path == "/graphql/CreateTweet" && r.URL.Query().Get("mode") == "non-json" {
				_, _ = w.Write([]byte("<html>local invalid response</html>"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoint": r.URL.Path, "request": body}})
			return
		}
		file := "index.html"
		t.Logf("local fixture GET %s", r.URL.Path)
		if r.URL.Path == "/main.123abc.js" {
			file = "main.js"
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		data, err := os.ReadFile(filepath.Join("testdata", "browser", file))
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	account := &Account{ID: 123, ScreenName: "local_fixture", PlainCookies: ".x.com\tTRUE\t/\tTRUE\t0\tauth_token\tfake-local-cookie\nlocalhost\tFALSE\t/\tFALSE\t0\tonly_local\tvalue"}
	backend := NewBrowserBackend(account, BrowserOptions{BinaryPath: os.Getenv("KONOMITV_TWITTER_BROWSER_BINARY"), DataDir: t.TempDir(), startURL: server.URL, filterURLs: []string{},
		additionalArgs: []string{"--disable-background-networking", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1"}}).(*BrowserBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer backend.Shutdown(context.Background())
	if err := backend.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if !backend.IsSetupComplete() || !backend.IsBrowserProcessAlive() {
		t.Fatal("browser did not become ready")
	}
	pid, profile := backend.process.Process.Pid, backend.profile
	if err := backend.Setup(ctx); err != nil || backend.process.Process.Pid != pid {
		t.Fatalf("repeated setup did not reuse the browser: %v", err)
	}
	raw, err := backend.InvokeGraphQLAPI(ctx, "Viewer", NewOrderedMap().Set("text", "日本語\n<local>"), nil)
	if err != nil || raw == nil || raw.StatusCode == nil || *raw.StatusCode != 200 || raw.RequestError != "" || raw.ParsedResponse == nil {
		t.Fatalf("actual browser GraphQL invocation: %#v, %v", raw, err)
	}
	cookies, err := backend.SaveCookiesToNetscapeFormat(ctx)
	if err != nil || !strings.Contains(cookies, "auth_token\tfake-local-cookie") || strings.Contains(cookies, "only_local") {
		t.Fatalf("cookie export failed: %v", err)
	}
	var environment struct {
		Language string `json:"language"`
		Timezone string `json:"timezone"`
	}
	if err := backend.cdp.evaluate(ctx, backend.session, "({language:navigator.language, timezone:Intl.DateTimeFormat().resolvedOptions().timeZone})", false, &environment); err != nil || environment.Language != "ja" || environment.Timezone != "Asia/Tokyo" {
		t.Fatalf("environment override: %+v %v", environment, err)
	}
	path, err := backend.CaptureDebugScreenshot(ctx, "native <> / screenshot")
	if err != nil || path == "" {
		t.Fatalf("screenshot: %q %v", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	size, _, err := image.DecodeConfig(file)
	_ = file.Close()
	if err != nil || size.Width < 100 || size.Height < 100 {
		t.Fatalf("invalid screenshot: %+v %v", size, err)
	}
	if output := os.Getenv("KONOMITV_TWITTER_BROWSER_ARTIFACT_DIR"); output != "" {
		if err := os.MkdirAll(output, 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if err := os.WriteFile(filepath.Join(output, "native-browser.png"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aWQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	images := []UploadedImage{{Filename: "検証用.png", ContentType: "image/png", Data: png}}
	result, err := backend.PostTweetViaComposeUI(ctx, ComposeRequest{TweetText: "ローカル専用\n日本語", Images: images})
	if err != nil || result == nil || !result.IsSuccess || result.ComposeSubmittedAt == nil || result.GraphQLAPIResult == nil {
		t.Fatalf("local text/image compose: %+v %v", result, err)
	}
	mu.Lock()
	var uploaded, posted bool
	for _, request := range requests {
		if request.endpoint == "/upload.x.com/FINALIZE" {
			uploaded = request.body["base64"] == base64.StdEncoding.EncodeToString(png) && request.body["name"] == "検証用.png"
		}
		if request.endpoint == "/graphql/CreateTweet" {
			variables, _ := request.body["variables"].(map[string]any)
			posted = variables["text"] == "ローカル専用\n日本語"
		}
	}
	mu.Unlock()
	if !uploaded || !posted {
		t.Fatal("local receiver did not receive the actual text and image bytes")
	}
	id := "123450"
	result, err = backend.PostTweetViaComposeUI(ctx, ComposeRequest{TweetText: "ローカル返信", InReplyToStatusID: &id})
	if err != nil || result == nil || !result.IsSuccess {
		t.Fatalf("local reply compose: %+v %v", result, err)
	}
	mu.Lock()
	last := requests[len(requests)-1]
	mu.Unlock()
	if last.body["reply"] != id {
		t.Fatal("reply target was not passed through history state")
	}
	if err := backend.cdp.evaluate(ctx, backend.session, "window.fixtureMode='upload-fail'", false, nil); err != nil {
		t.Fatal(err)
	}
	result, err = backend.PostTweetViaComposeUI(ctx, ComposeRequest{TweetText: "upload failure", Images: images})
	if err != nil || result == nil || result.IsSuccess || result.ErrorMessage == nil || *result.ErrorMessage != "画像のアップロードに失敗しました。" {
		t.Fatalf("upload failure: %+v %v", result, err)
	}
	if err := backend.cdp.evaluate(ctx, backend.session, "window.fixtureMode='non-json'", false, nil); err != nil {
		t.Fatal(err)
	}
	result, err = backend.PostTweetViaComposeUI(ctx, ComposeRequest{TweetText: "invalid local response"})
	if err != nil || result == nil || result.IsSuccess || result.ComposeSubmittedAt == nil {
		t.Fatalf("invalid response: %+v %v", result, err)
	}
	backend.cdp.Close()
	if _, err := backend.InvokeGraphQLAPI(ctx, "Viewer", NewOrderedMap(), nil); !errors.Is(err, ErrBrowserConnectionLost) {
		t.Fatalf("connection loss was not classified: %v", err)
	}
	if err := backend.MarkForRestart(ctx); err != nil || backend.IsSetupComplete() || backend.IsBrowserProcessAlive() {
		t.Fatalf("restart cleanup: %v", err)
	}
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary profile leaked: %v", err)
	}
	if err := backend.Setup(ctx); err != nil {
		t.Fatalf("setup after connection loss: %v", err)
	}
	profile, exited := backend.profile, backend.exited
	if err := backend.process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("browser parent did not exit")
	}
	if backend.IsBrowserProcessAlive() {
		t.Fatal("dead browser was reported alive")
	}
	_ = backend.Shutdown(ctx)
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile remained after parent death: %v", err)
	}
	t.Logf("actual browser setup, pinned JS, GraphQL, cookies, screenshot %dx%d, text/image/reply, error cleanup and process shutdown passed", size.Width, size.Height)
}

func TestNativeTwitterBrowserSetupCleanup(t *testing.T) {
	if os.Getenv("KONOMITV_TWITTER_BROWSER_TEST") != "1" {
		t.Skip("set KONOMITV_TWITTER_BROWSER_TEST=1 for isolated real browser verification")
	}
	for _, action := range []string{"timeout", "cancel"} {
		t.Run(action, func(t *testing.T) {
			navigated := make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(navigated) })
				_, _ = w.Write([]byte("<html><body>No main.js in this fixture</body></html>"))
			}))
			defer server.Close()
			backend := NewBrowserBackend(&Account{}, BrowserOptions{BinaryPath: os.Getenv("KONOMITV_TWITTER_BROWSER_BINARY"), DataDir: t.TempDir(),
				startURL: server.URL, filterURLs: []string{}, setupTimeout: 500 * time.Millisecond,
				additionalArgs: []string{"--disable-background-networking", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1"}}).(*BrowserBackend)
			defer backend.Shutdown(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- backend.Setup(ctx) }()
			select {
			case <-navigated:
			case err := <-done:
				t.Fatalf("browser did not reach the local fixture: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			backend.mu.RLock()
			profile, exited := backend.profile, backend.exited
			backend.mu.RUnlock()
			if action == "cancel" {
				cancel()
			}
			err := <-done
			if action == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled setup: %v", err)
				}
			} else if message, ok := AsBrowserSetupMessage(err); !ok || message != browserTimeoutMessage {
				t.Fatalf("timed out setup: %v", err)
			}
			if backend.IsSetupComplete() || backend.IsBrowserProcessAlive() {
				t.Fatal("failed setup retained an active browser")
			}
			select {
			case <-exited:
			default:
				t.Fatal("failed setup left the browser parent running")
			}
			if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed setup retained its profile: %v", err)
			}
		})
	}
}

func TestBrowserShutdownAllInstances(t *testing.T) {
	ResetInstances()
	defer ResetInstances()
	backends := []*fakeBackend{{isSetupComplete: true}, {isSetupComplete: true}}
	for index, backend := range backends {
		GetInstance(&Account{ID: int64(index + 1)}, func(*Account) Backend { return backend }, nil)
	}
	ShutdownAllBrowsers(context.Background())
	for _, backend := range backends {
		if backend.shutdownCalls != 1 || backend.isSetupComplete {
			t.Fatal("an account browser was not shut down")
		}
	}
	if _, exists := GetInstanceIfExists(1); exists {
		t.Fatal("shut down browser remained registered")
	}
}

func TestBrowserMissingBinary(t *testing.T) {
	b := NewBrowserBackend(&Account{}, BrowserOptions{BinaryPath: filepath.Join(t.TempDir(), "missing-browser")})
	err := b.Setup(context.Background())
	if message, ok := AsBrowserSetupMessage(err); !ok || message != browserMissingMessage {
		t.Fatalf("missing browser error: %v", err)
	}
}
