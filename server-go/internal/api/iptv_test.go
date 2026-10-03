package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/iptv"
)

// iptvTestPlaylist はテスト用の M3U プレイリスト。
const iptvTestPlaylist = `#EXTM3U
#EXTINF:-1 tvg-id="Alpha.jp" group-title="News" tvg-logo="%LOGO%",Alpha Channel (720p)
%STREAM_A%
#EXTINF:-1 tvg-country="US" group-title="News",Beta Channel
%STREAM_B%
#EXTINF:-1 tvg-country="US" group-title="News",Gamma Channel
%STREAM_C%
#EXTINF:-1 tvg-country="US" group-title="Sports",Delta Channel
%STREAM_D%
`

// iptvTestCountries はテスト用の国一覧 API のレスポンス。
const iptvTestCountries = `[{"code": "JP", "name": "Japan", "flag": "🇯🇵"}, {"code": "US", "name": "United States", "flag": "🇺🇸"}]`

// setupIPTVTestServer は IPTV のテスト用サーバーを構築する。
func setupIPTVTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	server, paths := newTestServer(t, "")
	// IPTV 機能を有効化する (config は Server と iptv.Manager で共有される)
	server.config.IPTV.Enabled = true
	server.config.IPTV.UserAgent = "TestAgent/1.0"
	server.config.IPTV.CacheTTL = 3600

	// ストリーム・ロゴ・プレイリストを配信するダミーの配信サーバー
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/countries.json":
			_, _ = w.Write([]byte(iptvTestCountries))
		case "/jp.m3u":
			playlist := strings.NewReplacer(
				"%LOGO%", upstreamURL(r, "/logo.png"),
				"%STREAM_A%", upstreamURL(r, "/streams/alpha.m3u8"),
				"%STREAM_B%", upstreamURL(r, "/streams/beta.m3u8"),
				"%STREAM_C%", upstreamURL(r, "/streams/gamma.ts"),
				"%STREAM_D%", upstreamURL(r, "/streams/delta.mp4"),
			).Replace(iptvTestPlaylist)
			_, _ = w.Write([]byte(playlist))
		case "/streams/alpha.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS=\"avc1.4d401f\"\n1080p.m3u8\n"))
		case "/streams/beta.m3u8":
			// タグ内の URI とセグメントの書き換えを検証するためのメディアプレイリスト
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:4.0,\nsegment1.ts\n"))
		case "/streams/gamma.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write([]byte("streambody"))
		case "/streams/delta.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4body"))
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png; charset=binary")
			_, _ = w.Write([]byte("logodata"))
		case "/missing":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	manager := iptv.New(iptv.Options{
		Config:       server.config,
		DataDir:      paths.DataDir,
		Logger:       slog.New(slog.DiscardHandler),
		HTTPClient:   upstream.Client(),
		CountriesURL: upstream.URL + "/api/countries.json",
	})
	server.config.IPTV.Sources = []string{upstream.URL + "/jp.m3u"}
	server.iptv = manager

	// 既定のロゴを配置する (既定のロゴへのフォールバックを検証する)
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "default.png"), "defaultlogo")
	return server, upstream
}

// upstreamURL はリクエストからダミー配信サーバーの URL を組み立てる。
func upstreamURL(r *http.Request, path string) string {
	return "http://" + r.Host + path
}

