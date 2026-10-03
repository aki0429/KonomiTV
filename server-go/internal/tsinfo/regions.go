// Package tsinfo は日本の放送波 (MPEG-TS) に関するユーティリティを提供する。
// server/app/utils/TSInformation.py のうち、チャンネル API で必要な範囲を移植したもの。
package tsinfo

// terrestrialRegionToRegionIDs は地域名 → 対応する地域識別のリスト (県域 + 広域) 。
// 北海道は放送エリアごとに地域識別が異なるため個別に分割している。
// ARIB TR-B14 第五分冊 第七編 9.1「各種数値割り当て一覧」に基づく。
// 順序は Python 版の TERRESTRIAL_REGION_TO_REGION_IDS の定義順 (逆引きリストの順序に影響する) 。
var terrestrialRegionToRegionIDs = []struct {
	Name string
	IDs  []int
}{
	// 北海道（地域識別が異なる7つの放送エリア + 北海道域）
	{"北海道（札幌）", []int{10, 4}}, // 札幌 + 北海道域
	{"北海道（函館）", []int{11, 4}}, // 函館 + 北海道域
	{"北海道（旭川）", []int{12, 4}}, // 旭川 + 北海道域
	{"北海道（帯広）", []int{13, 4}}, // 帯広 + 北海道域
	{"北海道（釧路）", []int{14, 4}}, // 釧路 + 北海道域
	{"北海道（北見）", []int{15, 4}}, // 北見 + 北海道域
	{"北海道（室蘭）", []int{16, 4}}, // 室蘭 + 北海道域
	// 東北
	{"青森県", []int{22}},
	{"岩手県", []int{20}},
	{"宮城県", []int{17}},
	{"秋田県", []int{18}},
	{"山形県", []int{19}},
	{"福島県", []int{21}},
	// 関東（関東広域を含む）
	{"茨城県", []int{26, 1}},
	{"栃木県", []int{28, 1}},
	{"群馬県", []int{25, 1}},
	{"埼玉県", []int{29, 1}},
	{"千葉県", []int{27, 1}},
	{"東京都", []int{23, 1}},
	{"神奈川県", []int{24, 1}},
	// 甲信越・北陸
	{"新潟県", []int{31}},
	{"富山県", []int{37}},
	{"石川県", []int{34}},
	{"福井県", []int{36}},
	{"山梨県", []int{32}},
	{"長野県", []int{30}},
	// 東海（中京広域を含む）
	{"静岡県", []int{35}},
	{"愛知県", []int{33, 3}},
	{"岐阜県", []int{39, 3}},
	{"三重県", []int{38, 3}},
	// 近畿（近畿広域を含む）
	{"滋賀県", []int{45, 2}},
	{"京都府", []int{41, 2}},
	{"大阪府", []int{40, 2}},
	{"兵庫県", []int{42, 2}},
	{"奈良県", []int{44, 2}},
	{"和歌山県", []int{43, 2}},
	// 中国（岡山香川・島根鳥取を含む）
	{"鳥取県", []int{49, 6}},
	{"島根県", []int{48, 6}},
	{"岡山県", []int{47, 5}},
	{"広島県", []int{46}},
	{"山口県", []int{50}},
	// 四国（岡山香川を含む）
	{"徳島県", []int{53}},
	{"香川県", []int{52, 5}},
	{"愛媛県", []int{51}},
	{"高知県", []int{54}},
	// 九州・沖縄
	{"福岡県", []int{55}},
	{"佐賀県", []int{61}},
	{"長崎県", []int{57}},
	{"熊本県", []int{56}},
	{"大分県", []int{60}},
	{"宮崎県", []int{59}},
	{"鹿児島県", []int{58}},
	{"沖縄県", []int{62}},
}

// regionIDToRegionNames は地域識別 → 対応する地域名のリスト (逆引きマッピング) 。
// terrestrialRegionToRegionIDs から起動時に構築する。
var regionIDToRegionNames = buildRegionIDToRegionNames()

// buildRegionIDToRegionNames は逆引きマッピングを構築する。
func buildRegionIDToRegionNames() map[int][]string {
	result := map[int][]string{}
	for _, entry := range terrestrialRegionToRegionIDs {
		for _, regionID := range entry.IDs {
			result[regionID] = append(result[regionID], entry.Name)
		}
	}
	return result
}

// GetRegionIDFromNetworkID は地デジのネットワーク ID から該当する地域識別を取得する。
// 地デジ以外または不明な場合は 0 (Python 版の None 相当) を返す。
//
// ARIB TR-B14 第五分冊 第七編 9.1 より:
// network_id = 0x7FF0 - 0x0010 × 地域識別 + 地域事業者識別 - 0x0400 × 県複フラグ
func GetRegionIDFromNetworkID(networkID int) int {
	// 地デジの NID 範囲チェック
	// 県複フラグ=0: 0x7C10 ~ 0x7FEF
	// 県複フラグ=1: 0x7810 ~ 0x7BEF
	if !(0x7800 <= networkID && networkID <= 0x7FF0) {
		return 0
	}

	// 県複フラグの判定と補正
	// NID < 0x7C00 なら県複フラグ=1 と判断し、0x0400 を加算して正規化する
	if networkID < 0x7C00 {
		networkID += 0x0400
	}

	// 地域識別の計算
	// 地域事業者識別は 0〜15 なので、0x0010 で割る場合は切り捨てではなく切り上げが必要
	// (region_broadcaster_id が 0 以外だと、切り捨てでは地域識別が 1 ずれる)
	regionID := (0x7FF0 - networkID + 0x000F) / 0x0010
	if 1 <= regionID && regionID <= 62 {
		return regionID
	}
	return 0
}

// TerrestrialRegionToRegionIDs は地域名に対応する地域識別のリストを返す。
// Python 版の TSInformation.TERRESTRIAL_REGION_TO_REGION_IDS 相当。
func TerrestrialRegionToRegionIDs(regionName string) []int {
	for _, entry := range terrestrialRegionToRegionIDs {
		if entry.Name == regionName {
			return entry.IDs
		}
	}
	return nil
}

// GetRegionNamesFromNetworkID は地デジのネットワーク ID から該当するすべての地域名を取得する。
// 広域放送局の場合はその広域に含まれるすべての都道府県名を返す。
// 地デジ以外または不明な場合は nil (Python 版の None 相当) を返す。
func GetRegionNamesFromNetworkID(networkID int) []string {
	regionID := GetRegionIDFromNetworkID(networkID)
	if regionID == 0 {
		return nil
	}
	regionNames := regionIDToRegionNames[regionID]
	if regionNames == nil {
		return nil
	}
	// 呼び出し元での変更を防ぐためコピーを返す
	result := make([]string, len(regionNames))
	copy(result, regionNames)
	return result
}
