package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

type browserEnvironment struct {
	HTTPHeaders struct {
		AcceptLanguages []any `json:"accept_languages"`
	} `json:"http_headers"`
	UserAgentData struct {
		Platform        string `json:"platform"`
		PlatformVersion string `json:"platform_version"`
		Architecture    string `json:"architecture"`
		Model           string `json:"model"`
		Bitness         string `json:"bitness"`
		Mobile          bool   `json:"mobile"`
		WoW64           bool   `json:"wow64"`
	} `json:"user_agent_data"`
	NavigatorPlatform string `json:"navigator_platform"`
	Locale            string `json:"locale"`
	Timezone          string `json:"timezone"`
}

func fallbackBrowserEnvironment() browserEnvironment {
	var info browserEnvironment
	platform := map[string]string{"windows": "Windows", "darwin": "macOS", "linux": "Linux"}[runtime.GOOS]
	info.UserAgentData.Platform = platform
	info.UserAgentData.Architecture = "x86"
	if strings.HasPrefix(runtime.GOARCH, "arm") {
		info.UserAgentData.Architecture = "arm"
	}
	info.UserAgentData.Bitness = strconv.Itoa(strconv.IntSize)
	info.UserAgentData.PlatformVersion = browserPlatformVersion()
	info.NavigatorPlatform = map[string]string{"Windows": "Win32", "macOS": "MacIntel", "Linux": "Linux x86_64"}[platform]
	if platform == "Linux" {
		if info.UserAgentData.Architecture == "arm" {
			info.NavigatorPlatform = "Linux aarch64"
			if strconv.IntSize == 32 {
				info.NavigatorPlatform = "Linux armv7l"
			}
		} else if strconv.IntSize == 32 {
			info.NavigatorPlatform = "Linux i686"
		}
	}
	info.HTTPHeaders.AcceptLanguages = []any{"ja", "en"}
	info.Locale, info.Timezone = "ja", "Asia/Tokyo"
	return info
}

func buildBrowserUserAgent(agent, platform string) (string, bool) {
	osInfo := map[string]string{"macOS": "Macintosh; Intel Mac OS X 10_15_7", "Windows": "Windows NT 10.0; Win64; x64", "Linux": "X11; Linux x86_64"}
	replacement, supported := osInfo[platform]
	pattern := regexp.MustCompile(`^(Mozilla/5\.0 \()[^)]+(\))`)
	if !supported || !pattern.MatchString(agent) {
		return "", false
	}
	return pattern.ReplaceAllString(agent, "${1}"+replacement+"${2}"), true
}

func buildBrowserAcceptLanguage(raw any) (string, bool) {
	values, ok := raw.([]any)
	if !ok {
		return "", false
	}
	languages := []string{}
	for _, raw := range values {
		if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
			languages = append(languages, strings.TrimSpace(value))
		}
	}
	return strings.Join(languages, ","), len(languages) > 0
}

func (b *BrowserBackend) applyEnvironment(ctx context.Context, c *cdpConnection, session, encoded string) error {
	info := fallbackBrowserEnvironment()
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(encoded), &object) == nil && object != nil {
		// 保存情報が辞書ならその値を使い、欠落値をフォールバックの値で補完しない。
		info = browserEnvironment{}
		if err := json.Unmarshal([]byte(encoded), &info); err != nil {
			return err
		}
	}
	var agent string
	if err := c.evaluate(ctx, session, "navigator.userAgent", false, &agent); err != nil {
		return err
	}
	agent, supported := buildBrowserUserAgent(agent, info.UserAgentData.Platform)
	if !supported {
		return nil
	}
	platform := info.NavigatorPlatform
	if platform == "" {
		platform = map[string]string{"macOS": "MacIntel", "Windows": "Win32", "Linux": "Linux x86_64"}[info.UserAgentData.Platform]
	}
	metadata := map[string]any{"platform": info.UserAgentData.Platform, "platformVersion": info.UserAgentData.PlatformVersion,
		"architecture": info.UserAgentData.Architecture, "model": info.UserAgentData.Model, "mobile": info.UserAgentData.Mobile,
		"bitness": info.UserAgentData.Bitness, "wow64": info.UserAgentData.WoW64}
	params := map[string]any{"userAgent": agent, "platform": platform, "userAgentMetadata": metadata}
	if language, ok := buildBrowserAcceptLanguage(info.HTTPHeaders.AcceptLanguages); ok {
		params["acceptLanguage"] = language
	}
	if err := c.call(ctx, session, "Emulation.setUserAgentOverride", params, nil); err != nil {
		return err
	}
	if info.Locale != "" {
		if err := c.call(ctx, session, "Emulation.setLocaleOverride", map[string]any{"locale": info.Locale}, nil); err != nil {
			return err
		}
	}
	if info.Timezone != "" {
		return c.call(ctx, session, "Emulation.setTimezoneOverride", map[string]any{"timezoneId": info.Timezone}, nil)
	}
	return nil
}

