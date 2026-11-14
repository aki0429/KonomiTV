package twitter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrowserAnalyticsDownloadAndCache(t *testing.T) {
	var broken atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/list", http.StatusFound)
			return
		}
		if broken.Load() {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "short")
			return
		}
		_, _ = io.WriteString(w, "\ufeff||x.com/i/analytics$xhr\n||twitter.com/jot$3p,ping")
	}))
	defer server.Close()
	backend := NewBrowserBackend(&Account{}, BrowserOptions{DataDir: t.TempDir(), filterURLs: []string{server.URL + "/redirect", server.URL + "/list"}}).(*BrowserBackend)
	got := backend.analyticsFilters(context.Background())
	if len(got) != 2 {
		t.Fatalf("did not download and deduplicate complete lists: %+v", got)
	}
	cache := filepath.Join(backend.options.DataDir, "twitter-analytics-block-filters.json")
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	if restored := backend.analyticsFilters(context.Background()); !reflect.DeepEqual(restored, got) {
		t.Fatal("failed download did not restore the previous complete cache")
	}
	after, _ := os.ReadFile(cache)
	if string(before) != string(after) {
		t.Fatal("partial download overwrote the cache")
	}
	for _, malformed := range []string{`{`, `{"filters":null}`, `{"filters":[{"request_types":["XHR"]}]}`,
		`{"filters":[{"regex_pattern":null,"request_types":["XHR"]}]}`, `{"filters":[{"regex_pattern":"a","request_types":[]}]}`,
		`{"filters":[{"regex_pattern":"a","request_types":["Other"]}]}`} {
		if err := os.WriteFile(cache, []byte(malformed), 0600); err != nil {
			t.Fatal(err)
		}
		if got := backend.analyticsFilters(context.Background()); len(got) != 0 {
			t.Fatalf("malformed cache was used: %s", malformed)
		}
	}
}

func TestBrowserAnalyticsRejectsIncompleteLists(t *testing.T) {
	for _, kind := range []string{"empty", "too-large", "invalid-utf8", "status-error"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "too-large":
					_, _ = io.WriteString(w, strings.Repeat("x", 2*1024*1024+1))
				case "invalid-utf8":
					_, _ = w.Write([]byte{255, 254, 128})
				case "status-error":
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			if _, err := downloadAnalyticsFilters(context.Background(), []string{server.URL}); err == nil {
				t.Fatalf("incomplete list %s was accepted", kind)
			}
		})
	}
}
