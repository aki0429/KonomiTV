package iptv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
)

// Options は Manager の生成に必要な依存関係。
type Options struct {
	// Config はサーバー設定 (iptv セクションを参照する) 。
	Config *config.Config
	// DataDir は server/data ディレクトリのパス (iptv_sources.json などの保存先) 。
	DataDir string
	// Logger はログ出力先。
	Logger *slog.Logger
	// HTTPClient はプレイリストやストリームの取得に利用する HTTP クライアント。
	// nil の場合は Config のタイムアウト設定から生成する (テストでは差し替える) 。
	HTTPClient *http.Client
	// CountriesURL は国一覧 API の URL (テストで差し替えるため) 。
	CountriesURL string
}

// state は更新処理で差し替えられるキャッシュのスナップショット。
type state struct {
	channels        []*Channel
	countriesByCode map[string]CountryInfo
	byDisplayID     map[string]*Channel
	headersByURL    map[string]map[string]string
	headersByHost   map[string]map[string]string
	sourceErrors    map[string]string
	updatedAt       float64
}

// Manager は IPTV チャンネル一覧の取得・キャッシュ・検索を担当する。
type Manager struct {
	config       *config.Config
	dataDir      string
	logger       *slog.Logger
	client       *http.Client
	streamClient *http.Client
	countriesURL string

	// refreshMutex は更新処理を排他制御する (Python 版の _refresh_lock 相当) 。
	refreshMutex sync.Mutex
	// dataMutex はキャッシュの読み書きを保護する。
	dataMutex sync.RWMutex

	current state

	// qualityMutex は画質の検出結果のキャッシュを保護する。
	qualityMutex sync.Mutex
	// qualityCache はストリームの URL → 検出結果 (プロセスが生きている間は再利用する) 。
	qualityCache map[string]*QualityResult
}

