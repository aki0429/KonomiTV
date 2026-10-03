package iptv

import (
	"net/url"
	"strings"
	"unicode"
)

// ParseM3UPlaylist は M3U / M3U8 プレイリストの文字列を解析し、チャンネル一覧を生成する。
//
// 拡張 M3U (#EXTINF) の属性 (tvg-id / tvg-logo / tvg-name / group-title / tvg-country /
// http-user-agent / http-referrer など) を読み取り、次の行の URL と組み合わせて1チャンネル分の情報を組み立てる。
// 同じストリーム URL が複数存在する場合は、最初に見つかったものを優先して重複を除外する。
//
// countriesByCode は国コード → 国名・国旗の対応表 (国名・国旗の付与に利用する) 。
func ParseM3UPlaylist(content string, sourceURL string, countriesByCode map[string]CountryInfo) []*Channel {
	channels := make([]*Channel, 0)
	seenURLs := map[string]bool{}

	// 現在処理中の #EXTINF 行の属性
	currentAttributes := map[string]string{}
	var currentName *string

	for _, rawLine := range SplitLines(content) {
		line := strings.TrimSpace(rawLine)

		// 空行は無視
		if line == "" {
			continue
		}

		// コメント行 (タグ) の場合
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXTINF:") {
				// #EXTINF 行なら、属性とチャンネル名を保持しておく
				currentAttributes = map[string]string{}
				for _, matched := range extinfAttributePattern.FindAllStringSubmatch(line, -1) {
					currentAttributes[matched[1]] = matched[2]
				}
				// カンマ以降がチャンネル名だが、属性値 (http-user-agent など) にカンマが含まれることがあるため、
				// まず属性 (key="value") をすべて取り除いてからカンマで分割する
				lineWithoutAttributes := extinfAttributePattern.ReplaceAllString(line, "")
				if index := strings.Index(lineWithoutAttributes, ","); index >= 0 {
					name := strings.TrimSpace(lineWithoutAttributes[index+1:])
					currentName = &name
				} else if tvgName, ok := currentAttributes["tvg-name"]; ok && tvgName != "" {
					name := tvgName
					currentName = &name
				} else {
					currentName = nil
				}
			} else if strings.HasPrefix(line, "#EXTGRP:") {
				// #EXTGRP はグループ指定の別形式
				if _, exists := currentAttributes["group-title"]; !exists {
					currentAttributes["group-title"] = strings.TrimSpace(strings.TrimPrefix(line, "#EXTGRP:"))
				}
			} else if strings.HasPrefix(line, "#EXTVLCOPT:") {
				// #EXTVLCOPT:http-user-agent=... / http-referrer=... 形式にも対応する
				option := strings.TrimSpace(strings.TrimPrefix(line, "#EXTVLCOPT:"))
				if index := strings.Index(option, "="); index >= 0 {
					key := strings.ToLower(strings.TrimSpace(option[:index]))
					value := strings.TrimSpace(option[index+1:])
					switch key {
					case "http-user-agent":
						currentAttributes["http-user-agent"] = value
					case "http-referrer":
						currentAttributes["http-referrer"] = value
					}
				}
			}
			continue
		}

		// ここに到達する行は URL
		streamURL := NormalizeURL(sourceURL, line)

		// #EXTINF が無いまま URL が来た場合は、前のチャンネル名などを引き継がないようにリセットする
		name := ""
		if currentName != nil && *currentName != "" {
			name = *currentName
		} else if tvgName, ok := currentAttributes["tvg-name"]; ok && tvgName != "" {
			name = tvgName
		} else if parsed, err := url.Parse(streamURL); err == nil && parsed.Host != "" {
			name = parsed.Host
		} else {
			name = streamURL
		}
		attributes := currentAttributes
		currentAttributes = map[string]string{}
		currentName = nil

		// 同一 URL の重複を除外
		if seenURLs[streamURL] {
			continue
		}
		seenURLs[streamURL] = true

		// ジオブロック (地域制限) の表記を検出する
		isGeoBlocked := strings.Contains(name, "[Geo-blocked]")

		// 国コードを推定する
		country := deriveCountryCode(attributes, sourceURL)
		countryInfo := CountryInfo{}
		if country != nil {
			countryInfo = countriesByCode[*country]
		}

		channel := &Channel{
			ID:           BuildChannelID(streamURL),
			Name:         name,
			URL:          streamURL,
			LogoURL:      optionalAttribute(attributes, "tvg-logo"),
			Group:        optionalAttribute(attributes, "group-title"),
			Country:      country,
			TvgID:        optionalAttribute(attributes, "tvg-id"),
			Language:     optionalAttribute(attributes, "tvg-language"),
			UserAgent:    optionalAttribute(attributes, "http-user-agent"),
			Referrer:     optionalAttribute(attributes, "http-referrer"),
			IsGeoBlocked: isGeoBlocked,
			SourceURL:    sourceURL,
		}
		if countryInfo.Name != "" {
			name := countryInfo.Name
			channel.CountryName = &name
		}
		if countryInfo.Flag != "" {
			flag := countryInfo.Flag
			channel.CountryFlag = &flag
		}
		channels = append(channels, channel)
	}

	return channels
}

