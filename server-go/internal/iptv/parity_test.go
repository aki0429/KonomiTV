package iptv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// parityFixture は Python 版 IPTVUtil の実行結果を格納したフィクスチャ。
type normalizeURLCase struct {
	Base     string `json:"base"`
	Target   string `json:"target"`
	Expected string `json:"expected"`
}

type parityFixture struct {
	SourceURL          string                 `json:"source_url"`
	Playlist           string                 `json:"playlist"`
	Countries          map[string]CountryInfo `json:"countries"`
	Channels           []map[string]any       `json:"channels"`
	RawChannels        []map[string]any       `json:"raw_channels"`
	DisplayChannelIDs  map[string]string      `json:"display_channel_ids"`
	ProxyURLs          map[string]string      `json:"proxy_urls"`
	QualityNames       map[string]*string     `json:"quality_names"`
	HLSMasterQualities map[string]any         `json:"hls_master_qualities"`
	HLSMediaQualities  map[string]any         `json:"hls_media_qualities"`
	RewrittenMaster    string                 `json:"rewritten_master"`
	RewrittenMedia     string                 `json:"rewritten_media"`
	StreamTypes        map[string]string      `json:"stream_types"`
	M3UOutput          string                 `json:"m3u_output"`
	Groups             []Group                `json:"groups"`
	CountriesList      []Country              `json:"countries_list"`
	NormalizeURLs      []normalizeURLCase     `json:"normalize_urls"`
	TVUIRegistryJSON   string                 `json:"tvui_registry_json"`
	UserSourcesJSON    string                 `json:"user_sources_json"`
}

// loadParityFixture はフィクスチャを読み込む。
func loadParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "iptv_parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &parityFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// toGenericMap は任意の値を JSON 経由で map[string]any に変換する。
func toGenericMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// insertDefaults は API レスポンスで既定値が入るフィールドを補う。
func insertDefaults(entry map[string]any) map[string]any {
	entry["is_tvui_registered"] = false
	entry["source_quality"] = nil
	entry["source_codec"] = nil
	entry["qualities"] = []any{}
	return entry
}

// TestParseM3UPlaylistParity は Python 版と同じチャンネルが解析されることを検証する。
func TestParseM3UPlaylistParity(t *testing.T) {
	fixture := loadParityFixture(t)
	channels := ParseM3UPlaylist(fixture.Playlist, fixture.SourceURL, fixture.Countries)
	if len(channels) != len(fixture.RawChannels) {
		t.Fatalf("channels = %d, want %d", len(channels), len(fixture.RawChannels))
	}

	for index, channel := range channels {
		expected := fixture.RawChannels[index]
		// Go 側の構造体を Python の dataclass と同じキー名で JSON 化して比較する
		actual := toGenericMap(t, map[string]any{
			"id":             channel.ID,
			"name":           channel.Name,
			"url":            channel.URL,
			"logo_url":       channel.LogoURL,
			"group":          channel.Group,
			"country":        channel.Country,
			"country_name":   channel.CountryName,
			"country_flag":   channel.CountryFlag,
			"tvg_id":         channel.TvgID,
			"language":       channel.Language,
			"user_agent":     channel.UserAgent,
			"referrer":       channel.Referrer,
			"is_geo_blocked": channel.IsGeoBlocked,
			"source_url":     channel.SourceURL,
		})
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("channel %d:\n got %v\nwant %v", index, actual, expected)
		}
	}
}

// TestChannelToResponseParity は API レスポンスへの変換が Python 版と一致することを検証する。
func TestChannelToResponseParity(t *testing.T) {
	fixture := loadParityFixture(t)
	channels := ParseM3UPlaylist(fixture.Playlist, fixture.SourceURL, fixture.Countries)

	for index, channel := range channels {
		expected := insertDefaults(fixture.Channels[index])
		actual := toGenericMap(t, channel.ToResponse())
		// 期待値側に無いキーは比較しない (source_quality などの既定値は補っている)
		for key, value := range actual {
			if _, exists := expected[key]; !exists {
				t.Errorf("channel %d: unexpected key %q", index, key)
				continue
			}
			if !reflect.DeepEqual(value, expected[key]) {
				t.Errorf("channel %d: %s = %v, want %v", index, key, value, expected[key])
			}
		}
		for key := range expected {
			if _, exists := actual[key]; !exists {
				t.Errorf("channel %d: missing key %q", index, key)
			}
		}

		// display_channel_id・プロキシ URL も個別に検証する
		if displayChannelID := BuildDisplayChannelID(channel.URL); displayChannelID != fixture.DisplayChannelIDs[channel.URL] {
			t.Errorf("display_channel_id = %s, want %s", displayChannelID, fixture.DisplayChannelIDs[channel.URL])
		}
		if proxyURL := BuildProxyURL(channel.URL); proxyURL != fixture.ProxyURLs[channel.URL] {
			t.Errorf("proxy url = %s, want %s", proxyURL, fixture.ProxyURLs[channel.URL])
		}
	}
}

