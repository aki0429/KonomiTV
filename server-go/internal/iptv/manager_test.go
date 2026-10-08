package iptv

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
)

// testConfig はテスト用の設定を返す。
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.IPTV.Enabled = true
	cfg.IPTV.CacheTTL = 3600
	cfg.IPTV.RequestTimeout = 5.0
	cfg.IPTV.UserAgent = "TestAgent/1.0"
	return cfg
}

// testPlaylistA はテスト用のプレイリスト。
const testPlaylistA = `#EXTM3U
#EXTINF:-1 tvg-id="ChannelA.jp" group-title="News",Alpha Channel (720p)
https://example.com/alpha.m3u8
#EXTINF:-1,Shared Channel
https://example.com/shared.m3u8
#EXTINF:-1,Geo Channel [Geo-blocked]
https://example.com/geo.m3u8
`

// testPlaylistB はテスト用のプレイリスト (A と重複する URL を含む) 。
const testPlaylistB = `#EXTM3U
#EXTINF:-1,Shared Channel Duplicate
https://example.com/shared.m3u8
#EXTINF:-1 http-referrer="https://example.com/" http-user-agent="ChannelAgent/2.0" group-title="Music",Beta Channel
https://example.com/beta.m3u8
`

// testCountriesJSON はテスト用の国一覧 API のレスポンス。
const testCountriesJSON = `[
  {"code": "jp", "name": "Japan", "flag": "🇯🇵"},
  {"code": "US", "name": "United States", "flag": "🇺🇸"}
]`

// newTestManager は HTTP リクエストを差し替えた Manager を生成する。
func newTestManager(t *testing.T, cfg *config.Config, handler http.HandlerFunc) (*Manager, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager := New(Options{
		Config:       cfg,
		DataDir:      t.TempDir(),
		Logger:       slog.New(slog.DiscardHandler),
		HTTPClient:   server.Client(),
		CountriesURL: server.URL + "/api/countries.json",
	})
	return manager, server
}

