package twitter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// applyBrowserEnvironment は Cookie 採取時の OS/言語情報を Chromium に反映する。
func applyBrowserEnvironment(ctx context.Context, browser BrowserSession, encoded string) error {
	var version struct {
		UserAgent string `json:"userAgent"`
	}
	if err := browser.Call(ctx, "Browser.getVersion", nil, &version); err != nil {
		return err
	}
	userAgent := strings.ReplaceAll(version.UserAgent, "HeadlessChrome/", "Chrome/")
	params := map[string]any{"userAgent": userAgent}
	if encoded != "" && encoded != "null" {
		var info struct {
			Headers struct {
				UserAgent string `json:"user_agent"`
				Language  string `json:"accept_language"`
			} `json:"http_headers"`
			Platform string         `json:"navigator_platform"`
			Locale   string         `json:"locale"`
			Timezone string         `json:"timezone"`
			Data     map[string]any `json:"user_agent_data"`
		}
		if err := json.Unmarshal([]byte(encoded), &info); err != nil {
			return errors.New("invalid browser environment")
		}
		// Chromium 自身のバージョンを保持し、採取元 OS 部分だけを移す。
		start, end := strings.Index(userAgent, "("), strings.Index(userAgent, ")")
		sourceStart, sourceEnd := strings.Index(info.Headers.UserAgent, "("), strings.Index(info.Headers.UserAgent, ")")
		if start >= 0 && end > start && sourceStart >= 0 && sourceEnd > sourceStart {
			params["userAgent"] = userAgent[:start] + info.Headers.UserAgent[sourceStart:sourceEnd+1] + userAgent[end+1:]
		}
		if info.Headers.Language != "" {
			params["acceptLanguage"] = info.Headers.Language
		}
		if info.Platform != "" {
			params["platform"] = info.Platform
		}
		if len(info.Data) > 0 {
			metadata := map[string]any{}
			for _, pair := range [][2]string{{"platform", "platform"}, {"platform_version", "platformVersion"}, {"architecture", "architecture"}, {"bitness", "bitness"}, {"mobile", "mobile"}, {"model", "model"}, {"wow64", "wow64"}} {
				if value, ok := info.Data[pair[0]]; ok {
					metadata[pair[1]] = value
				}
			}
			params["userAgentMetadata"] = metadata
		}
		if info.Locale != "" {
			if err := browser.Call(ctx, "Emulation.setLocaleOverride", map[string]any{"locale": info.Locale}, nil); err != nil {
				return err
			}
		}
		if info.Timezone != "" {
			if err := browser.Call(ctx, "Emulation.setTimezoneOverride", map[string]any{"timezoneId": info.Timezone}, nil); err != nil {
				return err
			}
		}
	}
	return browser.Call(ctx, "Emulation.setUserAgentOverride", params, nil)
}

// saveBrowserScreenshot は明示指定された保存先だけを使い、既存ファイルを上書きしない。
func saveBrowserScreenshot(ctx context.Context, browser BrowserSession, directory string) (string, error) {
	var result struct {
		Data string `json:"data"`
	}
	if err := browser.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &result); err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, "twitter-*.png")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return filepath.Abs(name)
}