// TestQualityNameParity はチャンネル名からの画質の抽出結果が一致することを検証する。
func TestQualityNameParity(t *testing.T) {
	fixture := loadParityFixture(t)
	for name, expected := range fixture.QualityNames {
		if actual := ParseQualityFromName(name); !reflect.DeepEqual(actual, expected) {
			t.Errorf("ParseQualityFromName(%q) = %v, want %v", name, actual, expected)
		}
	}
}

// TestParseHLSQualitiesParity は HLS プレイリストからの画質の抽出結果が一致することを検証する。
func TestParseHLSQualitiesParity(t *testing.T) {
	fixture := loadParityFixture(t)
	for name, content := range map[string]string{
		"master": contentOf(fixture, "hls_master"),
		"media":  contentOf(fixture, "hls_media"),
	} {
		expected := fixture.HLSMasterQualities
		if name == "media" {
			expected = fixture.HLSMediaQualities
		}
		// Python 版の ParseHLSQualities() の戻り値には source_quality が含まれないため、
		// 共通するキーのみ比較し、source_quality は別途検証する
		result := ParseHLSQualities(content)
		actual := toGenericMap(t, result)
		for key, value := range expected {
			if !reflect.DeepEqual(actual[key], value) {
				t.Errorf("%s: %s = %v, want %v", name, key, actual[key], value)
			}
		}
		// 最も高画質なバリアントの画質が「元配信の画質」になる
		if len(result.Qualities) > 0 && !reflect.DeepEqual(result.SourceQuality, result.Qualities[0].Name) {
			t.Errorf("%s: source_quality = %v", name, result.SourceQuality)
		}
	}
}

// contentOf はフィクスチャに埋め込まれた HLS プレイリストを取り出す。
func contentOf(fixture *parityFixture, name string) string {
	switch name {
	case "hls_master":
		return "#EXTM3U\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=640x360,CODECS=\"avc1.4d401f,mp4a.40.2\"\n360p.m3u8\n" +
			"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=12800000,RESOLUTION=3840x2160,CODECS=\"hvc1.1.6.L150.90\"\n2160p.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n1080p.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,CODECS=\"av01.0.08M.08\"\n1080p-high.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=800000\n240p.m3u8\n"
	default:
		return "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.000,\nsegment1.ts\n#EXTINF:4.000,\nsegment2.ts\n"
	}
}

// TestRewriteHLSPlaylistParity は HLS プレイリストの書き換え結果が一致することを検証する。
func TestRewriteHLSPlaylistParity(t *testing.T) {
	fixture := loadParityFixture(t)

	master := RewriteHLSPlaylist(contentOf(fixture, "hls_master"), "https://example.com/live/master.m3u8")
	if master != fixture.RewrittenMaster {
		t.Errorf("master:\n got %q\nwant %q", master, fixture.RewrittenMaster)
	}

	media := RewriteHLSPlaylist(
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x1\n#EXT-X-MAP:URI=\"init.mp4\"\nseg1.ts\n\n",
		"https://example.com/live/out/index.m3u8",
	)
	if media != fixture.RewrittenMedia {
		t.Errorf("media:\n got %q\nwant %q", media, fixture.RewrittenMedia)
	}
}

// TestNormalizeURLParity は相対 URL の解決結果が Python の urljoin() と一致することを検証する。
func TestNormalizeURLParity(t *testing.T) {
	fixture := loadParityFixture(t)
	if len(fixture.NormalizeURLs) == 0 {
		t.Fatal("normalize_urls がフィクスチャに含まれていません")
	}
	for _, testCase := range fixture.NormalizeURLs {
		if actual := NormalizeURL(testCase.Base, testCase.Target); actual != testCase.Expected {
			t.Errorf("NormalizeURL(%q, %q) = %q, want %q", testCase.Base, testCase.Target, actual, testCase.Expected)
		}
	}
}

// TestDetectStreamTypeParity は配信フォーマットの推定結果が一致することを検証する。
func TestDetectStreamTypeParity(t *testing.T) {
	fixture := loadParityFixture(t)
	for streamURL, expected := range fixture.StreamTypes {
		if actual := DetectStreamType(streamURL); actual != expected {
			t.Errorf("DetectStreamType(%q) = %s, want %s", streamURL, actual, expected)
		}
	}
}

