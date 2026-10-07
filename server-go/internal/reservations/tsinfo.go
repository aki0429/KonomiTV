package reservations

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// このファイルは server/app/utils/TSInformation.py のうち録画予約系 API が使う関数を移植したもの。
//
// formatString() の変換表は internal/metadata/format_table.go と同じ内容だが、
// 同パッケージの変数が非公開のため、ここにも同じ表を持つ (両者は同じ Python コードから機械的に作っている) 。

// formatRuneTable は 1 文字単位の置換表 (Python の str.translate 相当) 。
var formatRuneTable = map[rune]string{
	0xff10: "0", 0xff11: "1", 0xff12: "2", 0xff13: "3", 0xff14: "4",
	0xff15: "5", 0xff16: "6", 0xff17: "7", 0xff18: "8", 0xff19: "9",
	0xff21: "A", 0xff22: "B", 0xff23: "C", 0xff24: "D", 0xff25: "E",
	0xff26: "F", 0xff27: "G", 0xff28: "H", 0xff29: "I", 0xff2a: "J",
	0xff2b: "K", 0xff2c: "L", 0xff2d: "M", 0xff2e: "N", 0xff2f: "O",
	0xff30: "P", 0xff31: "Q", 0xff32: "R", 0xff33: "S", 0xff34: "T",
	0xff35: "U", 0xff36: "V", 0xff37: "W", 0xff38: "X", 0xff39: "Y",
	0xff3a: "Z", 0xff41: "a", 0xff42: "b", 0xff43: "c", 0xff44: "d",
	0xff45: "e", 0xff46: "f", 0xff47: "g", 0xff48: "h", 0xff49: "i",
	0xff4a: "j", 0xff4b: "k", 0xff4c: "l", 0xff4d: "m", 0xff4e: "n",
	0xff4f: "o", 0xff50: "p", 0xff51: "q", 0xff52: "r", 0xff53: "s",
	0xff54: "t", 0xff55: "u", 0xff56: "v", 0xff57: "w", 0xff58: "x",
	0xff59: "y", 0xff5a: "z",
	0xff02: "\"", 0xff03: "#", 0xff04: "$", 0xff05: "%", 0xff06: "&",
	0xff07: "'", 0xff08: "(", 0xff09: ")", 0xff0b: "+", 0xff0c: ",",
	0xff0d: "-", 0xff0e: ".", 0xff0f: "/", 0xff1a: ":", 0xff1b: ";",
	0xff1c: "<", 0xff1d: "=", 0xff1e: ">", 0xff3b: "[", 0xff3c: "\\",
	0xff3d: "]", 0xff3e: "^", 0xff3f: "_", 0xff40: "`", 0xff5b: "{",
	0xff5c: "|", 0xff5d: "}", 0x3000: " ",
	0x21: "！", 0x3f: "？", 0x2a: "＊", 0x7e: "～", 0x266f: "#", 0x301c: "～",
	0x1f14a: "[HV]", 0x1f14c: "[SD]", 0x1f13f: "[P]", 0x1f146: "[W]", 0x1f14b: "[MV]",
	0x1f210: "[手]", 0x1f211: "[字]", 0x1f212: "[双]", 0x1f213: "[デ]", 0x1f142: "[S]",
	0x1f214: "[二]", 0x1f215: "[多]", 0x1f216: "[解]", 0x1f14d: "[SS]", 0x1f131: "[B]",
	0x1f13d: "[N]", 0x1f217: "[天]", 0x1f218: "[交]", 0x1f219: "[映]", 0x1f21a: "[無]",
	0x1f21b: "[料]", 0x1f21c: "[前]", 0x1f21d: "[後]", 0x1f21e: "[再]", 0x1f21f: "[新]",
	0x1f220: "[初]", 0x1f221: "[終]", 0x1f222: "[生]", 0x1f223: "[販]", 0x1f224: "[声]",
	0x1f225: "[吹]", 0x1f14e: "[PPV]", 0x1f200: "[ほか]", 0x1f19b: "[3D]", 0x1f19c: "[2ndScr]",
	0x1f19d: "[2K]", 0x1f19e: "[4K]", 0x1f19f: "[8K]", 0x1f1a0: "[5.1]", 0x1f1a1: "[7.1]",
	0x1f1a2: "[22.2]", 0x1f1a3: "[60P]", 0x1f1a4: "[120P]", 0x1f1a5: "[d]", 0x1f1a6: "[HC]",
	0x1f1a7: "[HDR]", 0x1f1a8: "[Hi-Res]", 0x1f1a9: "[Lossless]", 0x1f1aa: "[SHV]",
	0x1f1ab: "[UHD]", 0x1f1ac: "[VOD]", 0x1f23b: "[配]",
}