// TestIPTVChannelsAPI はチャンネル一覧 API の挙動を検証する。
func TestIPTVChannelsAPI(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 4 || response.AllTotal != 4 || len(response.Channels) != 4 {
		t.Fatalf("total/all_total/channels = %d/%d/%d", response.Total, response.AllTotal, len(response.Channels))
	}
	if response.Page != 1 || response.PerPage != iptvDefaultPerPage || response.MaxPage != 1 {
		t.Errorf("pagination = %+v", response)
	}
	if response.UpdatedAt == nil || *response.UpdatedAt <= 0 {
		t.Errorf("updated_at = %v", response.UpdatedAt)
	}
	if len(response.Sources) != 1 || len(response.Errors) != 0 {
		t.Errorf("sources/errors = %v/%v", response.Sources, response.Errors)
	}

	// 国名 → グループ名 → チャンネル名の順に並ぶ
	expectedNames := []string{"Alpha Channel (720p)", "Beta Channel", "Gamma Channel", "Delta Channel"}
	for index, name := range expectedNames {
		if response.Channels[index].Name != name {
			t.Errorf("channels[%d] = %s, want %s", index, response.Channels[index].Name, name)
		}
	}
	alpha := response.Channels[0]
	if alpha.Country == nil || *alpha.Country != "JP" || alpha.CountryName == nil || *alpha.CountryName != "Japan" {
		t.Errorf("alpha country = %v/%v", alpha.Country, alpha.CountryName)
	}
	if alpha.Group == nil || *alpha.Group != "News" || !alpha.IsHLS || alpha.StreamType != "HLS" {
		t.Errorf("alpha = %+v", alpha)
	}
	if alpha.QualityHint == nil || *alpha.QualityHint != "720p" {
		t.Errorf("quality hint = %v", alpha.QualityHint)
	}
	if !strings.HasPrefix(alpha.StreamURL, "/api/iptv/proxy?url=") {
		t.Errorf("stream url = %s", alpha.StreamURL)
	}
	if !strings.HasPrefix(alpha.DisplayChannelID, "iptv") {
		t.Errorf("display channel id = %s", alpha.DisplayChannelID)
	}
	// レスポンスのチャンネルは display_channel_id から引き当てられる
	if found := server.iptv.GetChannelByDisplayChannelID(alpha.DisplayChannelID); found == nil || found.Name != alpha.Name {
		t.Errorf("channel by display_channel_id = %+v", found)
	}
	if alpha.IsTVUIRegistered {
		t.Error("is_tvui_registered should be false")
	}
	if response.Channels[1].StreamType != "HLS" || response.Channels[2].StreamType != "MPEGTS" || response.Channels[3].StreamType != "MP4" {
		t.Errorf("stream types = %s/%s/%s",
			response.Channels[1].StreamType, response.Channels[2].StreamType, response.Channels[3].StreamType)
	}

	// ***** ページネーション *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?page=2&per_page=3", "", "", "")
	var secondPage iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if secondPage.Page != 2 || secondPage.MaxPage != 2 || len(secondPage.Channels) != 1 || secondPage.Total != 4 {
		t.Errorf("page 2 = %+v", secondPage)
	}
	// 範囲外のページは最終ページに丸められる
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?page=99", "", "", "")
	var clamped iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &clamped); err != nil {
		t.Fatal(err)
	}
	if clamped.Page != 1 || len(clamped.Channels) != 4 {
		t.Errorf("clamped = %+v", clamped)
	}

	// ***** 絞り込み *****
	for _, testCase := range []struct {
		query    string
		expected int
	}{
		{"?country=us", 3},
		{"?country=US", 3},
		{"?country=jp", 1},
		{"?group=" + urlEncode("Sports"), 1},
		{"?group=" + urlEncode("News"), 3},
		{"?search=channel", 4},
		{"?search=beta", 1},
		{"?country=us&group=" + urlEncode("News"), 2},
		{"?country=us&search=gamma", 1},
		{"?search=nonexistent", 0},
	} {
		recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+testCase.query, "", "", "")
		var filtered iptvChannelsResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil {
			t.Fatal(err)
		}
		if filtered.Total != testCase.expected {
			t.Errorf("%s: total = %d, want %d", testCase.query, filtered.Total, testCase.expected)
		}
	}

	// ***** 不正なパラメーターは 422 *****
	for _, query := range []string{"?page=0", "?page=abc", "?per_page=0", "?per_page=501", "?per_page=abc"} {
		recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels"+query, "", "", "")
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", query, recorder.Code)
		}
	}

	// ***** 画質の検出 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?with_quality=true&search=alpha", "", "", "")
	var withQuality iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &withQuality); err != nil {
		t.Fatal(err)
	}
	if len(withQuality.Channels) != 1 {
		t.Fatalf("channels = %d", len(withQuality.Channels))
	}
	channel := withQuality.Channels[0]
	if channel.SourceQuality == nil || *channel.SourceQuality != "1080p" || channel.SourceCodec == nil || *channel.SourceCodec != "H.264" {
		t.Errorf("source quality = %v/%v", channel.SourceQuality, channel.SourceCodec)
	}
	if len(channel.Qualities) != 1 || *channel.Qualities[0].Height != 1080 {
		t.Errorf("qualities = %+v", channel.Qualities)
	}
	// 画質を取得できないチャンネルは null / 空配列になる
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?with_quality=true&search=gamma", "", "", "")
	var noQuality iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &noQuality); err != nil {
		t.Fatal(err)
	}
	if noQuality.Channels[0].SourceQuality != nil || len(noQuality.Channels[0].Qualities) != 0 {
		t.Errorf("beta qualities = %+v", noQuality.Channels[0])
	}
}

