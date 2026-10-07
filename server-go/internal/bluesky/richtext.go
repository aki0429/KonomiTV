// Package bluesky は Bluesky (AT Protocol) の API クライアントを提供する。
//
// Python 版 server/app/utils/BlueskyAPI.py の移植。atproto SDK に相当する部分は
// net/http による XRPC クライアントとして自前実装している (Go に atproto SDK は存在しない) 。
package bluesky

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// ***** Bluesky handle の正規化 *****

// profileURLPrefixes は bsky.app のプロフィール URL として扱う接頭辞。
var profileURLPrefixes = []string{"https://bsky.app/profile/", "http://bsky.app/profile/"}

// NormalizeHandle はユーザー入力の Bluesky handle を API が扱う形式に正規化する。
// Python 版 BlueskyAPI.normalizeBlueskyHandle() と同じく、前後の空白と先頭の @ を除去し、
// bsky.app のプロフィール URL も受け付けて小文字化する。
func NormalizeHandle(handle string) string {
	normalized := strings.TrimSpace(handle)
	for _, prefix := range profileURLPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			normalized = strings.TrimPrefix(normalized, prefix)
			if index := strings.IndexByte(normalized, '/'); index >= 0 {
				normalized = normalized[:index]
			}
			if index := strings.IndexByte(normalized, '?'); index >= 0 {
				normalized = normalized[:index]
			}
			break
		}
	}
	if strings.HasPrefix(normalized, "@") {
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "@"))
	}
	return strings.ToLower(normalized)
}

// ExtractRecordKey は AT URI から record key を取り出す。
// Python 版 BlueskyAPI._extractRecordKey() と同じく最後のスラッシュ以降を返す。
func ExtractRecordKey(uri string) string {
	if index := strings.LastIndexByte(uri, '/'); index >= 0 {
		return uri[index+1:]
	}
	return uri
}

// ***** facets (リンク / ハッシュタグ) の構築 *****

// urlTrailingPunctuations は URL の末尾に張り付きがちな句読点・閉じ括弧の集合。
// Python 版の URL_TRAILING_PUNCTUATIONS と同一。
const urlTrailingPunctuations = "。、,.;:!?！？)）」』]>"

// FacetFeature は facet の feature (link / tag) 。
type FacetFeature struct {
	Type string `json:"$type"`
	// Link のときのみ設定される
	URI string `json:"uri,omitempty"`
	// Tag のときのみ設定される
	Tag string `json:"tag,omitempty"`
}

// ByteSlice は facet が指す本文上の byte range 。
type ByteSlice struct {
	Type      string `json:"$type"`
	ByteStart int    `json:"byteStart"`
	ByteEnd   int    `json:"byteEnd"`
}

// Facet は Bluesky のリッチテキスト装飾 (app.bsky.richtext.facet) 。
type Facet struct {
	Features []FacetFeature `json:"features"`
	Index    ByteSlice      `json:"index"`
	Type     string         `json:"$type"`
}

// BuildFacets は本文中の URL とハッシュタグだけを facets 化する。
// Python 版 BlueskyAPI._buildTextBuilder() の移植で、本文の全文字列と facets を返す。
func BuildFacets(text string) (string, []Facet) {
	facets := make([]Facet, 0)

	// 本文の byte offset を追いながら、URL / ハッシュタグの出現位置を facets にする
	cursor := 0
	buildText := strings.Builder{}
	buildText.Grow(len(text))
	// 現在までに書き込んだ byte 長 (facet の byteStart に使う)
	buildLength := 0

	appendText := func(value string) {
		buildText.WriteString(value)
		buildLength += len(value)
	}

	for _, match := range scanTokens(text) {
		if match.start > cursor {
			appendText(text[cursor:match.start])
		}
		token := text[match.start:match.end]
		cursor = match.end

		// URL の末尾に張り付いた句読点・閉じ括弧は本文側に戻す
		token, trailing := splitTrailingPunctuations(token)
		if match.isURL {
			if token != "" {
				start := buildLength
				appendText(token)
				facets = append(facets, Facet{
					Features: []FacetFeature{{Type: "app.bsky.richtext.facet#link", URI: token}},
					Index:    ByteSlice{Type: "app.bsky.richtext.facet#byteSlice", ByteStart: start, ByteEnd: start + len(token)},
					Type:     "app.bsky.richtext.facet",
				})
			}
			if trailing != "" {
				appendText(trailing)
			}
			continue
		}

		// ハッシュタグの facet には # を含まないタグ名を渡す必要がある
		if token != "" {
			tagText := strings.TrimLeft(token, "#＃")
			if tagText != "" {
				start := buildLength
				appendText(token)
				facets = append(facets, Facet{
					Features: []FacetFeature{{Type: "app.bsky.richtext.facet#tag", Tag: tagText}},
					Index:    ByteSlice{Type: "app.bsky.richtext.facet#byteSlice", ByteStart: start, ByteEnd: start + len(token)},
					Type:     "app.bsky.richtext.facet",
				})
			} else {
				// 記号だけのハッシュタグは facet として意味を持たないので本文として残す
				appendText(token)
			}
		}
		if trailing != "" {
			appendText(trailing)
		}
	}

	if cursor < len(text) {
		appendText(text[cursor:])
	}

	return buildText.String(), facets
}