// New は Manager を生成する。
func New(options Options) *Manager {
	timeout := time.Duration(options.Config.IPTV.RequestTimeout * float64(time.Second))
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	countriesURL := options.CountriesURL
	if countriesURL == "" {
		countriesURL = CountriesAPIURL
	}
	// ライブストリームは長時間流れ続けるため、総タイムアウトを設定しないクライアントを別に用意する
	// (接続とレスポンスヘッダーの待機にのみタイムアウトを設定する)
	streamClient := options.HTTPClient
	if streamClient == nil {
		streamClient = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   timeout,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	manager := &Manager{
		config:       options.Config,
		dataDir:      options.DataDir,
		logger:       logger,
		client:       client,
		streamClient: streamClient,
		countriesURL: countriesURL,
		qualityCache: map[string]*QualityResult{},
		current: state{
			countriesByCode: map[string]CountryInfo{},
			byDisplayID:     map[string]*Channel{},
			headersByURL:    map[string]map[string]string{},
			headersByHost:   map[string]map[string]string{},
			sourceErrors:    map[string]string{},
		},
	}
	return manager
}

// HTTPClient はプレイリストやロゴの取得に利用する HTTP クライアントを返す。
func (m *Manager) HTTPClient() *http.Client {
	return m.client
}

// StreamClient はライブストリームの取得に利用する HTTP クライアントを返す。
// ライブストリームは長時間流れ続けるため、総タイムアウトは設定されていない。
func (m *Manager) StreamClient() *http.Client {
	return m.streamClient
}

// userSourcesPath は追加登録した M3U プレイリストの URL を保存するファイルのパスを返す。
func (m *Manager) userSourcesPath() string {
	return filepath.Join(m.dataDir, UserSourcesFileName)
}

// tvuiChannelsPath はテレビ視聴 UI への登録を保存するファイルのパスを返す。
func (m *Manager) tvuiChannelsPath() string {
	return filepath.Join(m.dataDir, TVUIChannelsFileName)
}

// LoadUserSources は追加登録された M3U プレイリストの URL の一覧を読み込む。
// ファイルが無い (または内容が不正な) 場合は空のリストを返す。
func (m *Manager) LoadUserSources() []string {
	data, err := os.ReadFile(m.userSourcesPath())
	if err != nil {
		return []string{}
	}
	var sources []string
	if err := json.Unmarshal(data, &sources); err != nil {
		m.logger.Warn("Failed to load IPTV user sources", "error", err)
		return []string{}
	}
	return sources
}

// SaveUserSources は追加登録された M3U プレイリストの URL の一覧を保存する。
func (m *Manager) SaveUserSources(sources []string) error {
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	return writeJSONFile(m.userSourcesPath(), sources)
}

// AllSourceURLs は config.yaml に設定されたソースと、追加登録されたソースをまとめて返す (重複は除外する) 。
func (m *Manager) AllSourceURLs() []string {
	sources := []string{}
	for _, source := range append(append([]string{}, m.config.IPTV.Sources...), m.LoadUserSources()...) {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		duplicated := false
		for _, existing := range sources {
			if existing == source {
				duplicated = true
				break
			}
		}
		if !duplicated {
			sources = append(sources, source)
		}
	}
	return sources
}

// ConfigSources は config.yaml に設定されたソースの一覧を返す。
func (m *Manager) ConfigSources() []string {
	sources := []string{}
	for _, source := range m.config.IPTV.Sources {
		source = strings.TrimSpace(source)
		if source != "" {
			sources = append(sources, source)
		}
	}
	return sources
}

// Channels は現在キャッシュされているチャンネル一覧を返す。
func (m *Manager) Channels() []*Channel {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()
	return m.current.channels
}

// UpdatedAt はチャンネル一覧を最後に更新した時刻 (UNIX 時間) を返す。
func (m *Manager) UpdatedAt() float64 {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()
	return m.current.updatedAt
}

// SourceErrors は取得に失敗したプレイリストの URL とエラーメッセージの対応表を返す。
func (m *Manager) SourceErrors() map[string]string {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()
	return m.current.sourceErrors
}

// Refresh は config.yaml に設定された全ての M3U プレイリストを取得し、チャンネル一覧のキャッシュを更新する。
//
// キャッシュが有効な間 (cache_ttl 秒以内) は、force が true でない限り再取得しない。
// 複数のソースは並行して取得するが、1つのソースの失敗が他のソースに影響しないようにする。
func (m *Manager) Refresh(ctx context.Context, force bool) []*Channel {
	// IPTV 機能が無効の場合は何もしない
	if !m.config.IPTV.Enabled {
		return m.Channels()
	}

	// キャッシュが有効な場合はキャッシュを返す
	if !force && m.cacheIsFresh() {
		return m.Channels()
	}

	m.refreshMutex.Lock()
	defer m.refreshMutex.Unlock()

	// ロック取得待ちの間に他のリクエストが更新を完了している可能性があるため、再度キャッシュを確認する
	if !force && m.cacheIsFresh() {
		return m.Channels()
	}

	sources := m.AllSourceURLs()
	timestamp := time.Now()
	m.logger.Info("IPTV playlists updating...", "sources", len(sources))

	type fetchResult struct {
		source  string
		content string
		err     error
	}

	// 国一覧とプレイリストを並行して取得する
	var waitGroup sync.WaitGroup
	results := make([]fetchResult, len(sources))
	for index, source := range sources {
		waitGroup.Add(1)
		go func(index int, source string) {
			defer waitGroup.Done()
			content, err := m.fetchPlaylist(ctx, source)
			results[index] = fetchResult{source: source, content: content, err: err}
		}(index, source)
	}
	waitGroup.Add(1)
	var countriesByCode map[string]CountryInfo
	go func() {
		defer waitGroup.Done()
		countriesByCode = m.fetchCountries(ctx)
	}()
	waitGroup.Wait()

	channels := make([]*Channel, 0)
	headersByURL := map[string]map[string]string{}
	headersByHost := map[string]map[string]string{}
	errorsBySource := map[string]string{}

	seenURLs := map[string]bool{}
	for _, result := range results {
		// 取得に失敗した場合
		if result.err != nil {
			errorMessage := result.err.Error()
			errorsBySource[result.source] = errorMessage
			m.logger.Warn("Failed to fetch IPTV playlist", "source", result.source, "error", errorMessage)
			continue
		}
		for _, channel := range ParseM3UPlaylist(result.content, result.source, countriesByCode) {
			// ソースをまたいだ重複を除外する
			if seenURLs[channel.URL] {
				continue
			}
			seenURLs[channel.URL] = true
			channels = append(channels, channel)

			// ストリーム取得用のヘッダーを記録する
			streamHeaders := map[string]string{}
			if channel.UserAgent != nil {
				streamHeaders["User-Agent"] = *channel.UserAgent
			}
			if channel.Referrer != nil {
				streamHeaders["Referer"] = *channel.Referrer
			}
			if len(streamHeaders) > 0 {
				headersByURL[channel.URL] = streamHeaders
				host := hostOf(channel.URL)
				if _, exists := headersByHost[host]; !exists {
					headersByHost[host] = streamHeaders
				}
			}
		}
	}

	// 国名・グループ名・チャンネル名の順で並べ替える
	sort.SliceStable(channels, func(i int, j int) bool {
		left, right := channels[i], channels[j]
		if leftKey, rightKey := sortKey(left.CountryName), sortKey(right.CountryName); leftKey != rightKey {
			return leftKey < rightKey
		}
		if leftKey, rightKey := sortKey(left.Group), sortKey(right.Group); leftKey != rightKey {
			return leftKey < rightKey
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})

	byDisplayID := map[string]*Channel{}
	for _, channel := range channels {
		byDisplayID[BuildDisplayChannelID(channel.URL)] = channel
	}

	m.dataMutex.Lock()
	m.current = state{
		channels:        channels,
		countriesByCode: countriesByCode,
		byDisplayID:     byDisplayID,
		headersByURL:    headersByURL,
		headersByHost:   headersByHost,
		sourceErrors:    errorsBySource,
		updatedAt:       float64(time.Now().UnixNano()) / 1e9,
	}
	m.dataMutex.Unlock()

	m.logger.Info(
		"IPTV playlists update complete.",
		"channels", len(channels),
		"countries", len(countriesByCode),
		"errors", len(errorsBySource),
		"duration", time.Since(timestamp).Seconds(),
	)

	return m.Channels()
}

// cacheIsFresh はキャッシュが有効かどうかを返す。
func (m *Manager) cacheIsFresh() bool {
	m.dataMutex.RLock()
	updatedAt := m.current.updatedAt
	m.dataMutex.RUnlock()
	if updatedAt <= 0 {
		return false
	}
	return time.Since(time.Unix(0, int64(updatedAt*1e9))).Seconds() < float64(m.config.IPTV.CacheTTL)
}

// fetchPlaylist は指定されたソースからプレイリストの内容を取得する。
//
// http(s):// で始まる場合はネットワークから、それ以外はローカルファイルとして読み込む。
func (m *Manager) fetchPlaylist(ctx context.Context, source string) (string, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return "", err
		}
		request.Header.Set("User-Agent", m.config.IPTV.UserAgent)
		response, err := m.client.Do(request)
		if err != nil {
			return "", err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != 200 {
			return "", fmt.Errorf("HTTP Error %d", response.StatusCode)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return "", err
		}
		return string(body), nil
	}

	data, err := os.ReadFile(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("File Not Found")
		}
		return "", err
	}
	return string(data), nil
}