// deriveCountryCode は EXTINF の属性とプレイリストの URL から、チャンネルの国コードを推定する。
//
// 推定の優先順位は tvg-country > tvg-id > プレイリストの URL の順。
func deriveCountryCode(attributes map[string]string, sourceURL string) *string {
	// tvg-country が指定されている場合 (複数指定されている場合は先頭を使う)
	if tvgCountry, ok := attributes["tvg-country"]; ok {
		code := splitCountryCodes(tvgCountry)
		if len(code) == 2 {
			upper := strings.ToUpper(code)
			return &upper
		}
	}

	// tvg-id (例: "NHKWorldJapan.jp@SD") から抽出する
	if tvgID, ok := attributes["tvg-id"]; ok {
		if matched := tvgIDCountryPattern.FindStringSubmatch(tvgID); matched != nil {
			upper := strings.ToUpper(matched[1])
			return &upper
		}
	}

	// プレイリストの URL (例: ".../countries/jp.m3u") から抽出する
	if matched := sourceURLCountryPattern.FindStringSubmatch(sourceURL); matched != nil {
		upper := strings.ToUpper(matched[1])
		return &upper
	}

	return nil
}

// splitCountryCodes は tvg-country の値 ('JP' や 'JP;US' など) から先頭の国コードを取り出す。
func splitCountryCodes(value string) string {
	codes := strings.FieldsFunc(strings.TrimSpace(value), func(character rune) bool {
		return character == ';' || character == ',' || unicode.IsSpace(character)
	})
	if len(codes) == 0 {
		return ""
	}
	return codes[0]
}

// optionalAttribute は属性値を取得し、空文字の場合は nil を返す。
func optionalAttribute(attributes map[string]string, key string) *string {
	value, exists := attributes[key]
	if !exists || value == "" {
		return nil
	}
	return &value
}

// RewriteHLSPlaylist は HLS プレイリスト内の URI を、すべて KonomiTV サーバーのプロキシ API 経由に書き換える。
//
//   - セグメントや子プレイリストの URI (タグではない行) を書き換える
//   - #EXT-X-KEY / #EXT-X-MAP などのタグ内の URI="..." を書き換える
func RewriteHLSPlaylist(content string, baseURL string) string {
	rewrittenLines := make([]string, 0)
	for _, line := range SplitLines(content) {
		stripped := strings.TrimSpace(line)

		if stripped == "" {
			rewrittenLines = append(rewrittenLines, "")
			continue
		}

		// タグ行の場合
		if strings.HasPrefix(stripped, "#") {
			// タグ内に URI="..." がある場合は書き換える
			if strings.Contains(line, `URI="`) {
				line = playlistURIPattern.ReplaceAllStringFunc(line, func(matched string) string {
					sub := playlistURIPattern.FindStringSubmatch(matched)
					return `URI="` + BuildProxyURL(NormalizeURL(baseURL, sub[1])) + `"`
				})
			}
			rewrittenLines = append(rewrittenLines, line)
			continue
		}

		// それ以外の行は URI なのでプロキシ経由に書き換える
		rewrittenLines = append(rewrittenLines, BuildProxyURL(NormalizeURL(baseURL, stripped)))
	}

	return strings.Join(rewrittenLines, "\n") + "\n"
}

// BuildM3UPlaylist はチャンネル一覧を、外部プレイヤーでも利用できる M3U プレイリストとして出力する。
func BuildM3UPlaylist(channels []*Channel) string {
	lines := []string{"#EXTM3U"}
	for _, channel := range channels {
		attributes := ""
		if channel.TvgID != nil {
			attributes += ` tvg-id="` + *channel.TvgID + `"`
		}
		if channel.LogoURL != nil {
			attributes += ` tvg-logo="` + *channel.LogoURL + `"`
		}
		if channel.Group != nil {
			attributes += ` group-title="` + *channel.Group + `"`
		}
		if channel.Country != nil {
			attributes += ` tvg-country="` + *channel.Country + `"`
		}
		lines = append(lines, "#EXTINF:-1"+attributes+","+channel.Name)
		// 外部プレイヤーが KonomiTV サーバー経由でアクセスできるよう、プロキシの URL を出力する
		lines = append(lines, BuildProxyURL(channel.URL))
	}
	return strings.Join(lines, "\n") + "\n"
}

// SplitLines は文字列を行に分割する (Python の str.splitlines() と等価) 。
//
// 末尾の改行で生じる空要素は取り除くため、"a\n" は ['a'] になる。
func SplitLines(content string) []string {
	lines := newlinePattern.Split(content, -1)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
