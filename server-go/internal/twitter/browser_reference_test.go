package twitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

func TestPinnedBrowserReference(t *testing.T) {
	data, err := os.ReadFile("testdata/browser_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SourceSHA256 string `json:"source_sha256"`
		SetupSHA256  string `json:"setup_script_sha256"`
		Filters      []struct {
			Input    []string          `json:"input"`
			Expected []analyticsFilter `json:"expected"`
			Error    bool              `json:"error"`
		} `json:"filters"`
		UserAgents []struct {
			Agent    string  `json:"user_agent"`
			Platform string  `json:"platform"`
			Expected *string `json:"expected"`
		} `json:"user_agents"`
		Languages []struct {
			Input    any     `json:"input"`
			Expected *string `json:"expected"`
		} `json:"languages"`
		Cookies []struct {
			Input    json.RawMessage `json:"input"`
			Expected string          `json:"expected"`
		} `json:"cookies"`
		Scripts []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Expected  string          `json:"expected"`
		} `json:"scripts"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SourceSHA256 != "23403a0738544acea02ce2264b8396752d325cc9939e7b295678a1cfe2c56095" || len(fixture.Filters) != 591 || len(fixture.Scripts) != 9 {
		t.Fatal("pinned source fixture is incomplete")
	}
	setupHash := sha256.Sum256([]byte(strings.ReplaceAll(twitterSetupScript, "\r\n", "\n")))
	if hex.EncodeToString(setupHash[:]) != fixture.SetupSHA256 {
		t.Fatal("embedded setup script differs from pinned upstream")
	}
	for index, item := range fixture.Filters {
		got, err := extractAnalyticsFilters(item.Input)
		if (err != nil) != item.Error || (!item.Error && !reflect.DeepEqual(got, item.Expected)) {
			t.Errorf("source filter case %d: got=%+v err=%v expected=%+v", index, got, err, item.Expected)
		}
	}
	for index, item := range fixture.UserAgents {
		got, ok := buildBrowserUserAgent(item.Agent, item.Platform)
		if ok != (item.Expected != nil) || (ok && got != *item.Expected) {
			t.Errorf("source user agent case %d: %q %v", index, got, ok)
		}
	}
	for index, item := range fixture.Languages {
		got, ok := buildBrowserAcceptLanguage(item.Input)
		if ok != (item.Expected != nil) || (ok && got != *item.Expected) {
			t.Errorf("source language case %d: %q %v", index, got, ok)
		}
	}
	for index, item := range fixture.Cookies {
		connection := newCDPTestPeer(t, func(ws *websocket.Conn, request cdpMessage) {
			replyCDP(ws, request, map[string]any{"cookies": item.Input})
		})
		backend := NewBrowserBackend(&Account{}, BrowserOptions{DataDir: t.TempDir()}).(*BrowserBackend)
		backend.cdp, backend.session = connection, "page"
		got, err := backend.SaveCookiesToNetscapeFormat(context.Background())
		connection.Close()
		if err != nil || got != item.Expected {
			t.Errorf("source cookie case %d: %q %v expected=%q", index, got, err, item.Expected)
		}
	}
	for _, item := range fixture.Scripts {
		arguments, ok := parseOrdered(t, item.Arguments).([]any)
		if !ok {
			t.Fatal("script arguments are not an array")
		}
		got, err := renderBrowserScript(item.Name, arguments...)
		if err != nil || got != item.Expected {
			t.Errorf("source browser script %s differs: %v", item.Name, err)
		}
	}
}