// TestIPTVChannelsDisabled は IPTV 機能が無効な場合の挙動を検証する。
func TestIPTVChannelsDisabled(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	server.config.IPTV.Enabled = false
	handler := server.Handler()

	for _, path := range []string{"/api/iptv/channels", "/api/iptv/countries", "/api/iptv/groups"} {
		recorder := doJSONRequest(t, handler, http.MethodGet, path, "", "", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `"total":0`) {
			t.Errorf("%s: body = %s", path, recorder.Body.String())
		}
	}

	// 国一覧には updated_at / all_total が含まれる
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/countries", "", "", "")
	if !strings.Contains(recorder.Body.String(), `"updated_at":null`) || !strings.Contains(recorder.Body.String(), `"all_total":0`) {
		t.Errorf("countries = %s", recorder.Body.String())
	}

	// チャンネル一覧は指定された page / per_page をそのまま返す
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels?page=3&per_page=10", "", "", "")
	var response iptvChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Page != 3 || response.PerPage != 10 || response.MaxPage != 1 || len(response.Channels) != 0 {
		t.Errorf("disabled = %+v", response)
	}
}

// TestIPTVCountriesAndGroupsAPI は国一覧・グループ一覧 API の挙動を検証する。
func TestIPTVCountriesAndGroupsAPI(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/countries", "", "", "")
	var countries iptvCountriesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &countries); err != nil {
		t.Fatal(err)
	}
	if countries.Total != 2 || countries.AllTotal != 4 || len(countries.Countries) != 2 {
		t.Fatalf("countries = %+v", countries)
	}
	// チャンネル数の多い順に並ぶ
	if countries.Countries[0].Code != "US" || countries.Countries[0].Count != 3 {
		t.Errorf("countries[0] = %+v", countries.Countries[0])
	}
	if countries.Countries[1].Code != "JP" || countries.Countries[1].Name != "Japan" || countries.Countries[1].Flag != "🇯🇵" {
		t.Errorf("countries[1] = %+v", countries.Countries[1])
	}

	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/groups", "", "", "")
	var groups iptvGroupsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if groups.Total != 2 || groups.Groups[0].Name != "News" || groups.Groups[0].Count != 3 || groups.Groups[1].Name != "Sports" {
		t.Errorf("groups = %+v", groups)
	}

	// 国コードで絞り込める
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/groups?country=us", "", "", "")
	var usGroups iptvGroupsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &usGroups); err != nil {
		t.Fatal(err)
	}
	if usGroups.Total != 2 || usGroups.Groups[0].Name != "News" || usGroups.Groups[0].Count != 2 || usGroups.Groups[1].Name != "Sports" {
		t.Errorf("us groups = %+v", usGroups.Groups)
	}
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/groups?country=jp", "", "", "")
	var jpGroups iptvGroupsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &jpGroups); err != nil {
		t.Fatal(err)
	}
	if jpGroups.Total != 1 || jpGroups.Groups[0].Name != "News" {
		t.Errorf("jp groups = %+v", jpGroups.Groups)
	}
}

// TestIPTVPlaylistAPI はプレイリスト出力 API の挙動を検証する。
func TestIPTVPlaylistAPI(t *testing.T) {
	server, upstream := setupIPTVTestServer(t)
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/playlist.m3u", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "audio/x-mpegurl") {
		t.Errorf("content type = %s", contentType)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cache control = %s", recorder.Header().Get("Cache-Control"))
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, "#EXTM3U\n") {
		t.Errorf("body = %s", body)
	}
	if !strings.Contains(body, "#EXTINF:-1 tvg-id=\"Alpha.jp\" tvg-logo=\""+upstream.URL+"/logo.png\" group-title=\"News\" tvg-country=\"JP\",Alpha Channel (720p)") {
		t.Errorf("body = %s", body)
	}
	if !strings.Contains(body, "/api/iptv/proxy?url=") {
		t.Errorf("body = %s", body)
	}

	// 絞り込みが反映される
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/playlist.m3u?country=US&group="+urlEncode("Sports"), "", "", "")
	body = recorder.Body.String()
	if strings.Count(body, "#EXTINF") != 1 || !strings.Contains(body, "Delta Channel") {
		t.Errorf("filtered body = %s", body)
	}
}

