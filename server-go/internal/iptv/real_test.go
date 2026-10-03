package iptv

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// realExpectation は Python 版で生成した期待値。
type realExpectation struct {
	Total     int `json:"total"`
	Countries []Country
	Groups    []Group
	Channels  []struct {
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		URL              string  `json:"url"`
		Country          *string `json:"country"`
		Group            *string `json:"group"`
		DisplayChannelID string  `json:"display_channel_id"`
		ProxyURL         string  `json:"proxy_url"`
		StreamType       string  `json:"stream_type"`
		HeaderUserAgent  *string `json:"header_user_agent"`
		HeaderReferrer   *string `json:"header_referrer"`
	} `json:"channels"`
}

// TestRealPlaylistParity は実データ (iptv-org のプレイリスト) を Python 版と比較する。
//
// tools/generate_iptv_real_fixture.py で生成したフィクスチャが必要。
// フィクスチャのディレクトリは環境変数 KONOMITV_IPTV_REAL_FIXTURE で指定できる。
func TestRealPlaylistParity(t *testing.T) {
	// Countries フィールドは JSON のキー名に合わせて個別に読み込む
	fixtureDir := os.Getenv("KONOMITV_IPTV_REAL_FIXTURE")
	if fixtureDir == "" {
		fixtureDir = filepath.Join("..", "..", "tmp_real_iptv")
	}
	data, err := os.ReadFile(filepath.Join(fixtureDir, "expected.json"))
	if err != nil {
		t.Skipf("実データのフィクスチャがありません (%s)。tools/generate_iptv_real_fixture.py で生成できます。", fixtureDir)
	}
	var raw struct {
		Total     int               `json:"total"`
		Countries []Country         `json:"countries"`
		Groups    []Group           `json:"groups"`
		Channels  []json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	playlistPath := filepath.Join(fixtureDir, "index.m3u")
	countriesData, err := os.ReadFile(filepath.Join(fixtureDir, "countries.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 国一覧 API の代わりにローカルファイルを配信するサーバーを用意する
	countriesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(countriesData)
	}))
	defer countriesServer.Close()

	cfg := testConfig()
	cfg.IPTV.Sources = []string{playlistPath}
	manager := New(Options{
		Config:       cfg,
		DataDir:      t.TempDir(),
		Logger:       slog.New(slog.DiscardHandler),
		HTTPClient:   countriesServer.Client(),
		CountriesURL: countriesServer.URL + "/countries.json",
	})
	channels := manager.Refresh(context.Background(), true)
	if len(channels) != raw.Total {
		t.Fatalf("total = %d, want %d", len(channels), raw.Total)
	}

	// 国・グループの集計 (並び順も含めて比較する)
	if actual := manager.Countries(nil); !reflect.DeepEqual(actual, raw.Countries) {
		for index := range min(len(actual), len(raw.Countries)) {
			if actual[index] != raw.Countries[index] {
				t.Errorf("countries[%d] = %+v, want %+v", index, actual[index], raw.Countries[index])
				break
			}
		}
		t.Errorf("countries = %d entries, want %d entries", len(actual), len(raw.Countries))
	}
	if actual := manager.Groups(nil); !reflect.DeepEqual(actual, raw.Groups) {
		for index := range min(len(actual), len(raw.Groups)) {
			if actual[index] != raw.Groups[index] {
				t.Errorf("groups[%d] = %+v, want %+v", index, actual[index], raw.Groups[index])
				break
			}
		}
		t.Errorf("groups = %d entries, want %d entries", len(actual), len(raw.Groups))
	}

	// 全チャンネルの内容と順序を比較する
	mismatches := 0
	for index, expectedRaw := range raw.Channels {
		var expected struct {
			ID               string  `json:"id"`
			Name             string  `json:"name"`
			URL              string  `json:"url"`
			Country          *string `json:"country"`
			Group            *string `json:"group"`
			DisplayChannelID string  `json:"display_channel_id"`
			ProxyURL         string  `json:"proxy_url"`
			StreamType       string  `json:"stream_type"`
			HeaderUserAgent  *string `json:"header_user_agent"`
			HeaderReferrer   *string `json:"header_referrer"`
		}
		if err := json.Unmarshal(expectedRaw, &expected); err != nil {
			t.Fatal(err)
		}
		channel := channels[index]
		problems := []string{}
		if channel.ID != expected.ID {
			problems = append(problems, "id")
		}
		if channel.Name != expected.Name {
			problems = append(problems, "name")
		}
		if channel.URL != expected.URL {
			problems = append(problems, "url")
		}
		if !reflect.DeepEqual(channel.Country, expected.Country) {
			problems = append(problems, "country")
		}
		if !reflect.DeepEqual(channel.Group, expected.Group) {
			problems = append(problems, "group")
		}
		if BuildDisplayChannelID(channel.URL) != expected.DisplayChannelID {
			problems = append(problems, "display_channel_id")
		}
		if BuildProxyURL(channel.URL) != expected.ProxyURL {
			problems = append(problems, "proxy_url")
		}
		if DetectStreamType(channel.URL) != expected.StreamType {
			problems = append(problems, "stream_type")
		}
		if !reflect.DeepEqual(channel.UserAgent, expected.HeaderUserAgent) {
			problems = append(problems, "user_agent")
		}
		if !reflect.DeepEqual(channel.Referrer, expected.HeaderReferrer) {
			problems = append(problems, "referrer")
		}
		if len(problems) > 0 {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("channel %d (%s): mismatch in %v\n got %+v\nwant %+v", index, channel.Name, problems, channel, expected)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("mismatches = %d / %d", mismatches, len(raw.Channels))
	}

	// ストリーム用ヘッダーの記録も Python 版と一致することを確認する
	withHeaders := 0
	for _, channel := range channels {
		headers := manager.StreamHeaders(channel.URL)
		if channel.UserAgent != nil {
			if headers["User-Agent"] != *channel.UserAgent {
				t.Errorf("user agent header = %v", headers)
			}
			withHeaders++
		}
		if channel.Referrer != nil && headers["Referer"] != *channel.Referrer {
			t.Errorf("referer header = %v", headers)
		}
	}
	t.Logf("channels with custom headers = %d", withHeaders)
}
