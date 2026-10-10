// Package iptv はインターネット上で公開されている IPTV の M3U プレイリストを取り込み、
// KonomiTV 上で視聴できるようにするためのユーティリティ群を提供する。
//
// Python 版 server/app/utils/IPTVUtil.py の移植。
// チャンネル一覧はメモリ上にキャッシュされ、cache_ttl 秒間は再利用される。
package iptv

import (
	"crypto/sha1"
	"encoding/hex"
	"math/big"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	// ProxyPath は IPTV ストリームをプロキシする API のパス (IPTVRouter と一致させる必要がある) 。
	ProxyPath = "/api/iptv/proxy"
	// DisplayChannelIDPrefix は IPTV チャンネルの display_channel_id のプレフィックス。
	// クライアント側の ChannelUtils.getChannelType() は「英字 + 数字」の ID を前提としているため、
	// プレフィックスの後ろは必ず数字 (10 進数) にする。
	DisplayChannelIDPrefix = "iptv"
	// ChannelIDPrefix は IPTV の疑似チャンネルの id のプレフィックス (通常のチャンネルの id と衝突しないようにする) 。
	ChannelIDPrefix = "IPTV-"
	// TVUIMaxChannels はテレビ視聴 UI に登録できる IPTV チャンネルの最大数。
	TVUIMaxChannels = 200
	// CountriesAPIURL は国コード (ISO 3166-1 alpha-2) から国名・国旗を取得するための iptv-org の API。
	CountriesAPIURL = "https://iptv-org.github.io/api/countries.json"
	// UserSourcesFileName は追加登録した M3U プレイリストの URL を保存するファイル名。
	UserSourcesFileName = "iptv_sources.json"
	// DisabledSourcesFileName は無効化した M3U プレイリストの URL を保存するファイル名。
	DisabledSourcesFileName = "iptv_disabled_sources.json"
	// TVUIChannelsFileName はテレビ視聴 UI に登録した IPTV チャンネルを保存するファイル名。
	TVUIChannelsFileName = "iptv_tvui_channels.json"
	// QualityDetectionConcurrency は画質の検出を同時に実行する数の上限。
	QualityDetectionConcurrency = 8
)

// 正規表現 (Python 版と等価なものを使う) 。
var (
	// extinfAttributePattern は EXTINF 行の属性 (key="value") を抽出する。
	extinfAttributePattern = regexp.MustCompile(`([\w-]+)="([^"]*)"`)
	// playlistURIPattern はプレイリスト内の URI="..." を抽出する。
	playlistURIPattern = regexp.MustCompile(`URI="([^"]+)"`)
	// tvgIDCountryPattern は tvg-id (例: "NHKWorldJapan.jp@SD") から国コードを抽出する。
	tvgIDCountryPattern = regexp.MustCompile(`\.([A-Za-z]{2})(?:@|$)`)
	// sourceURLCountryPattern はプレイリストの URL (例: ".../countries/jp.m3u") から国コードを抽出する。
	sourceURLCountryPattern = regexp.MustCompile(`(?i)/countries/([A-Za-z]{2})\.m3u`)
	// nameQualityPattern はチャンネル名に含まれる画質 (例: '1080p') を抽出する。
	nameQualityPattern = regexp.MustCompile(`[\[（(]\s*(\d{3,4}[pi])\s*[\]）)]`)
	// newlinePattern は文字列を行に分割するために利用する (Python の str.splitlines() 相当) 。
	newlinePattern = regexp.MustCompile("\r\n|\r|\n")
)