// TestRefresh はプレイリストの取得・重複排除・並べ替え・ソースのエラー記録を検証する。
func TestRefresh(t *testing.T) {
	cfg := testConfig()
	// Refresh は複数ソースを並行取得するため、handler からの追記を直列化する。
	var userAgentsMu sync.Mutex
	var userAgents []string
	manager, server := newTestManager(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		userAgentsMu.Lock()
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		userAgentsMu.Unlock()
		switch r.URL.Path {
		case "/api/countries.json":
			_, _ = w.Write([]byte(testCountriesJSON))
		case "/a.m3u":
			_, _ = w.Write([]byte(testPlaylistA))
		case "/b.m3u":
			_, _ = w.Write([]byte(testPlaylistB))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	cfg.IPTV.Sources = []string{server.URL + "/a.m3u", server.URL + "/b.m3u", server.URL + "/missing.m3u"}

	channels := manager.Refresh(context.Background(), false)
	if len(channels) != 4 {
		t.Fatalf("channels = %d, want 4", len(channels))
	}

	// 国名 → グループ名 → チャンネル名の順で並ぶ (グループなしは最後)
	expectedNames := []string{"Alpha Channel (720p)", "Beta Channel", "Geo Channel [Geo-blocked]", "Shared Channel"}
	actualNames := make([]string, 0, len(channels))
	for _, channel := range channels {
		actualNames = append(actualNames, channel.Name)
	}
	if !reflect.DeepEqual(actualNames, expectedNames) {
		t.Errorf("order = %v, want %v", actualNames, expectedNames)
	}

	// 国名・国旗が付与される (コードは大文字に正規化される)
	alpha := channels[0]
	if alpha.Country == nil || *alpha.Country != "JP" || alpha.CountryName == nil || *alpha.CountryName != "Japan" {
		t.Errorf("alpha country = %+v", alpha.Country)
	}
	if alpha.CountryFlag == nil || *alpha.CountryFlag != "🇯🇵" {
		t.Errorf("alpha flag = %v", alpha.CountryFlag)
	}
	if alpha.ID != BuildChannelID(alpha.URL) {
		t.Errorf("alpha id = %s", alpha.ID)
	}

	// 取得に失敗したソースはエラーとして記録され、他のソースには影響しない
	errorsBySource := manager.SourceErrors()
	if len(errorsBySource) != 1 || errorsBySource[server.URL+"/missing.m3u"] != "HTTP Error 404" {
		t.Errorf("errors = %v", errorsBySource)
	}
	if manager.UpdatedAt() <= 0 {
		t.Error("updated_at should be set")
	}

	// ストリーム用のヘッダー (User-Agent / Referer) が記録される
	headers := manager.StreamHeaders("https://example.com/beta.m3u8")
	if headers["User-Agent"] != "ChannelAgent/2.0" || headers["Referer"] != "https://example.com/" {
		t.Errorf("headers = %v", headers)
	}
	// URL が一致しない場合はホスト名でフォールバックする
	headers = manager.StreamHeaders("https://example.com/beta/segment1.ts")
	if headers["User-Agent"] != "ChannelAgent/2.0" {
		t.Errorf("fallback headers = %v", headers)
	}
	// 登録の無いホストの場合は何も返さない
	if headers := manager.StreamHeaders("https://other.example.com/stream.ts"); len(headers) != 0 {
		t.Errorf("unknown headers = %v", headers)
	}

	// display_channel_id からチャンネルを取得できる
	displayChannelID := BuildDisplayChannelID(alpha.URL)
	if found := manager.GetChannelByDisplayChannelID(displayChannelID); found == nil || found.ID != alpha.ID {
		t.Errorf("GetChannelByDisplayChannelID = %v", found)
	}
	if found := manager.GetChannelByDisplayChannelID("iptv999999999999"); found != nil {
		t.Errorf("unknown display_channel_id = %v", found)
	}

	// すべてのリクエストに設定の User-Agent が送信される
	userAgentsMu.Lock()
	defer userAgentsMu.Unlock()
	for _, userAgent := range userAgents {
		if userAgent != "TestAgent/1.0" {
			t.Errorf("user agent = %q", userAgent)
		}
	}
}

// TestRefreshCacheAndForce はキャッシュが有効な間は再取得しないことを検証する。
func TestRefreshCacheAndForce(t *testing.T) {
	cfg := testConfig()
	requests := 0
	manager, server := newTestManager(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/countries.json" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		requests++
		_, _ = w.Write([]byte(testPlaylistA))
	})
	cfg.IPTV.Sources = []string{server.URL + "/a.m3u"}

	manager.Refresh(context.Background(), false)
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	// キャッシュが有効な間は再取得しない
	manager.Refresh(context.Background(), false)
	if requests != 1 {
		t.Errorf("requests after cache hit = %d, want 1", requests)
	}
	// force の場合は再取得する
	manager.Refresh(context.Background(), true)
	if requests != 2 {
		t.Errorf("requests after force = %d, want 2", requests)
	}

	// ローカルファイルのソースも読み込める
	localPath := filepath.Join(t.TempDir(), "local.m3u")
	if err := os.WriteFile(localPath, []byte("#EXTM3U\n#EXTINF:-1,Local Channel\nhttps://example.com/local.m3u8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.IPTV.Sources = []string{localPath}
	channels := manager.Refresh(context.Background(), true)
	if len(channels) != 1 || channels[0].Name != "Local Channel" {
		t.Fatalf("local channels = %+v", channels)
	}
	// 存在しないファイルはエラーとして記録される
	cfg.IPTV.Sources = []string{filepath.Join(t.TempDir(), "missing.m3u")}
	if channels := manager.Refresh(context.Background(), true); len(channels) != 0 {
		t.Errorf("channels = %+v", channels)
	}
	if message := manager.SourceErrors()[cfg.IPTV.Sources[0]]; message != "File Not Found" {
		t.Errorf("error = %q", message)
	}
}

// TestRefreshDisabled は IPTV 機能が無効な場合は何もしないことを検証する。
func TestRefreshDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.IPTV.Enabled = false
	requests := 0
	manager, server := newTestManager(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		requests++
	})
	cfg.IPTV.Sources = []string{server.URL + "/a.m3u"}

	if channels := manager.Refresh(context.Background(), true); len(channels) != 0 {
		t.Errorf("channels = %+v", channels)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
}

// TestFilterChannels は国・グループ・キーワードでの絞り込みを検証する。
func TestFilterChannels(t *testing.T) {
	cfg := testConfig()
	manager, server := newTestManager(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/countries.json" {
			_, _ = w.Write([]byte(testCountriesJSON))
			return
		}
		_, _ = w.Write([]byte(testPlaylistA))
	})
	cfg.IPTV.Sources = []string{server.URL + "/a.m3u"}
	manager.Refresh(context.Background(), false)

	if channels := manager.FilterChannels(nil, nil, nil); len(channels) != 3 {
		t.Errorf("all = %d", len(channels))
	}
	// 国コードは大文字小文字を区別しない (tvg-id="ChannelA.jp" から JP と推定されるチャンネルのみ)
	if channels := manager.FilterChannels(strPtr("jp"), nil, nil); len(channels) != 1 {
		t.Errorf("country jp = %d", len(channels))
	}
	if channels := manager.FilterChannels(strPtr("us"), nil, nil); len(channels) != 0 {
		t.Errorf("country us = %d", len(channels))
	}
	if channels := manager.FilterChannels(nil, strPtr("News"), nil); len(channels) != 1 {
		t.Errorf("group = %d", len(channels))
	}
	// キーワードは大文字小文字を区別しない部分一致
	if channels := manager.FilterChannels(nil, nil, strPtr("ALPHA")); len(channels) != 1 {
		t.Errorf("search = %d", len(channels))
	}
	if channels := manager.FilterChannels(nil, nil, strPtr("channel")); len(channels) != 3 {
		t.Errorf("search channel = %d", len(channels))
	}
}

