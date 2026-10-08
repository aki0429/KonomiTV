package api

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// fresh review: 短い秘密が長い秘密の接頭辞・前方重なりになる場合も、長い秘密の残りを漏らさない。
func TestPG1ReviewOverlappingSecrets(t *testing.T) {
	cases := []struct{ name, target, diag, leak string }{
		{"username prefix of password", "http://abc:abcdefSECRET@review-host.invalid:7010/", "auth abcdefSECRET failed", "SECRET"},
		{"earlier overlap", "http://review-host.invalid:7010/abx/xyzSECRET", "open abxyzSECRET failed", "SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got := safeProxyError(errors.New(tc.diag), target)
			if strings.Contains(got, tc.leak) {
				t.Errorf("overlapping secret leaked: %q", got)
			}
			if !strings.Contains(got, "failed") {
				t.Errorf("safe diagnostic lost: %q", got)
			}
		})
	}
}

// fresh review: 巨大診断でも有限時間で固定分類へ閉じ、内容を保持しない。
func TestPG1ReviewOversizedDiagnostic(t *testing.T) {
	target, _ := url.Parse("http://a:b@review-host.invalid:7010/c")
	got := safeProxyError(errors.New(strings.Repeat("x", 1<<20)), target)
	if strings.Contains(got, "xxxx") || len(got) > 256 {
		t.Errorf("oversized diagnostic retained: %d bytes", len(got))
	}
}