// fetchCountries は iptv-org の API から国コード → 国名・国旗の対応表を取得する。
//
// 取得に失敗した場合でもチャンネル一覧の更新自体は継続させたいため、例外は返さず空のマップを返す。
func (m *Manager) fetchCountries(ctx context.Context) map[string]CountryInfo {
	countriesByCode := map[string]CountryInfo{}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.countriesURL, nil)
	if err != nil {
		m.logger.Warn("Failed to fetch IPTV countries", "error", err)
		return countriesByCode
	}
	request.Header.Set("User-Agent", m.config.IPTV.UserAgent)
	response, err := m.client.Do(request)
	if err != nil {
		m.logger.Warn("Failed to fetch IPTV countries", "error", err)
		return countriesByCode
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		m.logger.Warn("Failed to fetch IPTV countries", "status", response.StatusCode)
		return countriesByCode
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		m.logger.Warn("Failed to fetch IPTV countries", "error", err)
		return countriesByCode
	}

	var countries []struct {
		Code *string `json:"code"`
		Name *string `json:"name"`
		Flag *string `json:"flag"`
	}
	if err := json.Unmarshal(body, &countries); err != nil {
		m.logger.Warn("Failed to fetch IPTV countries", "error", err)
		return countriesByCode
	}
	for _, country := range countries {
		if country.Code == nil {
			continue
		}
		name := *country.Code
		if country.Name != nil && *country.Name != "" {
			name = *country.Name
		}
		flag := ""
		if country.Flag != nil {
			flag = *country.Flag
		}
		countriesByCode[strings.ToUpper(*country.Code)] = CountryInfo{Name: name, Flag: flag}
	}
	return countriesByCode
}