// TestDetectChannelQualities は画質の検出とキャッシュを検証する。
func TestDetectChannelQualities(t *testing.T) {
	cfg := testConfig()
	requests := 0
	manager, server := newTestManager(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS=\"avc1.4d401f\"\n1080p.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n360p.m3u8\n"))
	})

	channel := &Channel{
		ID:        "test",
		Name:      "Test Channel",
		URL:       server.URL + "/live/master.m3u8",
		UserAgent: strPtr("ChannelAgent/1.0"),
		Referrer:  strPtr("https://example.com/"),
	}
	result := manager.DetectChannelQualities(context.Background(), channel)
	if result.SourceQuality == nil || *result.SourceQuality != "1080p" {
		t.Fatalf("source quality = %v", result.SourceQuality)
	}
	if result.Codec == nil || *result.Codec != "H.264" {
		t.Errorf("codec = %v", result.Codec)
	}
	if len(result.Qualities) != 2 {
		t.Fatalf("qualities = %+v", result.Qualities)
	}
	if *result.Qualities[1].Name != "360p" || *result.Qualities[1].Width != 640 {
		t.Errorf("qualities[1] = %+v", result.Qualities[1])
	}

	// 2回目はキャッシュから返すためリクエストが増えない
	manager.DetectChannelQualities(context.Background(), channel)
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}

	// 複数チャンネルの画質を並行して検出する (id をキーにした結果が返る)
	other := &Channel{ID: "other", Name: "Other", URL: server.URL + "/other.m3u8"}
	detected := manager.DetectChannelsQualities(context.Background(), []*Channel{channel, other})
	if len(detected) != 2 {
		t.Fatalf("detected = %v", detected)
	}
	if detected["other"].SourceQuality == nil || *detected["other"].SourceQuality != "1080p" {
		t.Errorf("other = %+v", detected["other"])
	}

	// HLS 以外 (TS など) の場合は画質を取得できない
	ts := &Channel{ID: "ts", Name: "TS", URL: "https://example.com/live.ts"}
	if result := manager.DetectChannelQualities(context.Background(), ts); len(result.Qualities) != 0 || result.SourceQuality != nil {
		t.Errorf("ts = %+v", result)
	}
}