// TestIPTVSourcesAPI はプレイリストソース管理 API の挙動を検証する。
func TestIPTVSourcesAPI(t *testing.T) {
	server, upstream := setupIPTVTestServer(t)
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/sources", "", "", "")
	var sources iptvSourcesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources.ConfigSources) != 1 || len(sources.UserSources) != 0 {
		t.Fatalf("sources = %+v", sources)
	}
	if !strings.HasSuffix(sources.ConfigSources[0], "/jp.m3u") {
		t.Errorf("config sources = %v", sources.ConfigSources)
	}

	// ***** 追加 *****
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/iptv/sources",
		`{"url": "  `+upstream.URL+`/extra.m3u  "}`, "", "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("add status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources.UserSources) != 1 || sources.UserSources[0] != upstream.URL+"/extra.m3u" {
		t.Errorf("user sources = %v", sources.UserSources)
	}
	// ファイルに保存される
	saved, err := os.ReadFile(filepath.Join(server.paths.DataDir, iptv.UserSourcesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "extra.m3u") {
		t.Errorf("saved = %s", saved)
	}

	// 同じ URL を再登録しても重複しない
	doJSONRequest(t, handler, http.MethodPost, "/api/iptv/sources", `{"url": "`+upstream.URL+`/extra.m3u"}`, "", "application/json")
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/sources", "", "", "").Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources.UserSources) != 1 {
		t.Errorf("user sources = %v", sources.UserSources)
	}
	// 空の URL は 400
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/iptv/sources", `{"url": "  "}`, "", "application/json")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "The playlist URL must not be empty.") {
		t.Errorf("empty url = %d %s", recorder.Code, recorder.Body.String())
	}

	// ***** 削除 *****
	recorder = doJSONRequest(t, handler, http.MethodDelete, "/api/iptv/sources?url="+urlEncode(upstream.URL+"/extra.m3u"), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources.UserSources) != 0 {
		t.Errorf("user sources = %v", sources.UserSources)
	}
	// config.yaml のソースは削除できない
	doJSONRequest(t, handler, http.MethodDelete, "/api/iptv/sources?url="+urlEncode(upstream.URL+"/jp.m3u"), "", "", "")
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/sources", "", "", "").Body.Bytes(), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources.ConfigSources) != 1 || len(sources.UserSources) != 0 {
		t.Errorf("sources = %+v", sources)
	}
}

