package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// VersionInformation は /api/version のレスポンス (server/app/schemas.py の VersionInformation 互換) 。
type VersionInformation struct {
	Version       string  `json:"version"`
	LatestVersion *string `json:"latest_version"`
	Environment   *string `json:"environment"`
	Backend       string  `json:"backend"`
	Encoder       string  `json:"encoder"`
}

// latestVersionCache は GitHub API から取得した KonomiTV の最新バージョンをキャッシュする。
// Python 版と同じく、GitHub API のレート制限 (無認証で60回/1時間) を考慮して10分間キャッシュする。
type latestVersionCache struct {
	mutex         sync.Mutex
	latestVersion *string
	updatedAt     time.Time
	httpClient    *http.Client
	logger        *slog.Logger
}

// latestVersionCacheTTL はキャッシュの有効期限 (Python 版の 60 * 10 秒と一致) 。
const latestVersionCacheTTL = 10 * time.Minute

func newLatestVersionCache(logger *slog.Logger) *latestVersionCache {
	return &latestVersionCache{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
	}
}

// get は最新バージョンを返す。キャッシュがなく、または期限切れの場合は GitHub API から再取得する。
// 取得に失敗した場合は直前の値 (ない場合は nil) を返す。
func (c *latestVersionCache) get(ctx context.Context) *string {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.latestVersion != nil && time.Since(c.updatedAt) <= latestVersionCacheTTL {
		return c.latestVersion
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/tsukumijima/KonomiTV/tags", nil)
	if err != nil {
		return c.latestVersion
	}
	request.Header.Set("User-Agent", "KonomiTV/"+constants.Version)
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.logger.Debug("failed to fetch latest version from GitHub", slog.Any("error", err))
		return c.latestVersion
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		c.logger.Debug("failed to fetch latest version from GitHub", slog.Int("status", response.StatusCode))
		return c.latestVersion
	}

	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&tags); err != nil || len(tags) == 0 {
		c.logger.Debug("failed to parse tags from GitHub", slog.Any("error", err))
		return c.latestVersion
	}

	// 先頭の v を取り除く
	version := tags[0].Name
	if len(version) > 0 && (version[0] == 'v' || version[0] == 'V') {
		version = version[1:]
	}
	c.latestVersion = &version
	c.updatedAt = time.Now()
	return c.latestVersion
}

// handleVersion は GET /api/version を処理する。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	result := VersionInformation{
		Version:       constants.Version,
		LatestVersion: s.latestVersion.get(r.Context()),
		Environment:   platformEnvironment(),
		Backend:       s.config.General.Backend,
		Encoder:       s.config.General.Encoder,
	}
	writeJSON(w, http.StatusOK, result)
}

// platformEnvironment はサーバーが稼働している環境を判定する (server/app/utils/__init__.py の GetPlatformEnvironment 互換) 。
func platformEnvironment() *string {
	set := func(value string) *string { return &value }
	switch runtime.GOOS {
	case "windows":
		return set("Windows")
	case "linux":
		if _, err := os.Stat("/.dockerenv"); err == nil {
			return set("Linux-Docker")
		}
		if runtime.GOARCH == "arm64" {
			return set("Linux-ARM")
		}
		return set("Linux")
	default:
		// KonomiTV は Windows と Linux のみサポートする
		return nil
	}
}