// Channel は IPTV の1チャンネル分の情報を保持する。
type Channel struct {
	// ID はチャンネルを一意に識別する ID (URL のハッシュから生成する) 。
	ID string `json:"id"`
	// Name はチャンネル名 (EXTINF 行のカンマ以降、または tvg-name) 。
	Name string `json:"name"`
	// URL はストリームの URL (m3u8 / ts / mp4 など) 。
	URL string `json:"url"`
	// LogoURL はチャンネルロゴの URL。
	LogoURL *string `json:"logo_url"`
	// Group はグループ (ジャンルやカテゴリ) 。
	Group *string `json:"group"`
	// Country は国コード (ISO 3166-1 alpha-2, 大文字) 。
	Country *string `json:"country"`
	// CountryName は国名。
	CountryName *string `json:"country_name"`
	// CountryFlag は国旗の絵文字。
	CountryFlag *string `json:"country_flag"`
	// TvgID は元のプレイリストで振られている tvg-id。
	TvgID *string `json:"tvg_id"`
	// Language は言語コード。
	Language *string `json:"language"`
	// UserAgent はストリーム取得時に送信する User-Agent (プレイリストで指定されている場合) 。
	UserAgent *string `json:"user_agent"`
	// Referrer はストリーム取得時に送信する Referer (プレイリストで指定されている場合) 。
	Referrer *string `json:"referrer"`
	// IsGeoBlocked はジオブロック (地域制限) されているとプレイリストが明記しているか。
	IsGeoBlocked bool `json:"is_geo_blocked"`
	// SourceURL はこのチャンネルを提供しているプレイリストの URL。
	SourceURL string `json:"source_url"`
	// SourceName はこのチャンネルを提供しているプレイリストの表示名 (ソース URL から生成する) 。
	SourceName string `json:"source_name"`
	// Identity は同一チャンネル判定キー (同じ Identity なら同一チャンネルとみなす) 。
	Identity string `json:"identity"`
	// SameChannelCount は同じ Identity を持つチャンネルの総数 (1 = この 1 件のみ) 。
	// チャンネル一覧のキャッシュ構築後に集計するため、解析直後は常に 1 になる。
	SameChannelCount int `json:"same_channel_count"`
	// SameChannelSources は同じ Identity を持つチャンネルが由来するソース表示名の一覧 (重複除去・ソート済み) 。
	SameChannelSources []string `json:"same_channel_sources"`
}

// CountryInfo は国コードに対する国名・国旗の情報。
type CountryInfo struct {
	Name string `json:"name"`
	Flag string `json:"flag"`
}

// Country は国一覧 API の1件分。
type Country struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Flag  string `json:"flag"`
	Count int    `json:"count"`
}

// Group はグループ一覧 API の1件分。
type Group struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Quality は IPTV の元配信で配信されている画質 (バリアント) の1件分。
type Quality struct {
	Name      *string `json:"name"`
	Width     *int    `json:"width"`
	Height    *int    `json:"height"`
	Bandwidth *int    `json:"bandwidth"`
}

// QualityResult は画質の検出結果。
type QualityResult struct {
	Qualities     []Quality `json:"qualities"`
	Codec         *string   `json:"codec"`
	SourceQuality *string   `json:"source_quality"`
}

// ChannelResponse は schemas.IPTVChannel 互換のレスポンス。
type ChannelResponse struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	LogoURL          *string   `json:"logo_url"`
	Group            *string   `json:"group"`
	Country          *string   `json:"country"`
	CountryName      *string   `json:"country_name"`
	CountryFlag      *string   `json:"country_flag"`
	TvgID            *string   `json:"tvg_id"`
	Language         *string   `json:"language"`
	IsGeoBlocked     bool      `json:"is_geo_blocked"`
	SourceURL        string    `json:"source_url"`
	QualityHint      *string   `json:"quality_hint"`
	StreamURL        string    `json:"stream_url"`
	StreamType       string    `json:"stream_type"`
	IsHLS            bool      `json:"is_hls"`
	DisplayChannelID string    `json:"display_channel_id"`
	IsTVUIRegistered bool      `json:"is_tvui_registered"`
	SourceQuality    *string   `json:"source_quality"`
	SourceCodec      *string   `json:"source_codec"`
	Qualities        []Quality `json:"qualities"`
	// SourceName はこのチャンネルを提供しているプレイリストの表示名 (例: 'iptv-org.github.io/streams/jp.m3u') 。
	SourceName string `json:"source_name"`
	// Identity は同一チャンネル判定キー (同じ Identity なら同一チャンネルとみなす) 。
	Identity string `json:"identity"`
	// SameChannelCount は同じ Identity を持つチャンネルの総数 (1 = この 1 件のみ) 。
	SameChannelCount int `json:"same_channel_count"`
	// SameChannelSources は同じ Identity を持つチャンネルが由来するソース表示名の一覧 (重複除去・ソート済み) 。
	SameChannelSources []string `json:"same_channel_sources"`
}