func (b *BrowserBackend) restoreCookies(ctx context.Context, c *cdpConnection, account Account) error {
	text := account.PlainCookies
	if account.LoadCookies != nil {
		var err error
		text, err = account.LoadCookies()
		if err != nil {
			return err
		}
	} else if text == "" {
		text = account.AccessTokenSecret
		if strings.HasPrefix(text, "enc:") {
			return fmt.Errorf("encrypted Twitter cookies require a decryption callback")
		}
	}
	cookies, err := ParseNetscapeCookieFile(text)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return nil
	}
	params := make([]map[string]any, 0, len(cookies))
	for _, cookie := range cookies {
		item := map[string]any{"name": cookie.Name, "value": cookie.Value, "url": cookie.URL, "domain": cookie.Domain,
			"path": cookie.Path, "secure": cookie.Secure}
		if cookie.Expires != nil {
			item["expires"] = *cookie.Expires
		}
		params = append(params, item)
	}
	// 本家同様に CDP の Cookie 設定自体の失敗は記録してページのセットアップを続ける。
	if err := c.call(ctx, "", "Storage.setCookies", map[string]any{"cookies": params}, nil); err != nil {
		b.options.Logger.Warn("Failed to restore Twitter cookies", "error", err)
	}
	return nil
}

// SaveCookiesToNetscapeFormat はブラウザーの x.com Cookie を本家の Netscape 形式で返す。
func (b *BrowserBackend) SaveCookiesToNetscapeFormat(ctx context.Context) (string, error) {
	c, _, err := b.page()
	if err != nil {
		return "", err
	}
	var response struct {
		Cookies []struct {
			Name    string   `json:"name"`
			Value   string   `json:"value"`
			Domain  string   `json:"domain"`
			Path    string   `json:"path"`
			Secure  bool     `json:"secure"`
			Expires *float64 `json:"expires"`
		} `json:"cookies"`
	}
	if err := c.call(ctx, "", "Storage.getCookies", map[string]any{}, &response); err != nil {
		return "", err
	}
	lines := []string{"# Netscape HTTP Cookie File", "# https://curl.haxx.se/rfc/cookie_spec.html", "# This is a generated file! Do not edit.", ""}
	flag := func(value bool) string {
		if value {
			return "TRUE"
		}
		return "FALSE"
	}
	for _, cookie := range response.Cookies {
		if !strings.Contains(cookie.Domain, "x.com") {
			continue
		}
		var expires int64
		if cookie.Expires != nil {
			expires = int64(*cookie.Expires)
		}
		lines = append(lines, strings.Join([]string{cookie.Domain, flag(strings.HasPrefix(cookie.Domain, ".")), cookie.Path,
			flag(cookie.Secure), strconv.FormatInt(expires, 10), cookie.Name, cookie.Value}, "\t"))
	}
	return strings.Join(lines, "\n"), nil
}

func safeScreenshotName(value string, limit int) string {
	var result []rune
	for _, char := range value {
		if char < 32 || unicode.IsSpace(char) || strings.ContainsRune(`<>:"/\|?*`, char) {
			if len(result) == 0 || result[len(result)-1] != '_' {
				result = append(result, '_')
			}
		} else {
			result = append(result, char)
		}
	}
	trimmed := []rune(strings.Trim(string(result), "_"))
	return string(trimmed[:min(len(trimmed), limit)])
}

// CaptureDebugScreenshot はエラー画面を PNG へ保存し、本家と同じ7日の保持期間を適用する。
func (b *BrowserBackend) CaptureDebugScreenshot(ctx context.Context, reason string) (string, error) {
	c, session, err := b.page()
	if err != nil {
		return "", nil
	}
	var response struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, session, "Page.captureScreenshot", map[string]any{"format": "png"}, &response); err != nil {
		b.options.Logger.Warn("Failed to capture browser screenshot", "error", err)
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(response.Data)
	if err != nil {
		return "", nil
	}
	b.mu.RLock()
	name := b.account.ScreenName
	b.mu.RUnlock()
	directory := filepath.Join(b.options.DataDir, "twitter-debug-screenshots")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", nil
	}
	filename := safeScreenshotName(name, 80) + "_" + time.Now().In(constants.JST).Format("20060102_150405") + "_" + safeScreenshotName(reason, 80) + ".png"
	path := filepath.Join(directory, filename)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", nil
	}
	entries, _ := os.ReadDir(directory)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".png") {
			if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(directory, entry.Name()))
			}
		}
	}
	return path, nil
}