// TestIPTVTVUIAPI はテレビ視聴 UI 登録 API の挙動を検証する。
func TestIPTVTVUIAPI(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	// 登録対象の display_channel_id を取得する
	var channels iptvChannelsResponse
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels", "", "", "").Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	alpha := channels.Channels[0]
	beta := channels.Channels[1]

	// ***** 未ログイン (Cookie の匿名 ID) ごとに分離される *****
	recorder := doJSONRequest(t, handler, http.MethodPost, "/api/iptv/tvui",
		`{"display_channel_id": "`+alpha.DisplayChannelID+`"}`, "", "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var registered iptvTVUIChannelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Total != 1 || registered.Channels[0].DisplayChannelID != alpha.DisplayChannelID {
		t.Fatalf("registered = %+v", registered)
	}
	if registered.Channels[0].Name != alpha.Name || registered.Channels[0].CountryName == nil || *registered.Channels[0].CountryName != "Japan" {
		t.Errorf("registered channel = %+v", registered.Channels[0])
	}
	if registered.Channels[0].LogoURL == nil || !strings.HasSuffix(*registered.Channels[0].LogoURL, "/logo.png") {
		t.Errorf("logo = %v", registered.Channels[0].LogoURL)
	}

	// Cookie が発行され、同じ Cookie で再取得すると登録済みの状態が返る
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("anonymous cookie should be set")
	}
	cookieHeader := cookies[0].Name + "=" + cookies[0].Value

	request := httptest.NewRequest(http.MethodGet, "/api/iptv/tvui", nil)
	request.Header.Set("Cookie", cookieHeader)
	recorder2 := httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if err := json.Unmarshal(recorder2.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Total != 1 || registered.Channels[0].DisplayChannelID != alpha.DisplayChannelID {
		t.Errorf("tvui = %+v", registered)
	}

	// チャンネル一覧にも登録済みとして反映される
	request = httptest.NewRequest(http.MethodGet, "/api/iptv/channels", nil)
	request.Header.Set("Cookie", cookieHeader)
	recorder2 = httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	var channelsWithTVUI iptvChannelsResponse
	if err := json.Unmarshal(recorder2.Body.Bytes(), &channelsWithTVUI); err != nil {
		t.Fatal(err)
	}
	if !channelsWithTVUI.Channels[0].IsTVUIRegistered || channelsWithTVUI.Channels[1].IsTVUIRegistered {
		t.Errorf("is_tvui_registered = %v/%v",
			channelsWithTVUI.Channels[0].IsTVUIRegistered, channelsWithTVUI.Channels[1].IsTVUIRegistered)
	}

	// 別の Cookie では登録されていない
	request = httptest.NewRequest(http.MethodGet, "/api/iptv/tvui", nil)
	recorder2 = httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if err := json.Unmarshal(recorder2.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Total != 0 {
		t.Errorf("other user = %+v", registered)
	}

	// 登録解除できる
	request = httptest.NewRequest(http.MethodDelete, "/api/iptv/tvui?display_channel_id="+alpha.DisplayChannelID, nil)
	request.Header.Set("Cookie", cookieHeader)
	recorder2 = httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if err := json.Unmarshal(recorder2.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Total != 0 {
		t.Errorf("after unregister = %+v", registered)
	}

	// 存在しない display_channel_id は 404
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/iptv/tvui",
		`{"display_channel_id": "iptv999999999999999999"}`, "", "application/json")
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "Specified IPTV channel was not found.") {
		t.Errorf("unknown = %d %s", recorder.Code, recorder.Body.String())
	}

	// IPTV 機能が無効な場合は 400
	server.config.IPTV.Enabled = false
	recorder = doJSONRequest(t, handler, http.MethodPost, "/api/iptv/tvui",
		`{"display_channel_id": "`+beta.DisplayChannelID+`"}`, "", "application/json")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "IPTV is disabled.") {
		t.Errorf("disabled = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestIPTVProxyAPI はストリームプロキシ API の挙動を検証する。
func TestIPTVProxyAPI(t *testing.T) {
	server, upstream := setupIPTVTestServer(t)
	handler := server.Handler()

	// ***** 不正なスキームは 400 *****
	for _, target := range []string{"file:///etc/passwd", "ftp://example.com/a.m3u8", "not a url"} {
		recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(target), "", "", "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "Proxy URL must be started with http:// or https://.") {
			t.Errorf("%s: body = %s", target, recorder.Body.String())
		}
	}

	// ***** HLS プレイリストは内部の URI がプロキシ経由に書き換わる *****
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(upstream.URL+"/streams/beta.m3u8"), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("hls status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/vnd.apple.mpegurl" {
		t.Errorf("content type = %s", contentType)
	}
	if recorder.Header().Get("Cache-Control") != "no-store, no-cache, must-revalidate" {
		t.Errorf("cache control = %s", recorder.Header().Get("Cache-Control"))
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `URI="/api/iptv/proxy?url=`) {
		t.Errorf("body = %s", body)
	}
	// プロキシ URL のクエリは Python 版と同じパーセントエンコーディングになる
	if !strings.Contains(body, "/api/iptv/proxy?url="+url.QueryEscape(upstream.URL+"/streams/key.bin")) {
		t.Errorf("body = %s", body)
	}
	if !strings.Contains(body, "/api/iptv/proxy?url="+url.QueryEscape(upstream.URL+"/streams/segment1.ts")) {
		t.Errorf("body = %s", body)
	}

	// ***** それ以外はバイト列をそのまま転送する *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(upstream.URL+"/streams/delta.mp4"), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("mp4 status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "mp4body" {
		t.Errorf("body = %s", body)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cache control = %s", recorder.Header().Get("Cache-Control"))
	}

	// ***** 上流のエラーは 502 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(upstream.URL+"/missing"), "", "", "")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "HTTP Error 404") {
		t.Errorf("missing = %d %s", recorder.Code, recorder.Body.String())
	}

	// ***** 接続できない場合は 502 *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode("http://127.0.0.1:1/stream.ts"), "", "", "")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "Failed to fetch IPTV stream:") {
		t.Errorf("unreachable = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestIPTVLogoAPI はチャンネルロゴプロキシ API の挙動を検証する。
func TestIPTVLogoAPI(t *testing.T) {
	server, upstream := setupIPTVTestServer(t)
	handler := server.Handler()

	// ***** 不正なスキームは 400 *****
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/logo?url="+urlEncode("file:///etc/passwd"), "", "", "")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}

	// ***** 取得できた場合はそのまま返す *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/logo?url="+urlEncode(upstream.URL+"/logo.png"), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "logodata" {
		t.Errorf("body = %s", body)
	}
	// Content-Type のパラメーターは除去される
	if contentType := recorder.Header().Get("Content-Type"); contentType != "image/png" {
		t.Errorf("content type = %s", contentType)
	}
	if recorder.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("cache control = %s", recorder.Header().Get("Cache-Control"))
	}

	// ***** 取得できなかった場合は既定のロゴを返す *****
	for _, target := range []string{upstream.URL + "/missing", "http://127.0.0.1:1/logo.png"} {
		recorder = doJSONRequest(t, handler, http.MethodGet, "/api/iptv/logo?url="+urlEncode(target), "", "", "")
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status = %d", target, recorder.Code)
		}
		if body := recorder.Body.String(); body != "defaultlogo" {
			t.Errorf("%s: body = %s", target, body)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "image/png" {
			t.Errorf("%s: content type = %s", target, contentType)
		}
		if recorder.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("%s: cache control = %s", target, recorder.Header().Get("Cache-Control"))
		}
	}
}