// normalizeNameSymbols はチャンネル名の正規化で除去する記号の集合 (Python 版の _NORMALIZE_NAME_SYMBOLS と等価) 。
// 全角形は正規化の過程で半角に畳み込まれるが、畳み込みの有無に関わらず除去できるよう両方の形を登録しておく。
var normalizeNameSymbols = map[rune]bool{
	'.': true, '・': true, '_': true, '-': true, ':': true, '：': true, '/': true, '／': true,
	'\\': true, '(': true, ')': true, '（': true, '）': true, '[': true, ']': true,
	'「': true, '」': true, '『': true, '』': true, '!': true, '！': true, '?': true, '？': true,
	',': true, '，': true, '、': true, '\'': true, '"': true, '’': true, '”': true,
}

// BuildSourceName はプレイリストのソース URL から、表示用のソース名を生成する。
//
// http(s):// の場合はホスト + パスを使い (クエリ・フラグメントは無視する) 、パスが無い場合はホストのみを使う。
// それ以外はローカルファイルパスとみなし、basename を使う。
// URL として解釈できない場合は、URL の文字列をそのまま返す (Python 版の _BuildSourceName() と等価) 。
func BuildSourceName(sourceURL string) string {
	parsed := SplitURL(sourceURL)
	// http(s):// の場合はホスト + パスを使う
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		host := parsed.Netloc
		if host == "" {
			return sourceURL
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return host + parsed.Path
		}
		return host
	}
	// それ以外はローカルファイルパスとして basename を使う
	// (basename が空・カレントディレクトリ・ルートになる場合は URL として解釈できなかったものとみなす)
	name := filepath.Base(sourceURL)
	if name != "" && name != "." && name != "/" && name != "\\" {
		return name
	}
	return sourceURL
}

// NormalizeChannelName はチャンネル名を、同一チャンネル判定用に正規化する。
//
// Python 版は NFKC 正規化 → 小文字化 → 空白と記号の除去の順で行うが、
// Go の標準ライブラリには NFKC が無いため、全角英数記号 (U+FF01–U+FF5E → U+0021–U+007E) と
// 全角スペース (U+3000 → 半角空白) の畳み込みで代替する。
// この代替は ASCII の名前・tvg-id では Python 版と完全に一致する (日本語の合字など NFKC 固有の変換は近似になる) 。
// その上で小文字化し、空白と区切り記号を除去する。
func NormalizeChannelName(name string) string {
	var builder strings.Builder
	for _, character := range name {
		// 全角英数記号を半角に、全角スペースを半角空白に畳み込む (NFKC の代替)
		switch {
		case character >= 0xFF01 && character <= 0xFF5E:
			character -= 0xFEE0
		case character == 0x3000:
			character = ' '
		}
		// 空白は除去する
		if unicode.IsSpace(character) {
			continue
		}
		// 区切り記号は除去する
		if normalizeNameSymbols[character] {
			continue
		}
		builder.WriteRune(unicode.ToLower(character))
	}
	return builder.String()
}

// BuildIdentity はチャンネルの identity (同一チャンネル判定キー) を生成する。
//
// 優先順位は tvg-id > 国コード + 正規化チャンネル名 > display_channel_id の順。
// tvg-id があればそれを trim・小文字化して使い、無ければ国コードと正規化したチャンネル名で判定する。
// 名前が正規化すると空になる場合のみ、最終フォールバックとして display_channel_id を使う
// (Python 版の _BuildIdentity() と等価) 。
func BuildIdentity(tvgID *string, country *string, name string, displayChannelID string) string {
	// tvg-id が指定されている場合はそれを最優先で使う
	if tvgID != nil && strings.TrimSpace(*tvgID) != "" {
		return "tvg:" + strings.ToLower(strings.TrimSpace(*tvgID))
	}
	// 国コード + 正規化チャンネル名で判定する
	normalizedName := NormalizeChannelName(name)
	if normalizedName != "" {
		countryPart := ""
		if country != nil {
			countryPart = strings.ToLower(*country)
		}
		return "name:" + countryPart + ":" + normalizedName
	}
	// 名前が正規化すると空になる場合のみ display_channel_id を使う
	return "id:" + displayChannelID
}

// BuildChannelID はストリームの URL から、チャンネルを一意に識別する安定した ID を生成する。
func BuildChannelID(url string) string {
	sum := sha1.Sum([]byte(url))
	return hex.EncodeToString(sum[:])[:16]
}

