package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/twitter"
)

func TestTwitterBrowserFactoryAndCookieLoading(t *testing.T) {
	server, _, _ := newTwitterTestServer(t)
	newTwitterBackendFactory = nil
	plain := ".x.com\tTRUE\t/\tTRUE\t0\tauth_token\tlocal-test-only"
	encrypted, err := server.encryptTwitterCookie(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		secret string
		valid  bool
	}{{plain, true}, {encrypted, true}, {"enc:invalid-token", false}} {
		account := server.twitterAccountToModel(twitterAccountRecord{AccessTokenSecret: value.secret})
		got, err := account.LoadCookies()
		if value.valid && (err != nil || got != plain) {
			t.Fatalf("stored cookie was not restored: %v", err)
		}
		if !value.valid && err == nil {
			t.Fatal("invalid encrypted cookie was accepted")
		}
		backend := server.twitterBackendFactory()(account)
		if _, ok := backend.(*twitter.BrowserBackend); !ok {
			t.Fatalf("production factory returned %T", backend)
		}
		if backend.IsSetupComplete() || backend.IsBrowserProcessAlive() {
			t.Fatal("constructing the backend started a browser")
		}
	}
}

type authIsolationBackend struct {
	*fakeTwitterBackend
	response *twitter.RawResponse
	started  chan<- int64
	release  <-chan struct{}
	id       int64
	stopped  atomic.Int32
}

func (b *authIsolationBackend) InvokeGraphQLAPI(context.Context, string, *twitter.OrderedMap, *twitter.OrderedMap) (*twitter.RawResponse, error) {
	b.started <- b.id
	<-b.release
	return b.response, nil
}

func (b *authIsolationBackend) Shutdown(context.Context) error { b.stopped.Add(1); return nil }

func TestTwitterBrowserConcurrentCookieAuthentication(t *testing.T) {
	server, handler, token := newTwitterTestServer(t)
	// この fixture は read/write 共用DBのため、本番の単一 writer と同じく接続を直列化する。
	server.writeDB.SetMaxOpenConns(1)
	if _, err := server.encryptTwitterCookie("initialize local key"); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan int64, 2), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend {
		name := "fixture_A"
		if strings.HasSuffix(account.PlainCookies, "cookie_B") {
			name = "fixture_B"
		}
		response := fmt.Sprintf(`{"data":{"viewer":{"user_results":{"result":{"rest_id":"100","core":{"name":"%s","screen_name":"%s"},"avatar":{"image_url":"https://example.com/local.png"}}}}}}`, name, name)
		return &authIsolationBackend{fakeTwitterBackend: newFakeTwitterBackend(), response: rawResponseJSON(response, 200), started: started, release: release, id: account.ID}
	}
	done := make(chan *httptest.ResponseRecorder, 2)
	for _, cookie := range []string{"cookie_A", "cookie_B"} {
		go func() {
			body, _ := json.Marshal(map[string]any{"cookies_txt": ".x.com\tTRUE\t/\tTRUE\t0\tauth_token\t" + cookie})
			request := httptest.NewRequest(http.MethodPost, "/api/twitter/auth", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			done <- recorder
		}()
	}
	ids := map[int64]bool{}
	for range 2 {
		select {
		case id := <-started:
			if id >= 0 || ids[id] {
				t.Fatal("authentication requests shared a temporary account")
			}
			ids[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("two independent authentication backends were not started")
		}
	}
	once.Do(func() { close(release) })
	for range 2 {
		result := <-done
		if result.Code != http.StatusNoContent {
			t.Fatalf("cookie authentication: %d %s", result.Code, result.Body.String())
		}
	}
	for id := range ids {
		if _, exists := twitter.GetInstanceIfExists(id); exists {
			t.Fatal("temporary browser remained registered")
		}
	}
	rows, err := server.db.Query("SELECT screen_name, access_token_secret FROM twitter_accounts ORDER BY screen_name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, encrypted string
		if err := rows.Scan(&name, &encrypted); err != nil {
			t.Fatal(err)
		}
		plain, err := server.decryptTwitterCookieForTest(encrypted)
		if err != nil || !strings.HasSuffix(plain, "cookie_"+strings.TrimPrefix(name, "fixture_")) {
			t.Fatal("an account was saved with another request's cookies")
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 2 {
		t.Fatalf("stored accounts: %d %v", count, err)
	}
}

func TestTwitterBrowserFailedAuthenticationReleasesTemporaryInstance(t *testing.T) {
	_, handler, token := newTwitterTestServer(t)
	started, release := make(chan int64, 1), make(chan struct{})
	close(release)
	backend := &authIsolationBackend{fakeTwitterBackend: newFakeTwitterBackend(), response: rawResponseJSON(`{"errors":[{"code":32,"message":"local fixture authentication failed"}]}`, 403), started: started, release: release}
	newTwitterBackendFactory = func(account *twitter.Account) twitter.Backend { backend.id = account.ID; return backend }
	request := httptest.NewRequest(http.MethodPost, "/api/twitter/auth", strings.NewReader(`{"cookies_txt":".x.com\tTRUE\t/\tTRUE\t0\tauth_token\tlocal"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || backend.stopped.Load() != 1 {
		t.Fatalf("failed authentication cleanup: %d stops=%d", recorder.Code, backend.stopped.Load())
	}
	if _, exists := twitter.GetInstanceIfExists(backend.id); exists {
		t.Fatal("failed authentication retained its temporary instance")
	}
}