// formatRegexPairs は文字列置換表 (Python の __format_string_regex_table) 。
// 置換の探索順が結果に影響するため、順序付きのスライスで保持する。
var formatRegexPairs = [][2]string{
	{"(秘)", "㊙"},
	{"m^2", "m²"},
	{"m^3", "m³"},
	{"cm^2", "cm²"},
	{"cm^3", "cm³"},
	{"km^2", "km²"},
	{"[社]", "㈳"},
	{"[財]", "㈶"},
	{"[有]", "㈲"},
	{"[株]", "㈱"},
	{"[代]", "㈹"},
	{"^2", "²"},
	{"^3", "³"},
	{"(〒)", "〶"},
	{"()()", "⚾"},
}

// formatStringRegex は formatRegexPairs を Python の re.compile("|".join(...)) と同じ形にしたもの。
var formatStringRegex = func() *regexp.Regexp {
	parts := make([]string, 0, len(formatRegexPairs))
	replacements := map[string]string{}
	for _, pair := range formatRegexPairs {
		parts = append(parts, regexp.QuoteMeta(pair[0]))
		replacements[pair[0]] = pair[1]
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}()

// FormatString は文字列に含まれる英数や記号を半角に置換し、一律な表現に整える。
//
// 移植元: TSInformation.formatString()
func FormatString(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if replacement, ok := formatRuneTable[r]; ok {
			builder.WriteString(replacement)
			continue
		}
		builder.WriteRune(r)
	}
	result := builder.String()
	return formatStringRegex.ReplaceAllStringFunc(result, func(match string) string {
		for _, pair := range formatRegexPairs {
			if pair[0] == match {
				return pair[1]
			}
		}
		return match
	})
}

// GetNetworkType はネットワーク ID からネットワークの種別を取得する。
//
// 移植元: TSInformation.getNetworkType()
// 不明なネットワーク ID の場合は "OTHER" を返す。
func GetNetworkType(networkID int) string {
	// 地上デジタルテレビジョン放送 (network_id: 30848 ~ 32744)
	if networkID >= 0x7880 && networkID <= 0x7FE8 {
		return "GR"
	}
	// BSデジタル放送
	if networkID == 0x0004 {
		return "BS"
	}
	// 110度CSデジタル放送
	if networkID == 0x0006 || networkID == 0x0007 {
		return "CS"
	}
	// ケーブルテレビ (リマックス方式・トランスモジュレーション方式)
	if networkID == 0xFFFE || networkID == 0xFFFA || networkID == 0xFFFD ||
		networkID == 0xFFF9 || networkID == 0xFFF7 {
		return "CATV"
	}
	// 124/128度CSデジタル放送
	if networkID == 0x000A || networkID == 0x0001 || networkID == 0x0003 {
		return "SKY"
	}
	// 高度BSデジタル放送・高度110度CSデジタル放送
	if networkID == 0x000B || networkID == 0x000C {
		return "BS4K"
	}
	return "OTHER"
}