// BuildDisplayChannelID はストリームの URL から、IPTV チャンネルの display_channel_id を生成する。
func BuildDisplayChannelID(url string) string {
	sum := sha1.Sum([]byte(url))
	// ハッシュの先頭 60 bit を 10 進数に変換する (Python 版の int(hexdigest[:15], 16) と等価) 。
	value := new(big.Int).SetBytes(sum[:])
	value.Rsh(value, 160-60)
	return DisplayChannelIDPrefix + value.String()
}

// IsDisplayChannelID は指定された display_channel_id が IPTV の疑似チャンネルのものかを判定する。
func IsDisplayChannelID(displayChannelID string) bool {
	if !strings.HasPrefix(displayChannelID, DisplayChannelIDPrefix) {
		return false
	}
	suffix := displayChannelID[len(DisplayChannelIDPrefix):]
	if suffix == "" {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// schemePattern は URL のスキームを抽出するための正規表現。
var schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

// relativeSchemes は相対 URL の解決に対応しているスキーム (Python の uses_relative 相当) 。
var relativeSchemes = map[string]bool{
	"": true, "ftp": true, "http": true, "gopher": true, "nntp": true, "imap": true, "wais": true,
	"file": true, "https": true, "shttp": true, "mms": true, "prospero": true, "rtsp": true,
	"rtspu": true, "sftp": true, "svn": true, "svn+ssh": true, "ws": true, "wss": true,
}

// netlocSchemes は netloc を持つスキーム (Python の uses_netloc 相当) 。
var netlocSchemes = map[string]bool{
	"": true, "ftp": true, "http": true, "gopher": true, "nntp": true, "telnet": true, "imap": true,
	"wais": true, "file": true, "mms": true, "https": true, "shttp": true, "snews": true,
	"prospero": true, "rtsp": true, "rtspu": true, "rsync": true, "svn": true, "svn+ssh": true,
	"sftp": true, "nfs": true, "git": true, "git+ssh": true, "ws": true, "wss": true,
	"itms-services": true,
}

// URLParts は URL を分割した結果。
type URLParts struct {
	// Scheme は小文字に正規化したスキーム。
	Scheme string
	Netloc string
	// Path は ';' 以降のパラメーターを含むパス
	// (Python の urlparse() はパラメーターを分離するが、再構築時には同じ位置に戻る) 。
	Path string
	// Query は '?' を含むクエリ。
	Query string
	// Fragment は '#' を含むフラグメント。
	Fragment string
}

// SplitURL は URL を Scheme / Netloc / Path / Query / Fragment に分割する (Python の urlsplit() 相当) 。
func SplitURL(value string) URLParts {
	parts := URLParts{}
	rest := value
	if index := strings.IndexByte(rest, '#'); index >= 0 {
		parts.Fragment = rest[index:]
		rest = rest[:index]
	}
	if index := strings.IndexByte(rest, '?'); index >= 0 {
		parts.Query = rest[index:]
		rest = rest[:index]
	}
	if matched := schemePattern.FindString(rest); matched != "" {
		parts.Scheme = strings.ToLower(matched[:len(matched)-1])
		rest = rest[len(matched):]
	}
	if strings.HasPrefix(rest, "//") {
		authority := rest[2:]
		if index := strings.IndexByte(authority, '/'); index >= 0 {
			parts.Netloc = authority[:index]
			parts.Path = authority[index:]
		} else {
			parts.Netloc = authority
		}
		return parts
	}
	parts.Path = rest
	return parts
}

// BuildURL は分割した URL を再構築する (Python の urlunsplit() 相当) 。
func BuildURL(parts URLParts) string {
	path := parts.Path
	switch {
	case parts.Netloc != "":
		if path != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		path = "//" + parts.Netloc + path
	case strings.HasPrefix(path, "//"):
		path = "//" + path
	case parts.Scheme != "" && netlocSchemes[parts.Scheme] && (path == "" || strings.HasPrefix(path, "/")):
		path = "//" + path
	}
	if parts.Scheme != "" {
		path = parts.Scheme + ":" + path
	}
	return path + parts.Query + parts.Fragment
}

// NormalizeURL はプレイリスト内の相対 URL を、プレイリストの URL を基準に絶対 URL に変換する。
//
// Python の urllib.parse.urljoin() と等価な処理を行う。
// Go の net/url は非 ASCII 文字をパーセントエンコーディングしてしまい Python 版と結果が一致しないため、
// 文字列ベースで解決する (Python の urljoin 自体も文字列ベースの実装になっている) 。
func NormalizeURL(baseURL string, target string) string {
	target = strings.TrimSpace(target)
	if baseURL == "" {
		return target
	}
	if target == "" {
		return baseURL
	}

	base := SplitURL(baseURL)
	parsed := SplitURL(target)

	// 相対 URL の解決に対応していないスキーム (Windows のローカルファイルパスなど) の場合はそのまま返す
	if !relativeSchemes[base.Scheme] {
		return target
	}
	// スキームが異なる場合はそのまま返す
	if parsed.Scheme != "" && parsed.Scheme != base.Scheme {
		return target
	}

	scheme := base.Scheme
	if parsed.Scheme != "" {
		scheme = parsed.Scheme
	}
	netloc := ""

	// netloc が指定されている場合はそのまま (scheme のみ小文字化して) 返す
	if netlocSchemes[scheme] && parsed.Netloc != "" {
		return BuildURL(URLParts{Scheme: scheme, Netloc: parsed.Netloc, Path: parsed.Path, Query: parsed.Query, Fragment: parsed.Fragment})
	}
	if netlocSchemes[scheme] {
		netloc = base.Netloc
	}

	// パスが無い場合はベースのパスを引き継ぐ
	if parsed.Path == "" {
		query := parsed.Query
		if query == "" {
			query = base.Query
		}
		return BuildURL(URLParts{Scheme: scheme, Netloc: netloc, Path: base.Path, Query: query, Fragment: parsed.Fragment})
	}

	baseParts := strings.Split(base.Path, "/")
	if baseParts[len(baseParts)-1] != "" {
		// 最後の要素はディレクトリではないため、相対パスの解決では考慮しない
		baseParts = baseParts[:len(baseParts)-1]
	}

	var segments []string
	if strings.HasPrefix(parsed.Path, "/") {
		segments = strings.Split(parsed.Path, "/")
	} else {
		segments = append(baseParts, strings.Split(parsed.Path, "/")...)
		// 再結合時に冗長なスラッシュになる要素 (中間の空要素) を取り除く
		if len(segments) > 2 {
			filtered := make([]string, 0, len(segments))
			filtered = append(filtered, segments[0])
			for _, segment := range segments[1 : len(segments)-1] {
				if segment != "" {
					filtered = append(filtered, segment)
				}
			}
			segments = append(filtered, segments[len(segments)-1])
		}
	}

	resolved := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
		case ".":
			continue
		default:
			resolved = append(resolved, segment)
		}
	}
	// 最後の要素が相対ディレクトリ (. や ..) の場合は末尾に '/' を付ける
	if segments[len(segments)-1] == "." || segments[len(segments)-1] == ".." {
		resolved = append(resolved, "")
	}

	path := strings.Join(resolved, "/")
	if path == "" {
		path = "/"
	}
	return BuildURL(URLParts{Scheme: scheme, Netloc: netloc, Path: path, Query: parsed.Query, Fragment: parsed.Fragment})
}

