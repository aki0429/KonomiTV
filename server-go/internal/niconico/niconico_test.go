package niconico

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture は testdata/generate_niconico_fixture.py が Python 版から生成した期待値。
type fixture struct {
	AuthURLCases []struct {
		Name           string  `json:"name"`
		Host           string  `json:"host"`
		Origin         *string `json:"origin"`
		Authorization  *string `json:"authorization"`
		ExpectedNetloc string  `json:"expected_netloc"`
		Expected       struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"expected"`
	} `json:"auth_url_cases"`
	OAuthCallbackResponseCases []struct {
		Detail      string `json:"detail"`
		RedirectTo  string `json:"redirect_to"`
		StatusCode  int    `json:"status_code"`
		ContentType string `json:"content_type"`
		BodyHTML    string `json:"body_html"`
	} `json:"oauth_callback_response_cases"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "niconico_fixture.json"))
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	var value fixture
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	return value
}

// TestBuildAuthorizationURLMatchesPython は認証 URL が Python 版と完全一致することを検証する。
func TestBuildAuthorizationURLMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.AuthURLCases) == 0 {
		t.Fatal("fixture has no auth_url_cases")
	}
	for _, testCase := range value.AuthURLCases {
		t.Run(testCase.Name, func(t *testing.T) {
			// Python 版 request.url.netloc は Host ヘッダーと同じ値になる
			if testCase.ExpectedNetloc != testCase.Host {
				t.Fatalf("expected_netloc (%q) != host (%q): fixture assumption broken", testCase.ExpectedNetloc, testCase.Host)
			}

			// ハンドラーと同じ手順でクライアント URL を組み立てる
			clientURL := ""
			if testCase.Origin != nil {
				clientURL = *testCase.Origin
			} else {
				clientURL = "https://" + testCase.Host
			}
			clientURL = strings.TrimRight(clientURL, "/") + "/"

			authorization := ""
			if testCase.Authorization != nil {
				authorization = *testCase.Authorization
			}
			_, userAccessToken := ParseAuthorizationSchemeParam(authorization)

			got := BuildAuthorizationURL(testCase.Host, clientURL, userAccessToken)
			if got != testCase.Expected.AuthorizationURL {
				t.Errorf("authorization_url mismatch\n got: %s\nwant: %s", got, testCase.Expected.AuthorizationURL)
			}
		})
	}
}

// TestRenderOAuthCallbackResponseMatchesPython はコールバック HTML が Python 版と完全一致することを検証する。
func TestRenderOAuthCallbackResponseMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.OAuthCallbackResponseCases) == 0 {
		t.Fatal("fixture has no oauth_callback_response_cases")
	}
	for index, testCase := range value.OAuthCallbackResponseCases {
		t.Run(testCase.Detail, func(t *testing.T) {
			got := RenderOAuthCallbackResponse(testCase.StatusCode, testCase.Detail, testCase.RedirectTo)
			if got != testCase.BodyHTML {
				t.Errorf("body mismatch\n got: %q\nwant: %q", got, testCase.BodyHTML)
			}
			// Content-Type は Python 版 HTMLResponse と同じ (ハンドラー側で設定する値)
			if testCase.ContentType != "text/html; charset=utf-8" {
				t.Fatalf("case %d: unexpected fixture content_type %q", index, testCase.ContentType)
			}
		})
	}
}

// TestParseAuthorizationSchemeParamMatchesFastAPI は Python 版
// get_authorization_scheme_param() と同じ結果になることを検証する。
func TestParseAuthorizationSchemeParamMatchesFastAPI(t *testing.T) {
	cases := []struct {
		header     string
		wantScheme string
		wantParam  string
	}{
		{"", "", ""},
		{"Bearer test-user-access-token", "Bearer", "test-user-access-token"},
		{"bearer token", "bearer", "token"},
		{"Bearer", "Bearer", ""},
		// Python 版は partition(" ") なので、スペース以降がすべてパラメーターになる
		{"Bearer token with spaces", "Bearer", "token with spaces"},
		{"Bearer ", "Bearer", ""},
	}
	for _, testCase := range cases {
		scheme, parameter := ParseAuthorizationSchemeParam(testCase.header)
		if scheme != testCase.wantScheme || parameter != testCase.wantParam {
			t.Errorf("ParseAuthorizationSchemeParam(%q) = (%q, %q), want (%q, %q)",
				testCase.header, scheme, parameter, testCase.wantScheme, testCase.wantParam)
		}
	}
}

// TestParseIDTokenSubject は id_token の sub 抽出が Python 版の
// int(jwt.get_unverified_claims(id_token).get('sub', 0)) と同じになることを検証する。
func TestParseIDTokenSubject(t *testing.T) {
	encode := func(payload string) string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
		body := base64.RawURLEncoding.EncodeToString([]byte(payload))
		return header + "." + body + ".signature"
	}

	cases := []struct {
		name    string
		token   string
		want    int64
		wantErr bool
	}{
		{"string sub", encode(`{"sub":"12345"}`), 12345, false},
		{"number sub", encode(`{"sub":67890}`), 67890, false},
		{"float sub", encode(`{"sub":123.9}`), 123, false},
		{"missing sub", encode(`{"aud":"4JTJdyBZLwMJwaI7"}`), 0, false},
		// Python 版は .get('sub', 0) が None を返し int(None) で TypeError になる
		{"null sub", encode(`{"sub":null}`), 0, true},
		{"non numeric sub", encode(`{"sub":"abc"}`), 0, true},
		{"not a jwt", "invalid", 0, true},
		{"broken payload", "a.!!!.c", 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseIDTokenSubject(testCase.token)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("ParseIDTokenSubject(%q) = %d, want error", testCase.token, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIDTokenSubject(%q) returned error: %v", testCase.token, err)
			}
			if got != testCase.want {
				t.Errorf("ParseIDTokenSubject(%q) = %d, want %d", testCase.token, got, testCase.want)
			}
		})
	}
}
