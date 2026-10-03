package jikkyo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// resolutionExpectation は Python 版 (JikkyoClient) から生成した期待値。
type resolutionExpectation struct {
	NetworkID     int     `json:"network_id"`
	ServiceID     int     `json:"service_id"`
	JikkyoID      *string `json:"jikkyo_id"`
	NicoChannelID *string `json:"nicochannel_id"`
}

// TestResolveMatchesPython は実況チャンネルの解決結果が Python 版と完全に一致することを検証する。
func TestResolveMatchesPython(t *testing.T) {
	// リポジトリ同梱の実データ (server/static/jikkyo_channels.json) を使う
	staticDir := filepath.Join("..", "..", "..", "server", "static")
	if _, err := os.Stat(filepath.Join(staticDir, "jikkyo_channels.json")); err != nil {
		t.Skipf("server/static/jikkyo_channels.json is not available: %v", err)
	}
	channelMap, err := LoadChannelMap(staticDir)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "jikkyo_resolution.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expectations []resolutionExpectation
	if err := json.Unmarshal(raw, &expectations); err != nil {
		t.Fatal(err)
	}
	if len(expectations) < 1000 {
		t.Fatalf("unexpectedly few expectations: %d", len(expectations))
	}

	for _, expectation := range expectations {
		jikkyoID, nicoChannelID, found := channelMap.Resolve(expectation.NetworkID, expectation.ServiceID)
		if expectation.JikkyoID == nil {
			if found {
				t.Errorf("Resolve(%d, %d) = %q, want not found", expectation.NetworkID, expectation.ServiceID, jikkyoID)
			}
			continue
		}
		if !found || jikkyoID != *expectation.JikkyoID {
			t.Errorf("Resolve(%d, %d) = %q, want %q", expectation.NetworkID, expectation.ServiceID, jikkyoID, *expectation.JikkyoID)
			continue
		}
		if expectation.NicoChannelID == nil {
			if nicoChannelID != "" {
				t.Errorf("Resolve(%d, %d) nicoChannelID = %q, want empty", expectation.NetworkID, expectation.ServiceID, nicoChannelID)
			}
			continue
		}
		if nicoChannelID != *expectation.NicoChannelID {
			t.Errorf("Resolve(%d, %d) nicoChannelID = %q, want %q", expectation.NetworkID, expectation.ServiceID, nicoChannelID, *expectation.NicoChannelID)
		}
	}
}

// TestWatchAndCommentSessionURL は NX-Jikkyo の WebSocket URL を検証する。
func TestWatchAndCommentSessionURL(t *testing.T) {
	if url := WatchSessionURL("jk1"); url != "https://nx-jikkyo.tsukumijima.net/api/v1/channels/jk1/ws/watch" {
		t.Errorf("watch url = %q", url)
	}
	if url := CommentSessionURL("jk101"); url != "https://nx-jikkyo.tsukumijima.net/api/v1/channels/jk101/ws/comment" {
		t.Errorf("comment url = %q", url)
	}
}

// TestLoadChannelMapError はファイルが存在しない場合にエラーになることを検証する。
func TestLoadChannelMapError(t *testing.T) {
	if _, err := LoadChannelMap(t.TempDir()); err == nil {
		t.Error("missing jikkyo_channels.json should be an error")
	}
	if _, _, found := (*ChannelMap)(nil).Resolve(32736, 1024); found {
		t.Error("nil ChannelMap should not resolve")
	}
}