// pathWithoutParams は ';' 以降のパラメーターを除いたパスを返す (Python の urlparse().path 相当) 。
func pathWithoutParams(value string) string {
	path := SplitURL(value).Path
	index := strings.LastIndexByte(path, ';')
	// 最後の '/' より後ろに ';' がある場合のみパラメーターとして扱う
	if index < 0 || strings.IndexByte(path[index:], '/') >= 0 {
		return path
	}
	return path[:index]
}

// DetectStreamType はストリームの URL から、配信フォーマットを推定する。
func DetectStreamType(streamURL string) string {
	parsed := SplitURL(streamURL)
	lowerPath := strings.ToLower(pathWithoutParams(streamURL))
	lowerQuery := strings.ToLower(strings.TrimPrefix(parsed.Query, "?"))
	if strings.HasSuffix(lowerPath, ".m3u8") || strings.Contains(lowerQuery, "m3u8") || strings.Contains(lowerQuery, "hls") {
		return "HLS"
	}
	if strings.HasSuffix(lowerPath, ".mpd") || strings.Contains(lowerQuery, "dash") {
		return "DASH"
	}
	if strings.HasSuffix(lowerPath, ".mp4") || strings.HasSuffix(lowerPath, ".m4v") {
		return "MP4"
	}
	if strings.HasSuffix(lowerPath, ".ts") || strings.HasSuffix(lowerPath, ".m2ts") || strings.HasSuffix(lowerPath, ".mts") {
		return "MPEGTS"
	}
	return "Other"
}