// TestBuildM3UPlaylistParity はプレイリストの出力結果が一致することを検証する。
func TestBuildM3UPlaylistParity(t *testing.T) {
	fixture := loadParityFixture(t)
	channels := ParseM3UPlaylist(fixture.Playlist, fixture.SourceURL, fixture.Countries)
	if actual := BuildM3UPlaylist(channels); actual != fixture.M3UOutput {
		t.Errorf("m3u:\n got %q\nwant %q", actual, fixture.M3UOutput)
	}
}

// TestCountriesAndGroupsParity は国・グループの集計結果が一致することを検証する。
func TestCountriesAndGroupsParity(t *testing.T) {
	fixture := loadParityFixture(t)
	channels := ParseM3UPlaylist(fixture.Playlist, fixture.SourceURL, fixture.Countries)

	manager := New(Options{
		Config:  testConfig(),
		DataDir: t.TempDir(),
	})
	// 国・グループの集計はキャッシュに依存するため、フィクスチャの内容を直接キャッシュへ反映する
	manager.current.channels = channels
	manager.current.countriesByCode = fixture.Countries

	if actual := manager.Groups(channels); !reflect.DeepEqual(actual, fixture.Groups) {
		t.Errorf("groups:\n got %v\nwant %v", actual, fixture.Groups)
	}
	if actual := manager.Countries(channels); !reflect.DeepEqual(actual, fixture.CountriesList) {
		t.Errorf("countries:\n got %v\nwant %v", actual, fixture.CountriesList)
	}
}

