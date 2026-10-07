package videostream

import (
	"strings"
	"testing"
)

// TestVirtualPlaylistPartialFinalSegment は Python と同じ最終端数と副音声 URI を検証する。
func TestVirtualPlaylistPartialFinalSegment(t *testing.T) {
	got := BuildVirtualPlaylist("session", "cache", 25, 13, "secondary")
	want := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.000000,\nsegment?session_id=session&sequence=0&cache_key=cache&audio=secondary\n" +
		"#EXTINF:6.000000,\nsegment?session_id=session&sequence=1&cache_key=cache&audio=secondary\n" +
		"#EXTINF:1.000000,\nsegment?session_id=session&sequence=2&cache_key=cache&audio=secondary\n#EXT-X-ENDLIST\n"
	if got != want {
		t.Fatalf("playlist mismatch:\n%s\nwant:\n%s", got, want)
	}
	if strings.Count(got, "#EXTINF:") != 3 {
		t.Fatal("unexpected segment count")
	}
}