// ParseQualityFromName はチャンネル名に含まれる画質 (例: 'NHK World-Japan (1080p)' の '1080p') を抽出する。
func ParseQualityFromName(name string) *string {
	matched := nameQualityPattern.FindStringSubmatch(name)
	if matched == nil {
		return nil
	}
	quality := strings.ToLower(matched[1])
	return &quality
}

// BuildQualityName は映像の高さ (ピクセル) から、画質名 (例: '1080p') を生成する。
func BuildQualityName(height *int) *string {
	if height == nil {
		return nil
	}
	// 高さ (ピクセル) から画質名への対応表 (高い順) 。
	thresholds := []struct {
		height int
		name   string
	}{
		{2160, "2160p"},
		{1440, "1440p"},
		{1080, "1080p"},
		{720, "720p"},
		{576, "576p"},
		{540, "540p"},
		{480, "480p"},
		{360, "360p"},
		{240, "240p"},
	}
	for _, threshold := range thresholds {
		if *height >= threshold.height {
			name := threshold.name
			return &name
		}
	}
	name := strconv.Itoa(*height) + "p"
	return &name
}

// BuildCodecName は HLS の CODECS 属性の値 (例: 'avc1.4d401f,mp4a.40.2') から、映像コーデックの表示名を生成する。
func BuildCodecName(codecs *string) *string {
	if codecs == nil {
		return nil
	}
	// CODECS 属性の値から表示用のコーデック名への対応表。
	prefixes := []struct {
		prefix string
		name   string
	}{
		{"avc1", "H.264"},
		{"avc3", "H.264"},
		{"hvc1", "H.265"},
		{"hev1", "H.265"},
		{"mp4v", "MPEG-4"},
		{"mp2v", "MPEG-2"},
		{"av01", "AV1"},
		{"vp09", "VP9"},
	}
	for _, codec := range strings.Split(*codecs, ",") {
		codec = strings.ToLower(strings.TrimSpace(codec))
		for _, prefix := range prefixes {
			if strings.HasPrefix(codec, prefix.prefix) {
				name := prefix.name
				return &name
			}
		}
	}
	return nil
}

// BuildProxyURL はストリームの URL を、KonomiTV サーバーのプロキシ API の URL に変換する。
func BuildProxyURL(streamURL string) string {
	return ProxyPath + "?url=" + quotePython(streamURL)
}