// TestIPTVStreamHeadersForwarded はプレイリストで指定されたヘッダーが転送されることを検証する。
func TestIPTVStreamHeadersForwarded(t *testing.T) {
	server, paths := newTestServer(t, "")
	server.config.IPTV.Enabled = true

	receivedHeaders := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/countries.json":
			_, _ = w.Write([]byte("[]"))
		case "/jp.m3u":
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1 http-user-agent=\"ChannelAgent/2.0\" http-referrer=\"https://referrer.example.com/\",Header Channel\nhttp://" + r.Host + "/stream.ts\n"))
		case "/stream.ts":
			receivedHeaders["User-Agent"] = r.Header.Get("User-Agent")
			receivedHeaders["Referer"] = r.Header.Get("Referer")
			receivedHeaders["Accept"] = r.Header.Get("Accept")
			receivedHeaders["Accept-Language"] = r.Header.Get("Accept-Language")
			_, _ = w.Write([]byte("body"))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	server.config.IPTV.Sources = []string{upstream.URL + "/jp.m3u"}
	server.iptv = iptv.New(iptv.Options{
		Config:       server.config,
		DataDir:      paths.DataDir,
		Logger:       slog.New(slog.DiscardHandler),
		HTTPClient:   upstream.Client(),
		CountriesURL: upstream.URL + "/api/countries.json",
	})
	handler := server.Handler()

	// プロキシ URL はチャンネル一覧から取得する
	var channels iptvChannelsResponse
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels", "", "", "").Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	if len(channels.Channels) != 1 {
		t.Fatalf("channels = %d", len(channels.Channels))
	}

	recorder := doJSONRequest(t, handler, http.MethodGet, channels.Channels[0].StreamURL, "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if receivedHeaders["User-Agent"] != "ChannelAgent/2.0" {
		t.Errorf("user agent = %s", receivedHeaders["User-Agent"])
	}
	if receivedHeaders["Referer"] != "https://referrer.example.com/" {
		t.Errorf("referer = %s", receivedHeaders["Referer"])
	}
	if receivedHeaders["Accept"] != "*/*" || receivedHeaders["Accept-Language"] != "ja,en-US;q=0.9,en;q=0.8" {
		t.Errorf("accept headers = %v", receivedHeaders)
	}
}