// TestSplitLinesAndQuote は文字列ユーティリティの挙動を検証する。
func TestSplitLinesAndQuote(t *testing.T) {
	if actual := SplitLines("a\nb"); !reflect.DeepEqual(actual, []string{"a", "b"}) {
		t.Errorf("SplitLines = %v", actual)
	}
	if actual := SplitLines("a\n"); !reflect.DeepEqual(actual, []string{"a"}) {
		t.Errorf("SplitLines = %v", actual)
	}
	if actual := SplitLines("a\n\n"); !reflect.DeepEqual(actual, []string{"a", ""}) {
		t.Errorf("SplitLines = %v", actual)
	}
	if actual := SplitLines(""); len(actual) != 0 {
		t.Errorf("SplitLines = %v", actual)
	}
	if actual := SplitLines("a\r\nb\rc"); !reflect.DeepEqual(actual, []string{"a", "b", "c"}) {
		t.Errorf("SplitLines = %v", actual)
	}

	// Python の quote(safe="") と同じ結果になる
	for value, expected := range map[string]string{
		"https://example.com/a b.m3u8": "https%3A%2F%2Fexample.com%2Fa%20b.m3u8",
		"a+b":                          "a%2Bb",
		"a~b_c-d.e":                    "a~b_c-d.e",
		"日本語":                          "%E6%97%A5%E6%9C%AC%E8%AA%9E",
	} {
		if actual := quotePython(value); actual != expected {
			t.Errorf("quotePython(%q) = %s, want %s", value, actual, expected)
		}
	}
}

// TestNormalizeURL は相対 URL の解決を検証する。
func TestNormalizeURL(t *testing.T) {
	for _, testCase := range []struct {
		base     string
		target   string
		expected string
	}{
		{"https://example.com/a/b.m3u8", "c.m3u8", "https://example.com/a/c.m3u8"},
		{"https://example.com/a/b.m3u8", "/c.m3u8", "https://example.com/c.m3u8"},
		{"https://example.com/a/b.m3u8", "https://other.com/d.m3u8", "https://other.com/d.m3u8"},
		{"https://example.com/a/b.m3u8", "//other.com/d.m3u8", "https://other.com/d.m3u8"},
		{"https://example.com/a/b.m3u8", "?x=1", "https://example.com/a/b.m3u8?x=1"},
		{"https://example.com/a/b.m3u8", " c.m3u8 ", "https://example.com/a/c.m3u8"},
	} {
		if actual := NormalizeURL(testCase.base, testCase.target); actual != testCase.expected {
			t.Errorf("NormalizeURL(%q, %q) = %q, want %q", testCase.base, testCase.target, actual, testCase.expected)
		}
	}
}

// TestIsHLSPlaylist は HLS プレイリストの判定を検証する。
func TestIsHLSPlaylist(t *testing.T) {
	for _, testCase := range []struct {
		streamURL string
		expected  bool
	}{
		{"https://example.com/a.m3u8", true},
		{"https://example.com/a.M3U8", true},
		{"https://example.com/a.m3u8?token=1", true},
		{"https://example.com/a.ts", false},
		{"https://example.com/live?type=hls", false},
	} {
		if actual := IsHLSPlaylist(testCase.streamURL); actual != testCase.expected {
			t.Errorf("IsHLSPlaylist(%q) = %v, want %v", testCase.streamURL, actual, testCase.expected)
		}
	}
}

