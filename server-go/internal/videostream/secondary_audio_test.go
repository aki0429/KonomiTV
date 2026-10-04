package videostream

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractSecondaryAudio は副音声抽出の結果が Python 版と完全に一致することを検証する。
// フィクスチャは tools/generate_videostream_audio_fixture.py で生成する。
func TestExtractSecondaryAudio(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "secondary_audio_input.ts"))
	if err != nil {
		t.Skipf("フィクスチャがありません: %v", err)
	}
	expected, err := os.ReadFile(filepath.Join("testdata", "secondary_audio_expected.ts"))
	if err != nil {
		t.Skipf("フィクスチャがありません: %v", err)
	}

	actual, err := ExtractSecondaryAudio(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("抽出結果が Python 版と一致しません (len = %d, want %d)", len(actual), len(expected))
	}
}

// TestExtractSecondaryAudioErrors は副音声抽出のエラー処理を検証する。
func TestExtractSecondaryAudioErrors(t *testing.T) {
	// 188 バイトの倍数でないデータはエラーになる
	if _, err := ExtractSecondaryAudio([]byte{0x47, 0x00, 0x00}); err == nil {
		t.Error("188 バイトの倍数でないデータでエラーになりません")
	}
	// 同期バイトが不正なデータはエラーになる
	invalid := make([]byte, PacketSize*2)
	if _, err := ExtractSecondaryAudio(invalid); err == nil {
		t.Error("同期バイトが不正なデータでエラーになりません")
	}
}
