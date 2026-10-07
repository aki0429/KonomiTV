package twitter

import (
	"testing"
)

// TestParseNetscapeCookieFile は Netscape 形式 Cookie のパースを検証する。
func TestParseNetscapeCookieFile(t *testing.T) {
	content := "# Netscape HTTP Cookie File\n" +
		"\n" +
		".x.com\tTRUE\t/\tTRUE\t1759800000\tauth_token\tdummy-auth-token\n" +
		"x.com\tTRUE\t/\tFALSE\t0\tct0\tdummy-ct0\n" +
		"broken-line\twith-few-fields\n"

	cookies, err := ParseNetscapeCookieFile(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies, got %d (%+v)", len(cookies), cookies)
	}
	first := cookies[0]
	if first.Name != "auth_token" || first.Value != "dummy-auth-token" {
		t.Errorf("unexpected first cookie: %+v", first)
	}
	if first.Domain != ".x.com" || first.Path != "/" || !first.Secure {
		t.Errorf("unexpected first cookie attributes: %+v", first)
	}
	if first.Expires == nil || *first.Expires != 1759800000 {
		t.Errorf("unexpected first cookie expiry: %v", first.Expires)
	}
	if first.URL != "https://x.com" {
		t.Errorf("unexpected first cookie url: %q", first.URL)
	}
	second := cookies[1]
	if second.Secure || second.Expires != nil {
		t.Errorf("unexpected second cookie: %+v", second)
	}
	if second.URL != "http://x.com" {
		t.Errorf("unexpected second cookie url: %q", second.URL)
	}

	if !HasTwitterCookie(cookies) {
		t.Error("expected x.com cookie to be detected")
	}
	if HasTwitterCookie([]NetscapeCookie{{Domain: "example.com"}}) {
		t.Error("example.com should not be detected as a Twitter cookie")
	}
}

// TestParseNetscapeCookieFileInvalidExpiry は expires が数値でない場合にエラーになることを確認する。
func TestParseNetscapeCookieFileInvalidExpiry(t *testing.T) {
	content := ".x.com\tTRUE\t/\tTRUE\tnot-a-number\tauth_token\tvalue\n"
	if _, err := ParseNetscapeCookieFile(content); err == nil {
		t.Fatal("expected an error for an invalid expiration")
	}
}

// TestPythonJSONOrdering は OrderedMap が挿入順で Python 互換の JSON を出力することを確認する。
func TestPythonJSONOrdering(t *testing.T) {
	ordered := NewOrderedMap()
	ordered.Set("count", 20)
	ordered.Set("cursor", "CURSOR")
	ordered.Set("enableRanking", false)
	nested := NewOrderedMap()
	nested.Set("isDelegate", false)
	ordered.Set("fieldToggles", nested)
	ordered.Set("seenTweetIds", []any{"1", "2"})

	encoded, err := PythonJSON(ordered)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"count": 20, "cursor": "CURSOR", "enableRanking": false, "fieldToggles": {"isDelegate": false}, "seenTweetIds": ["1", "2"]}`
	if string(encoded) != expected {
		t.Fatalf("PythonJSON mismatch\n got: %s\nwant: %s", encoded, expected)
	}
}

// TestPythonJSONEscaping は Python の json.dumps と同じエスケープ規則かを確認する。
func TestPythonJSONEscaping(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"<script>", `"<script>"`},             // Go の encoding/json は < をエスケープするが Python はしない
		{"日本語", `"日本語"`},                       // ensure_ascii=False
		{"a\tb\nc", `"a\tb\nc"`},               // 短縮表記
		{"\u0001", `"\u0001"`},                 // 制御文字は \uXXXX
		{"a\"b\\c", `"a\"b\\c"`},               // 引用符とバックスラッシュ
		{"https://t.co/x", `"https://t.co/x"`}, // スラッシュはエスケープしない
	}
	for _, testCase := range cases {
		encoded, err := PythonJSON(testCase.input)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != testCase.expected {
			t.Errorf("PythonJSON(%q) = %s, want %s", testCase.input, encoded, testCase.expected)
		}
	}
}

// TestRoundToThreeDecimals は round(value, 3) 相当が小数第3位へ丸めることを確認する。
//
// 注: 末尾がちょうど .5 になる二進タイの挙動は CPython の round() と厳密には一致しない
// (例: リテラル 0.1235 は CPython では 0.123、この実装では 0.124) 。この値はツイート送信
// 前のランダムな待機秒数 (random.uniform の丸め) にのみ使われ API 出力には現れないため、
// 完全一致は要求しない。
func TestRoundToThreeDecimals(t *testing.T) {
	cases := []struct {
		input    float64
		expected float64
	}{
		{0.1234, 0.123},
		{0.6, 0.6},
		{4.123456789, 4.123},
		{1.9996, 2.0},
		{2.5, 2.5},
	}
	for _, testCase := range cases {
		if actual := roundToThreeDecimals(testCase.input); actual != testCase.expected {
			t.Errorf("roundToThreeDecimals(%v) = %v, want %v", testCase.input, actual, testCase.expected)
		}
	}
}

// TestParseAcceptLanguageHeader は Accept-Language の変換を確認する。
func TestParseAcceptLanguageHeader(t *testing.T) {
	if tags := ParseAcceptLanguageHeader(nil); len(tags) != 0 {
		t.Errorf("nil should return an empty list, got %v", tags)
	}
	header := "ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7"
	tags := ParseAcceptLanguageHeader(&header)
	expected := []string{"ja-JP", "ja", "en-US", "en"}
	if len(tags) != len(expected) {
		t.Fatalf("tags = %v, want %v", tags, expected)
	}
	for index := range expected {
		if tags[index] != expected[index] {
			t.Errorf("tags[%d] = %q, want %q", index, tags[index], expected[index])
		}
	}
}
