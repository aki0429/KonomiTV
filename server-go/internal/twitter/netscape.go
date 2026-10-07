package twitter

import (
	"fmt"
	"strconv"
	"strings"
)

// NetscapeCookie は Netscape 形式 Cookie ファイルの 1 行
// (TwitterScrapeBrowser.parseNetscapeCookieFile が返す cdp.network.CookieParam 相当) 。
type NetscapeCookie struct {
	Name    string
	Value   string
	URL     string
	Domain  string
	Path    string
	Secure  bool
	Expires *int64
}

// ParseNetscapeCookieFile は Netscape 形式の Cookie 文字列をパースする
// (TwitterScrapeBrowser.parseNetscapeCookieFile 相当) 。
//
// 形式: domain, flag, path, secure, expiration, name, value (タブ区切り) 。
// コメント行 (# で始まる行) と空行、7 フィールド未満の行は無視する。
func ParseNetscapeCookieFile(cookiesContent string) ([]NetscapeCookie, error) {
	cookies := []NetscapeCookie{}
	for _, rawLine := range strings.Split(cookiesContent, "\n") {
		line := strings.TrimSpace(rawLine)
		// コメント行や空行をスキップ
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Netscape フォーマット: domain, flag, path, secure, expiration, name, value
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		domain := parts[0]
		// flag (parts[1]) は使用しない
		path := parts[2]
		secure := parts[3] == "TRUE"
		expiresText := parts[4]
		name := parts[5]
		value := parts[6]

		var expires *int64
		if expiresText != "" && expiresText != "0" {
			parsed, err := strconv.ParseInt(expiresText, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid expiration in cookies.txt: %q", expiresText)
			}
			expires = &parsed
		}

		// domain から URL を構築 (ドットで始まる場合は除去) 、secure フラグに応じてプロトコルを選択
		domainForURL := strings.TrimLeft(domain, ".")
		protocol := "http"
		if secure {
			protocol = "https"
		}

		cookies = append(cookies, NetscapeCookie{
			Name:    name,
			Value:   value,
			URL:     protocol + "://" + domainForURL,
			Domain:  domain,
			Path:    path,
			Secure:  secure,
			Expires: expires,
		})
	}
	return cookies, nil
}

// HasTwitterCookie はパース済み Cookie に x.com ドメインのものが含まれるかを返す
// (TwitterRouter.TwitterCookieAuthAPI の twitter_cookies チェック相当) 。
func HasTwitterCookie(cookies []NetscapeCookie) bool {
	for _, cookie := range cookies {
		if strings.Contains(cookie.Domain, "x.com") {
			return true
		}
	}
	return false
}