// tokenMatch は URL / ハッシュタグの byte range 。
type tokenMatch struct {
	start int
	end   int
	isURL bool
}

// scanTokens は本文から URL とハッシュタグの byte range を本文順に列挙する。
// Python 版の正規表現 r'(https?://[^\s]+)|([#＃]([^\s#＃]+))' と同じ範囲を返す。
func scanTokens(text string) []tokenMatch {
	matches := make([]tokenMatch, 0)

	// 各 rune の byte offset を先に求めておく (byte 長と rune 位置を混在させない)
	byteOffsets := make([]int, 0, len(text))
	runes := make([]rune, 0, len(text))
	byteIndex := 0
	for _, char := range text {
		byteOffsets = append(byteOffsets, byteIndex)
		runes = append(runes, char)
		byteIndex += utf8.RuneLen(char)
	}
	byteOffsets = append(byteOffsets, byteIndex)

	for index := 0; index < len(runes); {
		if isURLStart(runes, index) {
			end := index
			for end < len(runes) && !isSpaceRune(runes[end]) {
				end++
			}
			matches = append(matches, tokenMatch{start: byteOffsets[index], end: byteOffsets[end], isURL: true})
			index = end
			continue
		}
		if runes[index] == '#' || runes[index] == '＃' {
			end := index + 1
			for end < len(runes) && !isSpaceRune(runes[end]) && runes[end] != '#' && runes[end] != '＃' {
				end++
			}
			if end > index+1 {
				matches = append(matches, tokenMatch{start: byteOffsets[index], end: byteOffsets[end], isURL: false})
				index = end
				continue
			}
		}
		index++
	}
	return matches
}

// isURLStart は runes[index:] が http:// または https:// で始まるかどうかを返す。
func isURLStart(runes []rune, index int) bool {
	for _, prefix := range []string{"http://", "https://"} {
		if hasRunePrefix(runes, index, prefix) {
			return true
		}
	}
	return false
}

// hasRunePrefix は runes の指定位置以降が ASCII 文字列 prefix と一致するかどうかを返す。
func hasRunePrefix(runes []rune, index int, prefix string) bool {
	if index+len(prefix) > len(runes) {
		return false
	}
	for offset, char := range prefix {
		if runes[index+offset] != char {
			return false
		}
	}
	return true
}

// isSpaceRune は Python の正規表現 \s (Unicode 空白) と同じ判定を返す。
func isSpaceRune(char rune) bool {
	switch char {
	case ' ', '\t', '\n', '\v', '\f', '\r',
		'\u0085', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000':
		return true
	}
	if char >= '\u2000' && char <= '\u200a' {
		return true
	}
	return false
}

// splitTrailingPunctuations は末尾の句読点・閉じ括弧を本文側へ押し戻す。
func splitTrailingPunctuations(token string) (string, string) {
	trailing := ""
	for token != "" {
		lastRune, size := utf8.DecodeLastRuneInString(token)
		if !strings.ContainsRune(urlTrailingPunctuations, lastRune) {
			break
		}
		trailing = token[len(token)-size:] + trailing
		token = token[:len(token)-size]
	}
	return token, trailing
}

// ***** facet の link 本文展開 *****

// ExpandFacetLinksInText は Bluesky の link facet が持つ実 URL を表示用テキストへ反映する。
// Python 版 BlueskyAPI._expandFacetLinksInText() の移植。
func ExpandFacetLinksInText(text string, facets []Facet) string {
	if len(facets) == 0 {
		return text
	}

	type replacement struct {
		start int
		end   int
		uri   string
	}
	replacements := make([]replacement, 0, len(facets))
	textBytes := []byte(text)

	for _, facet := range facets {
		linkURI := ""
		for _, feature := range facet.Features {
			if feature.Type == "app.bsky.richtext.facet#link" {
				linkURI = feature.URI
				break
			}
		}
		if linkURI == "" {
			continue
		}
		byteStart := facet.Index.ByteStart
		byteEnd := facet.Index.ByteEnd

		// PDS 由来の byte slice が壊れている場合は表示本文を壊さず、その facet だけを無視する
		if byteStart < 0 || byteEnd <= byteStart || byteEnd > len(textBytes) {
			continue
		}
		replacements = append(replacements, replacement{start: byteStart, end: byteEnd, uri: linkURI})
	}

	if len(replacements) == 0 {
		return text
	}

	// byte slice の後ろから置換すると、前方 facet の byte offset を維持したまま安全に差し替えられる
	sort.SliceStable(replacements, func(left, right int) bool {
		return replacements[left].start > replacements[right].start
	})

	expanded := make([]byte, 0, len(textBytes))
	expanded = append(expanded, textBytes...)
	for _, item := range replacements {
		// Python の bytearray slice 代入と同じく、範囲外は切り詰めて扱う
		start := item.start
		end := item.end
		if start > len(expanded) {
			start = len(expanded)
		}
		if end > len(expanded) {
			end = len(expanded)
		}
		replaced := make([]byte, 0, len(expanded)-(end-start)+len(item.uri))
		replaced = append(replaced, expanded[:start]...)
		replaced = append(replaced, item.uri...)
		replaced = append(replaced, expanded[end:]...)
		expanded = replaced
	}
	return string(expanded)
}
