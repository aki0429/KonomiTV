package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

type logoFixtureClient struct {
	ini, index, image []byte
	requests          [][]string
	failed            bool
}

func (c *logoFixtureClient) FileCopy2(names []string) ([]reservations.FileData, bool) {
	c.requests = append(c.requests, names)
	if len(names) == 2 {
		return []reservations.FileData{{Data: c.ini}, {Data: c.index}}, true
	}
	return []reservations.FileData{{Data: c.image}}, !c.failed
}

// TestEDCBLogoFallback はHTTPで画質の優先順・BMP/PNG・ETag・転送失敗・同梱優先を通す。
func TestEDCBLogoFallback(t *testing.T) {
	old := edcbLogoClientFactory
	t.Cleanup(func() { edcbLogoClientFactory = old })
	for _, tc := range []struct {
		name, index, kind string
		failed, bundled   bool
	}{
		{"quality-priority", "date time size 8000_00A_001_02.png\ndate time size 8000_00A_000_05.png", "image/png", false, false},
		{"bitmap", "date time size 8000_00A_001_04.BMP", "image/bmp", false, false},
		{"missing", "date time size 8001_00A_001_05.png", "image/png", false, false},
		{"failed-transfer", "date time size 8000_00A_001_05.png\ndate time size 8000_00A_001_02.png", "image/png", true, false},
		{"bundled", "date time size 8000_00A_001_05.png", "image/png", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, paths := newTestServer(t, "")
			s.config.General.Backend = "EDCB"
			insertTestChannel(t, s.db, testChannel{ID: "NID32768-SID1024", DisplayChannelID: "gr101", NetworkID: 32768, ServiceID: 1024, Type: "GR", Name: "Unbundled", IsWatchable: true})
			writeFile(t, filepath.Join(paths.StaticDir, "logos", "default.png"), "default-logo")
			client := &logoFixtureClient{ini: []byte("\xef\xbb\xbf80000400=10\n"), index: []byte(tc.index), image: []byte("edcb-logo"), failed: tc.failed}
			edcbLogoClientFactory = func(context.Context, *config.Config) (logoFileClient, error) { return client, nil }
			if tc.bundled {
				writeFile(t, filepath.Join(paths.StaticDir, "logos", "NID32768-SID1024.png"), "bundled-logo")
			}
			response := doJSONRequest(t, s.Handler(), http.MethodGet, "/api/channels/gr101/logo", "", "", "")
			want := "edcb-logo"
			if tc.name == "missing" || tc.failed {
				want = "default-logo"
			}
			if tc.bundled {
				want = "bundled-logo"
				if len(client.requests) != 0 {
					t.Fatal("queried EDCB before bundled logo")
				}
			}
			if response.Code != 200 || response.Body.String() != want || response.Header().Get("Content-Type") != tc.kind {
				t.Fatalf("%d %s %s", response.Code, response.Body.String(), response.Header().Get("Content-Type"))
			}
			if tc.name == "quality-priority" && !reflect.DeepEqual(client.requests[1], []string{`LogoData\8000_00A_000_05.png`}) {
				t.Fatal(client.requests)
			}
			if tc.failed && len(client.requests) != 2 {
				t.Fatal("tried a lower quality after failed preferred transfer")
			}
			request := httptest.NewRequest(http.MethodGet, "/api/channels/gr101/logo", nil)
			request.Header.Set("If-None-Match", response.Header().Get("ETag"))
			recorder := httptest.NewRecorder()
			s.Handler().ServeHTTP(recorder, request)
			if recorder.Code != 304 || recorder.Body.Len() != 0 {
				t.Fatalf("conditional logo %d", recorder.Code)
			}
			edcbLogoClientFactory = func(context.Context, *config.Config) (logoFileClient, error) { return nil, errors.New("unavailable") }
			response = doJSONRequest(t, s.Handler(), http.MethodGet, "/api/channels/gr101/logo", "", "", "")
			if response.Code != 200 {
				t.Fatal(response.Code)
			}
		})
	}
}
