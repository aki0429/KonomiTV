package api

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// この回帰は秘密を含む error graph の全種類を最終ログ境界で検証する。
func TestPG1ErrorGraphPrivacy(t *testing.T) {
	const target = "http://graph-user:graph-password@graph-host.invalid:7010/graph-path?token=graph-query#graph-fragment"
	leaf := errors.New("synthetic safe dial failure")
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: target, Err: err} }
	cases := []struct {
		name string
		err  error
	}{
		{"safe", leaf},
		{"url", wrap(leaf)},
		{"nested", wrap(wrap(leaf))},
		{"wrapped nested", fmt.Errorf("safe prefix: %w", wrap(wrap(leaf)))},
		{"raw wrapper", fmt.Errorf("upstream %s: %w", target, leaf)},
		{"joined raw", errors.Join(leaf, errors.New(target))},
		{"raw leaf", wrap(errors.New("dial failed for " + target))},
		{"components only", errors.New("graph-user graph-password graph-path graph-query graph-fragment")},
		{"encoded", errors.New(url.QueryEscape(target))},
		{"cycle", &pg1CyclicError{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs, response := servePG1ProxyError(t, target, pg1FailingTransport{err: tc.err})
			if response.Code != 502 || response.Body.String() != "{\"detail\":\"Bad Gateway\"}\n" {
				t.Fatal("unclean response")
			}
			for _, secret := range []string{"graph-user", "graph-password", "graph-path", "graph-query", "graph-fragment"} {
				if strings.Contains(logs, secret) {
					t.Errorf("secret leaked: %s", secret)
				}
			}
			if !strings.Contains(logs, "path=/api/unimplemented") || !strings.Contains(logs, "graph-host.invalid:7010") {
				t.Fatal("safe request identity lost")
			}
			if tc.name == "safe" && !strings.Contains(logs, "synthetic safe dial failure") {
				t.Fatal("safe diagnostic lost")
			}
		})
	}
}

// 独自エラーの Error/Unwrap を呼ばず、安全な型分類で終了する必要がある。
type pg1CyclicError struct{}

func (e *pg1CyclicError) Error() string { return "opaque cycle error" }
func (e *pg1CyclicError) Unwrap() error { return e }