// TestTVUIRegistryParity は Python 版が保存したファイルを読み書きできることを検証する。
func TestTVUIRegistryParity(t *testing.T) {
	fixture := loadParityFixture(t)
	dataDir := t.TempDir()
	manager := New(Options{Config: testConfig(), DataDir: dataDir})

	// Python 版が書き出したファイルを読み込める
	if err := os.WriteFile(filepath.Join(dataDir, TVUIChannelsFileName), []byte(fixture.TVUIRegistryJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if ids := manager.LoadTVUIChannelIDs("user:1"); !reflect.DeepEqual(ids, []string{"iptv1", "iptv2"}) {
		t.Errorf("user:1 = %v", ids)
	}
	if ids := manager.LoadTVUIChannelIDs("anon:00000000-0000-0000-0000-000000000000"); len(ids) != 0 {
		t.Errorf("anon = %v", ids)
	}
	if ids := manager.LoadTVUIChannelIDs("user:999"); ids == nil || len(ids) != 0 {
		t.Errorf("unknown user = %v, want empty slice", ids)
	}

	// 既に登録済みのチャンネルは末尾に移動する
	manager.RegisterTVUIChannel("user:1", "iptv1")
	if ids := manager.LoadTVUIChannelIDs("user:1"); !reflect.DeepEqual(ids, []string{"iptv2", "iptv1"}) {
		t.Errorf("after register = %v", ids)
	}
	// 削除できる
	manager.UnregisterTVUIChannel("user:1", "iptv2")
	if ids := manager.LoadTVUIChannelIDs("user:1"); !reflect.DeepEqual(ids, []string{"iptv1"}) {
		t.Errorf("after unregister = %v", ids)
	}
	// 存在しないチャンネルの削除ではファイルを書き換えない
	manager.UnregisterTVUIChannel("user:1", "iptv999")
	if ids := manager.LoadTVUIChannelIDs("user:1"); !reflect.DeepEqual(ids, []string{"iptv1"}) {
		t.Errorf("after unregister unknown = %v", ids)
	}

	// 他のユーザーの登録内容が保持されている
	if ids := manager.LoadTVUIChannelIDs("anon:00000000-0000-0000-0000-000000000000"); len(ids) != 0 {
		t.Errorf("anon after write = %v", ids)
	}
}

// TestUserSourcesParity は Python 版が保存したファイルを読み書きできることを検証する。
func TestUserSourcesParity(t *testing.T) {
	fixture := loadParityFixture(t)
	dataDir := t.TempDir()
	manager := New(Options{Config: testConfig(), DataDir: dataDir})

	if err := os.WriteFile(filepath.Join(dataDir, UserSourcesFileName), []byte(fixture.UserSourcesJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	expected := []string{"https://example.com/a.m3u", "https://example.com/b.m3u"}
	if actual := manager.LoadUserSources(); !reflect.DeepEqual(actual, expected) {
		t.Errorf("sources = %v, want %v", actual, expected)
	}
	// 存在しないファイルの場合は空のリストを返す
	if actual := New(Options{Config: testConfig(), DataDir: t.TempDir()}).LoadUserSources(); len(actual) != 0 || actual == nil {
		t.Errorf("missing file = %v, want empty slice", actual)
	}

	// 保存した内容を再読み込みできる
	manager.SaveUserSources([]string{"https://example.com/c.m3u"})
	if actual := manager.LoadUserSources(); !reflect.DeepEqual(actual, []string{"https://example.com/c.m3u"}) {
		t.Errorf("after save = %v", actual)
	}
	// 壊れたファイルの場合は空のリストを返す
	if err := os.WriteFile(filepath.Join(dataDir, UserSourcesFileName), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if actual := manager.LoadUserSources(); len(actual) != 0 || actual == nil {
		t.Errorf("broken file = %v, want empty slice", actual)
	}
}

// TestAllSourceURLsParity は設定と追加登録分のソースが重複なく統合されることを検証する。
func TestAllSourceURLsParity(t *testing.T) {
	dataDir := t.TempDir()
	cfg := testConfig()
	cfg.IPTV.Sources = []string{"  https://example.com/a.m3u  ", "", "https://example.com/b.m3u"}
	manager := New(Options{Config: cfg, DataDir: dataDir})
	manager.SaveUserSources([]string{"https://example.com/b.m3u", "https://example.com/c.m3u"})

	expected := []string{
		"https://example.com/a.m3u",
		"https://example.com/b.m3u",
		"https://example.com/c.m3u",
	}
	if actual := manager.AllSourceURLs(); !reflect.DeepEqual(actual, expected) {
		t.Errorf("sources = %v, want %v", actual, expected)
	}
	if actual := manager.ConfigSources(); !reflect.DeepEqual(actual, []string{"https://example.com/a.m3u", "https://example.com/b.m3u"}) {
		t.Errorf("config sources = %v", actual)
	}
}

// TestIsDisplayChannelID は IPTV の疑似チャンネル ID の判定を検証する。
func TestIsDisplayChannelID(t *testing.T) {
	for _, testCase := range []struct {
		displayChannelID string
		expected         bool
	}{
		{"iptv12345", true},
		{"iptv0", true},
		{"iptv", false},
		{"iptv123a", false},
		{"gr011", false},
		{"IPTV-iptv123", false},
	} {
		if actual := IsDisplayChannelID(testCase.displayChannelID); actual != testCase.expected {
			t.Errorf("IsDisplayChannelID(%q) = %v, want %v", testCase.displayChannelID, actual, testCase.expected)
		}
	}
}

// TestBuildUserKey はユーザーキーの生成を検証する。
func TestBuildUserKey(t *testing.T) {
	if actual := BuildUserKey(nil); actual != "anonymous" {
		t.Errorf("anonymous = %s", actual)
	}
	userID := int64(42)
	if actual := BuildUserKey(&userID); actual != "user:42" {
		t.Errorf("user = %s", actual)
	}
}

// TestLiveAndEncodingChannelConversion は擬似チャンネルへの変換を検証する。
func TestLiveAndEncodingChannelConversion(t *testing.T) {
	channel := &Channel{
		Name:         "NHK World-Japan (1080p)",
		URL:          "https://example.com/nhk/index.m3u8",
		Country:      strPtr("JP"),
		CountryName:  strPtr("Japan"),
		CountryFlag:  strPtr("🇯🇵"),
		IsGeoBlocked: false,
		SourceURL:    "https://example.com/jp.m3u",
	}
	channel.ID = BuildChannelID(channel.URL)
	displayChannelID := BuildDisplayChannelID(channel.URL)

	live := channel.ToLiveChannelResponse()
	if live.ID != ChannelIDPrefix+displayChannelID || live.DisplayChannelID != displayChannelID {
		t.Errorf("live ids = %s / %s", live.ID, live.DisplayChannelID)
	}
	if live.Name != "NHK World-Japan (1080p) (Japan)" || live.ChannelNumber != "JP" || live.Type != "IPTV" {
		t.Errorf("live = %+v", live)
	}
	if live.ViewerCount != 0 || !live.IsWatchable || !live.IsDisplay || live.TransportStreamID != nil {
		t.Errorf("live = %+v", live)
	}

	encoding := channel.ToEncodingChannel()
	if encoding.ID != ChannelIDPrefix+displayChannelID || encoding.Type != "GR" || encoding.ChannelNumber != "JP" {
		t.Errorf("encoding = %+v", encoding)
	}
	if encoding.NetworkID != 0 || encoding.ServiceID != 0 || encoding.RemoconID != 0 {
		t.Errorf("encoding = %+v", encoding)
	}

	// 国が不明な場合はチャンネル番号が IPTV になる
	unknown := &Channel{Name: "Unknown", URL: "https://example.com/unknown.ts"}
	unknown.ID = BuildChannelID(unknown.URL)
	if actual := unknown.ToLiveChannelResponse(); actual.ChannelNumber != "IPTV" || actual.Name != "Unknown" {
		t.Errorf("unknown = %+v", actual)
	}
}

// strPtr は文字列のポインタを返す。
func strPtr(value string) *string {
	return &value
}
