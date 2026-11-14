package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type analyticsFilter struct {
	RegexPattern string   `json:"regex_pattern"`
	RequestTypes []string `json:"request_types"`
}

var analyticsUpdateLock = make(chan struct{}, 1)

var analyticsFilterURLs = []string{
	"https://raw.githubusercontent.com/easylist/easylist/master/easyprivacy/easyprivacy_specific.txt",
	"https://raw.githubusercontent.com/uBlockOrigin/uAssets/master/filters/filters.txt",
	"https://raw.githubusercontent.com/uBlockOrigin/uAssets/master/filters/privacy.txt",
	"https://raw.githubusercontent.com/uBlockOrigin/uAssets/master/filters/quick-fixes.txt",
	"https://raw.githubusercontent.com/uBlockOrigin/uAssets/master/filters/unbreak.txt",
	"https://raw.githubusercontent.com/uBlockOrigin/uAssets/master/filters/ubol-filters.txt",
}

func pythonRegexEscape(value string) string {
	var result strings.Builder
	for _, char := range value {
		if strings.ContainsRune("()[]{}?*+-|^$\\.&~# \t\n\r\v\f", char) {
			result.WriteByte('\\')
		}
		result.WriteRune(char)
	}
	return result.String()
}

func extractAnalyticsFilters(contents []string) ([]analyticsFilter, error) {
	hostPattern := regexp.MustCompile(`(?i)^\|\|((?:[a-z0-9-]+\.)*(?:twitter\.com|x\.com))(.+)$`)
	supported := map[string]bool{"xhr": true, "xmlhttprequest": true, "ping": true, "1p": true, "first-party": true,
		"3p": true, "third-party": true, "important": true, "redirect=nooptext": true}
	filters := map[string]analyticsFilter{}
	for _, content := range contents {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			parts := strings.SplitN(line, "$", 2)
			match := hostPattern.FindStringSubmatch(parts[0])
			if match == nil {
				continue
			}
			host, pattern := strings.ToLower(match[1]), match[2]
			requestTypes := []string{"XHR", "Ping"}
			if len(parts) == 2 {
				options, valid := map[string]bool{}, true
				for _, option := range strings.Split(strings.ToLower(parts[1]), ",") {
					options[option] = true
					valid = valid && supported[option]
				}
				thirdParty := host == "twitter.com" || strings.HasSuffix(host, ".twitter.com")
				if !valid || ((options["3p"] || options["third-party"]) && !thirdParty) || ((options["1p"] || options["first-party"]) && thirdParty) {
					continue
				}
				requestTypes = nil
				if options["xhr"] || options["xmlhttprequest"] {
					requestTypes = append(requestTypes, "XHR")
				}
				if options["ping"] {
					requestTypes = append(requestTypes, "Ping")
				}
				if len(requestTypes) == 0 {
					requestTypes = []string{"XHR", "Ping"}
				}
			}
			if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "^*/") {
				continue
			}
			anchor := ""
			if strings.HasSuffix(pattern, "|") {
				pattern, anchor = strings.TrimSuffix(pattern, "|"), "$"
			}
			if strings.Contains(pattern, "|") {
				continue
			}
			pattern = strings.ReplaceAll(strings.ReplaceAll(pythonRegexEscape(pattern), `\*`, ".*"), `\^`, `(?:[^A-Za-z0-9_.%-]|$)`)
			pattern = `^https?://(?:[^/?#]+\.)*` + pythonRegexEscape(host) + `(?::\d+)?` + pattern + anchor
			if utf8.RuneCountInString(pattern) > 512 {
				return nil, fmt.Errorf("analytics filter exceeds 512 characters")
			}
			filters[pattern+"\x00"+strings.Join(requestTypes, "\x00")] = analyticsFilter{RegexPattern: pattern, RequestTypes: requestTypes}
		}
	}
	keys := make([]string, 0, len(filters))
	for key := range filters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]analyticsFilter, 0, len(keys))
	for _, key := range keys {
		result = append(result, filters[key])
	}
	return result, nil
}

func validAnalyticsCache(filters []analyticsFilter) bool {
	if len(filters) > 256 {
		return false
	}
	for _, filter := range filters {
		if utf8.RuneCountInString(filter.RegexPattern) > 512 || len(filter.RequestTypes) < 1 || len(filter.RequestTypes) > 2 {
			return false
		}
		for _, kind := range filter.RequestTypes {
			if kind != "XHR" && kind != "Ping" {
				return false
			}
		}
	}
	return true
}

func (b *BrowserBackend) analyticsFilters(ctx context.Context) []analyticsFilter {
	select {
	case analyticsUpdateLock <- struct{}{}:
		defer func() { <-analyticsUpdateLock }()
	case <-ctx.Done():
		return []analyticsFilter{}
	}
	urls := b.options.filterURLs
	if urls == nil {
		urls = analyticsFilterURLs
	}
	cachePath := filepath.Join(b.options.DataDir, "twitter-analytics-block-filters.json")
	filters, err := downloadAnalyticsFilters(ctx, urls)
	if err == nil && len(filters) > 0 && validAnalyticsCache(filters) {
		data, _ := json.Marshal(map[string]any{"filters": filters})
		if err := os.MkdirAll(b.options.DataDir, 0700); err == nil {
			if err := os.WriteFile(cachePath+".tmp", data, 0600); err == nil {
				if err := os.Rename(cachePath+".tmp", cachePath); err == nil {
					return filters
				}
			}
		}
	}
	// 完全な更新ができなかった場合は既存キャッシュを保持し、本家の JS 組み込みルールへも戻れる。
	var cache struct {
		Filters []struct {
			Pattern *string  `json:"regex_pattern"`
			Types   []string `json:"request_types"`
		} `json:"filters"`
	}
	if data, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(data, &cache) == nil && cache.Filters != nil {
		filters := make([]analyticsFilter, 0, len(cache.Filters))
		for _, item := range cache.Filters {
			if item.Pattern == nil {
				return []analyticsFilter{}
			}
			filters = append(filters, analyticsFilter{RegexPattern: *item.Pattern, RequestTypes: item.Types})
		}
		if validAnalyticsCache(filters) {
			return filters
		}
	}
	return []analyticsFilter{}
}

func downloadAnalyticsFilters(ctx context.Context, urls []string) ([]analyticsFilter, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	contents, errors := make([]string, len(urls)), make([]error, len(urls))
	var group sync.WaitGroup
	for index, url := range urls {
		group.Go(func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				errors[index] = err
				return
			}
			request.Header.Set("User-Agent", "KonomiTV TwitterScrapeBrowser")
			response, err := client.Do(request)
			if err != nil {
				errors[index] = err
				return
			}
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
			if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(data) == 0 || len(data) > 2*1024*1024 || !utf8.Valid(data) {
				errors[index] = fmt.Errorf("invalid analytics list response: status=%d size=%d error=%v", response.StatusCode, len(data), err)
				return
			}
			contents[index] = strings.TrimPrefix(string(data), "\ufeff")
		})
	}
	group.Wait()
	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}
	return extractAnalyticsFilters(contents)
}