// TestIPTVProxyRangeForwarded は Range ヘッダーが転送されることを検証する。
func TestIPTVProxyRangeForwarded(t *testing.T) {
	server, paths := newTestServer(t, "")
	server.config.IPTV.Enabled = true

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/countries.json" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", `"etag-value"`)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("data"))
			return
		}
		_, _ = w.Write([]byte("streambody"))
	}))
	t.Cleanup(upstream.Close)

	server.iptv = iptv.New(iptv.Options{
		Config:       server.config,
		DataDir:      paths.DataDir,
		Logger:       slog.New(slog.DiscardHandler),
		HTTPClient:   upstream.Client(),
		CountriesURL: upstream.URL + "/api/countries.json",
	})
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(upstream.URL+"/stream.ts")+"&range=bytes%3D0-3", "", "", "")
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "data" {
		t.Errorf("body = %s", body)
	}
	for name, expected := range map[string]string{
		"Content-Range":  "bytes 0-3/8",
		"Accept-Ranges":  "bytes",
		"ETag":           `"etag-value"`,
		"Content-Length": "4",
	} {
		if actual := recorder.Header().Get(name); actual != expected {
			t.Errorf("%s = %q, want %q", name, actual, expected)
		}
	}
}

// TestIPTVStreamBodyRelay は分割されたボディが正しく転送されることを検証する。
func TestIPTVStreamBodyRelay(t *testing.T) {
	server, paths := newTestServer(t, "")
	server.config.IPTV.Enabled = true

	chunks := []string{"first-", "second-", "third"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	server.iptv = iptv.New(iptv.Options{
		Config:     server.config,
		DataDir:    paths.DataDir,
		Logger:     slog.New(slog.DiscardHandler),
		HTTPClient: upstream.Client(),
	})
	handler := server.Handler()

	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/iptv/proxy?url="+urlEncode(upstream.URL+"/stream.ts"), "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "first-second-third" {
		t.Errorf("body = %s", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "video/mp2t" {
		t.Errorf("content type = %s", contentType)
	}
}

// TestIPTVChannelLogoAPI は IPTV 疑似チャンネルのロゴ API の挙動を検証する。
func TestIPTVChannelLogoAPI(t *testing.T) {
	server, _ := setupIPTVTestServer(t)
	handler := server.Handler()

	var channels iptvChannelsResponse
	if err := json.Unmarshal(doJSONRequest(t, handler, http.MethodGet, "/api/iptv/channels", "", "", "").Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}

	// ***** IPTV チャンネルのロゴはプロキシして返す *****
	alpha := channels.Channels[0]
	recorder := doJSONRequest(t, handler, http.MethodGet, "/api/channels/"+iptv.ChannelIDPrefix+alpha.DisplayChannelID+"/logo", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "logodata" {
		t.Errorf("body = %s", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "image/png" {
		t.Errorf("content type = %s", contentType)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != logoCacheControl {
		t.Errorf("cache control = %s", cacheControl)
	}
	etag := recorder.Header().Get("ETag")
	if etag != sha256HexString("logodata") {
		t.Errorf("etag = %s", etag)
	}

	// ***** If-None-Match が一致する場合は 304 *****
	request := httptest.NewRequest(http.MethodGet, "/api/channels/"+iptv.ChannelIDPrefix+alpha.DisplayChannelID+"/logo", nil)
	request.Header.Set("If-None-Match", etag)
	recorder2 := httptest.NewRecorder()
	handler.ServeHTTP(recorder2, request)
	if recorder2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", recorder2.Code)
	}

	// ***** ロゴが無い (または取得できない) チャンネルは既定のロゴ *****
	beta := channels.Channels[1]
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/"+iptv.ChannelIDPrefix+beta.DisplayChannelID+"/logo", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "defaultlogo" {
		t.Errorf("body = %s", body)
	}
	if etag := recorder.Header().Get("ETag"); etag != sha256HexString("iptv-default"+constants.Version) {
		t.Errorf("etag = %s", etag)
	}

	// ***** 存在しない IPTV チャンネルは既定のロゴ *****
	recorder = doJSONRequest(t, handler, http.MethodGet, "/api/channels/"+iptv.ChannelIDPrefix+"999999999999999999/logo", "", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != "defaultlogo" {
		t.Errorf("body = %s", body)
	}
}
