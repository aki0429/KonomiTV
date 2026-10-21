package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInstallerIPTVDisabledBlocksDirectRoutesButKeepsTVAndRecordings(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.IPTV.InstallationDisabled = true
	s.config.IPTV.Enabled = false
	for _, path := range []string{"/api/iptv/channels", "/api/iptv/proxy?url=http://127.0.0.1/", "/iptv/", "/api/streams/live/iptv123/720p/mpegts", "/api/channels/IPTV-123/logo"} {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != 404 {
			t.Fatalf("%s returned %d", path, r.Code)
		}
	}
	for _, path := range []string{"/api/channels", "/api/videos", "/api/streams/live", "/tv/", "/videos/"} {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != 200 {
			t.Fatalf("ordinary feature %s returned %d: %s", path, r.Code, r.Body.String())
		}
	}
}