// CalculateRemoconID はサービス ID からチャンネルのリモコン番号を算出する (地デジ以外向け) 。
//
// 移植元: TSInformation.calculateRemoconID()
func CalculateRemoconID(channelType string, serviceID int) int {
	remoconID := serviceID
	if channelType == "BS" {
		switch {
		case serviceID >= 101 && serviceID <= 102:
			remoconID = 1
		case serviceID >= 103 && serviceID <= 104:
			remoconID = 3
		case serviceID >= 141 && serviceID <= 149:
			remoconID = 4
		case serviceID >= 151 && serviceID <= 159:
			remoconID = 5
		case serviceID >= 161 && serviceID <= 169:
			remoconID = 6
		case serviceID >= 171 && serviceID <= 179:
			remoconID = 7
		case serviceID >= 181 && serviceID <= 189:
			remoconID = 8
		case serviceID >= 191 && serviceID <= 193:
			remoconID = 9
		case serviceID >= 200 && serviceID <= 202:
			remoconID = 10
		case serviceID == 211:
			remoconID = 11
		case serviceID == 222:
			remoconID = 12
		}
	} else if channelType == "SKY" {
		remoconID = serviceID % 1024
	}
	return remoconID
}

// CalculateChannelNumber はチャンネルの3桁チャンネル番号を算出する (ex: 011, 031-1, 211) 。
//
// 移植元: TSInformation.calculateChannelNumber()
// 地デジの枝番処理ではデータベースを参照するため db を必要とする。
func CalculateChannelNumber(ctx context.Context, db *sql.DB, channelType string, networkID int, serviceID int, remoconID int) (string, error) {
	channelNumber := zeroPad(serviceID, 3)

	switch channelType {
	case "GR":
		// 同じネットワーク内にあるサービスのカウント (サービス番号のみを取り出す)
		sameNetworkIDCount := (serviceID & 0x0007) + 1
		channelNumber = zeroPad(remoconID, 2) + strconv.Itoa(sameNetworkIDCount)

		if db == nil {
			return channelNumber, nil
		}
		// 同じベースチャンネル番号を持つサービスを DB から取得する
		pattern := escapeLikePattern(channelNumber) + "-%"
		rows, err := db.QueryContext(
			ctx,
			`SELECT channel_number FROM channels
			 WHERE NOT (network_id = ? AND service_id = ?)
			   AND (channel_number = ? OR channel_number LIKE ? ESCAPE '\')
			   AND type = 'GR'`,
			networkID, serviceID, channelNumber, pattern,
		)
		if err != nil {
			return "", fmt.Errorf("failed to get same channel numbers: %w", err)
		}
		defer func() { _ = rows.Close() }()
		branchPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(channelNumber) + `-([0-9]+)$`)
		isBaseUsed := false
		usedBranches := map[int]bool{}
		for rows.Next() {
			var sameChannelNumber string
			if err := rows.Scan(&sameChannelNumber); err != nil {
				return "", fmt.Errorf("failed to scan channel_number: %w", err)
			}
			if sameChannelNumber == channelNumber {
				isBaseUsed = true
				continue
			}
			if match := branchPattern.FindStringSubmatch(sameChannelNumber); match != nil {
				branch, err := strconv.Atoi(match[1])
				if err != nil {
					continue
				}
				usedBranches[branch] = true
			}
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("failed to iterate channel_number rows: %w", err)
		}
		if isBaseUsed || len(usedBranches) > 0 {
			branch := 1
			for usedBranches[branch] {
				branch++
			}
			channelNumber += "-" + strconv.Itoa(branch)
		}
	case "SKY":
		channelNumber = zeroPad(serviceID%1024, 3)
	}
	return channelNumber, nil
}

// escapeLikePattern は SQL の LIKE のワイルドカードをエスケープする。
func escapeLikePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// CalculateIsSubchannel はチャンネルがサブチャンネルかどうかを算出する。
//
// 移植元: TSInformation.calculateIsSubchannel()
func CalculateIsSubchannel(channelType string, serviceID int) bool {
	if channelType == "GR" {
		return (serviceID & 0x0187) != 0
	}
	if channelType == "BS" {
		if serviceID == 102 || serviceID == 104 {
			return true
		}
		if serviceID >= 142 && serviceID <= 149 {
			return true
		}
		if serviceID >= 152 && serviceID <= 159 {
			return true
		}
		if serviceID >= 162 && serviceID <= 169 {
			return true
		}
		if serviceID >= 172 && serviceID <= 179 {
			return true
		}
		if serviceID >= 182 && serviceID <= 189 {
			return true
		}
		if serviceID == 232 || serviceID == 233 {
			return true
		}
	}
	return false
}