// FilterChannels はキャッシュされているチャンネル一覧を、国・グループ・キーワードで絞り込む。
func (m *Manager) FilterChannels(country *string, group *string, search *string) []*Channel {
	channels := m.Channels()
	var countryCode *string
	if country != nil {
		upper := strings.ToUpper(*country)
		countryCode = &upper
	}
	var searchKeyword *string
	if search != nil {
		lower := strings.ToLower(*search)
		searchKeyword = &lower
	}

	filtered := make([]*Channel, 0, len(channels))
	for _, channel := range channels {
		if countryCode != nil && (channel.Country == nil || *channel.Country != *countryCode) {
			continue
		}
		if group != nil && (channel.Group == nil || *channel.Group != *group) {
			continue
		}
		if searchKeyword != nil && !strings.Contains(strings.ToLower(channel.Name), *searchKeyword) {
			continue
		}
		filtered = append(filtered, channel)
	}
	return filtered
}

// Countries はチャンネル一覧に含まれる国を、チャンネル数付きで返す (チャンネル数の多い順) 。
//
// channels が nil の場合はキャッシュ全体を対象にする。
func (m *Manager) Countries(channels []*Channel) []Country {
	if channels == nil {
		channels = m.Channels()
	}
	m.dataMutex.RLock()
	countriesByCode := m.current.countriesByCode
	m.dataMutex.RUnlock()

	counts := map[string]int{}
	for _, channel := range channels {
		if channel.Country == nil {
			continue
		}
		counts[*channel.Country]++
	}

	countries := make([]Country, 0, len(counts))
	for code, count := range counts {
		info := countriesByCode[code]
		name := info.Name
		if name == "" {
			name = code
		}
		countries = append(countries, Country{Code: code, Name: name, Flag: info.Flag, Count: count})
	}
	// チャンネル数の多い順 → 国名の順で並べる
	sort.SliceStable(countries, func(i int, j int) bool {
		if countries[i].Count != countries[j].Count {
			return countries[i].Count > countries[j].Count
		}
		return strings.ToLower(countries[i].Name) < strings.ToLower(countries[j].Name)
	})
	return countries
}

// Groups はチャンネル一覧に含まれるグループを、チャンネル数付きで返す (チャンネル数の多い順) 。
//
// channels が nil の場合はキャッシュ全体を対象にする。
func (m *Manager) Groups(channels []*Channel) []Group {
	if channels == nil {
		channels = m.Channels()
	}
	counts := map[string]int{}
	for _, channel := range channels {
		if channel.Group == nil {
			continue
		}
		counts[*channel.Group]++
	}

	groups := make([]Group, 0, len(counts))
	for name, count := range counts {
		groups = append(groups, Group{Name: name, Count: count})
	}
	groups = dedupeSortGroups(groups)
	return groups
}

// dedupeSortGroups はグループをチャンネル数の多い順 → グループ名の順で並べ替える。
func dedupeSortGroups(groups []Group) []Group {
	sort.SliceStable(groups, func(i int, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	return groups
}

// StreamHeaders は指定されたストリーム URL を取得する際に送信すべき追加ヘッダーを返す。
//
// URL が完全一致しない場合 (HLS のセグメントなど) は、ホスト名が一致するチャンネルのヘッダーをフォールバックとして返す。
func (m *Manager) StreamHeaders(streamURL string) map[string]string {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()
	headers := map[string]string{}
	if values, exists := m.current.headersByURL[streamURL]; exists {
		for key, value := range values {
			headers[key] = value
		}
		return headers
	}
	if values, exists := m.current.headersByHost[hostOf(streamURL)]; exists {
		for key, value := range values {
			headers[key] = value
		}
	}
	return headers
}

// GetChannelByDisplayChannelID は display_channel_id に一致する IPTV チャンネルを取得する。
func (m *Manager) GetChannelByDisplayChannelID(displayChannelID string) *Channel {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()
	channel, exists := m.current.byDisplayID[displayChannelID]
	if !exists {
		return nil
	}
	return channel
}

// hostOf は URL のホスト名 (netloc) を返す。
func hostOf(target string) string {
	return SplitURL(target).Netloc
}

// sortKey は並べ替え用のキーを返す (値が無い場合は最大文字として扱う) 。
func sortKey(value *string) string {
	if value == nil {
		return "\uffff"
	}
	return strings.ToLower(*value)
}

// writeJSONFile は Python の json.dumps(indent=2, ensure_ascii=False) 相当の形式で JSON を書き出す。
func writeJSONFile(path string, value any) error {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}