// quotePython は Python の urllib.parse.quote(safe="") と等価なパーセントエンコーディングを行う。
// Go の url.QueryEscape() は空白を '+' に変換するため、そのままでは Python 版と一致しない。
func quotePython(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"
	var builder strings.Builder
	for _, character := range []byte(value) {
		if strings.IndexByte(unreserved, character) >= 0 {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteString(strings.ToUpper(hex.EncodeToString([]byte{character})))
	}
	return builder.String()
}

// IsHLSPlaylist は URL が HLS プレイリスト (.m3u8) を指しているかを判定する。
func IsHLSPlaylist(streamURL string) bool {
	return strings.HasSuffix(strings.ToLower(pathWithoutParams(streamURL)), ".m3u8")
}

// ToResponse はチャンネル情報を、API レスポンス用の構造体に変換する。
func (channel *Channel) ToResponse() *ChannelResponse {
	streamType := DetectStreamType(channel.URL)
	// same_channel_sources は常に配列で返す (未集計時も null にしない)
	sameChannelSources := channel.SameChannelSources
	if sameChannelSources == nil {
		sameChannelSources = []string{}
	}
	return &ChannelResponse{
		ID:           channel.ID,
		Name:         channel.Name,
		LogoURL:      channel.LogoURL,
		Group:        channel.Group,
		Country:      channel.Country,
		CountryName:  channel.CountryName,
		CountryFlag:  channel.CountryFlag,
		TvgID:        channel.TvgID,
		Language:     channel.Language,
		IsGeoBlocked: channel.IsGeoBlocked,
		SourceURL:    channel.SourceURL,
		// チャンネル名から推定した画質 (ストリームにアクセスできないチャンネルでも画質を提示できるようにする)
		QualityHint: ParseQualityFromName(channel.Name),
		// ストリームの URL はそのままクライアントに公開せず、プロキシ経由の URL に置き換える
		StreamURL:        BuildProxyURL(channel.URL),
		StreamType:       streamType,
		IsHLS:            streamType == "HLS",
		DisplayChannelID: BuildDisplayChannelID(channel.URL),
		Qualities:        []Quality{},
		// ソースの表示名と同一チャンネル判定の情報は、解析・集計済みの値をそのまま引き継ぐ
		SourceName:         channel.SourceName,
		Identity:           channel.Identity,
		SameChannelCount:   channel.SameChannelCount,
		SameChannelSources: sameChannelSources,
	}
}

// LiveChannelResponse は schemas.LiveChannel 互換のレスポンス (IPTV の疑似チャンネル用) 。
type LiveChannelResponse struct {
	ID                 string `json:"id"`
	DisplayChannelID   string `json:"display_channel_id"`
	NetworkID          int    `json:"network_id"`
	ServiceID          int    `json:"service_id"`
	TransportStreamID  *int   `json:"transport_stream_id"`
	RemoconID          int    `json:"remocon_id"`
	ChannelNumber      string `json:"channel_number"`
	Type               string `json:"type"`
	Name               string `json:"name"`
	TerrestrialRegions any    `json:"terrestrial_regions"`
	JikkyoForce        *int   `json:"jikkyo_force"`
	IsSubchannel       bool   `json:"is_subchannel"`
	IsRadiochannel     bool   `json:"is_radiochannel"`
	IsWatchable        bool   `json:"is_watchable"`
	IsDisplay          bool   `json:"is_display"`
	ViewerCount        int    `json:"viewer_count"`
	ProgramPresent     any    `json:"program_present"`
	ProgramFollowing   any    `json:"program_following"`
}

// ToLiveChannelResponse は IPTV チャンネルを、/api/channels のレスポンス (LiveChannel) 用の構造体に変換する。
//
// 番組情報 (EPG) は存在しないため、program_present / program_following は null にする。
func (channel *Channel) ToLiveChannelResponse() *LiveChannelResponse {
	displayChannelID := BuildDisplayChannelID(channel.URL)
	name := channel.Name
	if channel.CountryName != nil {
		name = channel.Name + " (" + *channel.CountryName + ")"
	}
	channelNumber := "IPTV"
	if channel.Country != nil {
		channelNumber = *channel.Country
	}
	return &LiveChannelResponse{
		ID:                 ChannelIDPrefix + displayChannelID,
		DisplayChannelID:   displayChannelID,
		NetworkID:          0,
		ServiceID:          0,
		TransportStreamID:  nil,
		RemoconID:          0,
		ChannelNumber:      channelNumber,
		Type:               "IPTV",
		Name:               name,
		TerrestrialRegions: nil,
		JikkyoForce:        nil,
		IsSubchannel:       false,
		IsRadiochannel:     false,
		IsWatchable:        true,
		IsDisplay:          true,
		ViewerCount:        0,
		ProgramPresent:     nil,
		ProgramFollowing:   nil,
	}
}

// EncodingChannel は LiveEncodingTask が扱う Channel モデル相当の情報 (IPTV の疑似チャンネル用) 。
type EncodingChannel struct {
	ID                string
	DisplayChannelID  string
	NetworkID         int
	ServiceID         int
	TransportStreamID *int
	RemoconID         int
	ChannelNumber     string
	Type              string
	Name              string
	IsSubchannel      bool
	IsRadiochannel    bool
	IsWatchable       bool
}

// ToEncodingChannel は IPTV チャンネルを、ライブエンコード処理が扱える Channel モデル相当の構造体に変換する。
func (channel *Channel) ToEncodingChannel() *EncodingChannel {
	displayChannelID := BuildDisplayChannelID(channel.URL)
	channelNumber := "IPTV"
	if channel.Country != nil {
		channelNumber = *channel.Country
	}
	return &EncodingChannel{
		ID:                ChannelIDPrefix + displayChannelID,
		DisplayChannelID:  displayChannelID,
		NetworkID:         0,
		ServiceID:         0,
		TransportStreamID: nil,
		RemoconID:         0,
		ChannelNumber:     channelNumber,
		// エンコードオプションはチャンネルタイプで分岐するため、地デジ相当として扱う
		Type:           "GR",
		Name:           channel.Name,
		IsSubchannel:   false,
		IsRadiochannel: false,
		IsWatchable:    true,
	}
}