// TestBuildQualityNameAndCodecName は画質名・コーデック名の生成を検証する。
func TestBuildQualityNameAndCodecName(t *testing.T) {
	if actual := BuildQualityName(nil); actual != nil {
		t.Errorf("nil height = %v", actual)
	}
	for height, expected := range map[int]string{
		2160: "2160p",
		2161: "2160p",
		1440: "1440p",
		1080: "1080p",
		1069: "720p",
		241:  "240p",
		144:  "144p",
	} {
		if actual := BuildQualityName(&height); actual == nil || *actual != expected {
			t.Errorf("BuildQualityName(%d) = %v, want %s", height, actual, expected)
		}
	}
	if actual := BuildCodecName(nil); actual != nil {
		t.Errorf("nil codecs = %v", actual)
	}
	for codecs, expected := range map[string]string{
		"avc1.4d401f,mp4a.40.2": "H.264",
		"hvc1.1.6.L150.90":      "H.265",
		"hev1.1.6.L150.90":      "H.265",
		"av01.0.08M.08":         "AV1",
		"vp09.00.10.08":         "VP9",
		"mp4a.40.2":             "",
		"unknown":               "",
	} {
		actual := BuildCodecName(&codecs)
		if expected == "" {
			if actual != nil {
				t.Errorf("BuildCodecName(%q) = %v, want nil", codecs, actual)
			}
			continue
		}
		if actual == nil || *actual != expected {
			t.Errorf("BuildCodecName(%q) = %v, want %s", codecs, actual, expected)
		}
	}
}

// TestTVUIRegisterLimit は登録数の上限を検証する。
func TestTVUIRegisterLimit(t *testing.T) {
	manager := New(Options{Config: testConfig(), DataDir: t.TempDir()})
	displayChannelIDs := []string{}
	for index := 0; index < TVUIMaxChannels+5; index++ {
		displayChannelIDs = manager.RegisterTVUIChannel("user:1", "iptv"+strconvItoa(index))
	}
	if len(displayChannelIDs) != TVUIMaxChannels {
		t.Fatalf("length = %d, want %d", len(displayChannelIDs), TVUIMaxChannels)
	}
	// 古いものから削除され、最新が末尾に残る
	if displayChannelIDs[len(displayChannelIDs)-1] != "iptv"+strconvItoa(TVUIMaxChannels+4) {
		t.Errorf("last = %s", displayChannelIDs[len(displayChannelIDs)-1])
	}
	if displayChannelIDs[0] != "iptv5" {
		t.Errorf("first = %s", displayChannelIDs[0])
	}
}

// TestGetTVUIChannels は登録順にチャンネルを返すことを検証する。
func TestGetTVUIChannels(t *testing.T) {
	manager := New(Options{Config: testConfig(), DataDir: t.TempDir()})
	first := &Channel{Name: "First", URL: "https://example.com/first.m3u8"}
	second := &Channel{Name: "Second", URL: "https://example.com/second.m3u8"}
	manager.current.byDisplayID = map[string]*Channel{
		BuildDisplayChannelID(first.URL):  first,
		BuildDisplayChannelID(second.URL): second,
	}
	manager.RegisterTVUIChannel("user:1", BuildDisplayChannelID(second.URL))
	manager.RegisterTVUIChannel("user:1", BuildDisplayChannelID(first.URL))
	// 存在しない display_channel_id は無視される
	manager.RegisterTVUIChannel("user:1", "iptv123456789")

	channels := manager.GetTVUIChannels("user:1")
	if len(channels) != 2 || channels[0] != second || channels[1] != first {
		t.Fatalf("channels = %+v", channels)
	}
}

// strconvItoa は整数を文字列にする。
func strconvItoa(value int) string {
	digits := ""
	for {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
		if value == 0 {
			return digits
		}
	}
}
