package reservations

// このファイルは ariblib.constants の CONTENT_TYPE / USER_TYPE から機械的に抽出したテーブル。
// 生成スクリプト: internal/reservations/gen_arib_genre_table.py (手動実行) 。
//
// ReservationConditionsRouter.py のジャンル変換で使う。変換結果は辞書の挿入順に依存するため、
// Go 側は map ではなく順序付きのスライスで保持する。

// aribGenreMiddleEntry は中分類 (content_nibble_level2) の 1 エントリ。
type aribGenreMiddleEntry struct {
	Key  int
	Name string
}

// aribGenreEntry は大分類 (content_nibble_level1) の 1 エントリ。
type aribGenreEntry struct {
	Key    int
	Major  string
	Middle []aribGenreMiddleEntry
}

// aribContentType は ariblib.constants.CONTENT_TYPE 相当 (挿入順を保持) 。
var aribContentType = []aribGenreEntry{
	{Key: 0, Major: "ニュース／報道", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "定時・総合"},
		{Key: 1, Name: "天気"},
		{Key: 2, Name: "特集・ドキュメント"},
		{Key: 3, Name: "政治・国会"},
		{Key: 4, Name: "経済・市況"},
		{Key: 5, Name: "海外・国際"},
		{Key: 6, Name: "解説"},
		{Key: 7, Name: "討論・会談"},
		{Key: 8, Name: "報道特番"},
		{Key: 9, Name: "ローカル・地域"},
		{Key: 10, Name: "交通"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 1, Major: "スポーツ", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "スポーツニュース"},
		{Key: 1, Name: "野球"},
		{Key: 2, Name: "サッカー"},
		{Key: 3, Name: "ゴルフ"},
		{Key: 4, Name: "その他の球技"},
		{Key: 5, Name: "相撲・格闘技"},
		{Key: 6, Name: "オリンピック・国際大会"},
		{Key: 7, Name: "マラソン・陸上・水泳"},
		{Key: 8, Name: "モータースポーツ"},
		{Key: 9, Name: "マリン・ウィンタースポーツ"},
		{Key: 10, Name: "競馬・公営競技"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 2, Major: "情報／ワイドショー", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "芸能・ワイドショー"},
		{Key: 1, Name: "ファッション"},
		{Key: 2, Name: "暮らし・住まい"},
		{Key: 3, Name: "健康・医療"},
		{Key: 4, Name: "ショッピング・通販"},
		{Key: 5, Name: "グルメ・料理"},
		{Key: 6, Name: "イベント"},
		{Key: 7, Name: "番組紹介・お知らせ"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 3, Major: "ドラマ", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "国内ドラマ"},
		{Key: 1, Name: "海外ドラマ"},
		{Key: 2, Name: "時代劇"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 4, Major: "音楽", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "国内ロック・ポップス"},
		{Key: 1, Name: "海外ロック・ポップス"},
		{Key: 2, Name: "クラシック・オペラ"},
		{Key: 3, Name: "ジャズ・フュージョン"},
		{Key: 4, Name: "歌謡曲・演歌"},
		{Key: 5, Name: "ライブ・コンサート"},
		{Key: 6, Name: "ランキング・リクエスト"},
		{Key: 7, Name: "カラオケ・のど自慢"},
		{Key: 8, Name: "民謡・邦楽"},
		{Key: 9, Name: "童謡・キッズ"},
		{Key: 10, Name: "民族音楽・ワールドミュージック"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 5, Major: "バラエティ", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "クイズ"},
		{Key: 1, Name: "ゲーム"},
		{Key: 2, Name: "トークバラエティ"},
		{Key: 3, Name: "お笑い・コメディ"},
		{Key: 4, Name: "音楽バラエティ"},
		{Key: 5, Name: "旅バラエティ"},
		{Key: 6, Name: "料理バラエティ"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 6, Major: "映画", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "洋画"},
		{Key: 1, Name: "邦画"},
		{Key: 2, Name: "アニメ"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 7, Major: "アニメ／特撮", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "国内アニメ"},
		{Key: 1, Name: "海外アニメ"},
		{Key: 2, Name: "特撮"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 8, Major: "ドキュメンタリー／教養", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "社会・時事"},
		{Key: 1, Name: "歴史・紀行"},
		{Key: 2, Name: "自然・動物・環境"},
		{Key: 3, Name: "宇宙・科学・医学"},
		{Key: 4, Name: "カルチャー・伝統文化"},
		{Key: 5, Name: "文学・文芸"},
		{Key: 6, Name: "スポーツ"},
		{Key: 7, Name: "ドキュメンタリー全般"},
		{Key: 8, Name: "インタビュー・討論"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 9, Major: "劇場／公演", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "現代劇・新劇"},
		{Key: 1, Name: "ミュージカル"},
		{Key: 2, Name: "ダンス・バレエ"},
		{Key: 3, Name: "落語・演芸"},
		{Key: 4, Name: "歌舞伎・古典"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 10, Major: "趣味／教育", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "旅・釣り・アウトドア"},
		{Key: 1, Name: "園芸・ペット・手芸"},
		{Key: 2, Name: "音楽・美術・工芸"},
		{Key: 3, Name: "囲碁・将棋"},
		{Key: 4, Name: "麻雀・パチンコ"},
		{Key: 5, Name: "車・オートバイ"},
		{Key: 6, Name: "コンピュータ・ＴＶゲーム"},
		{Key: 7, Name: "会話・語学"},
		{Key: 8, Name: "幼児・小学生"},
		{Key: 9, Name: "中学生・高校生"},
		{Key: 10, Name: "大学生・受験"},
		{Key: 11, Name: "生涯教育・資格"},
		{Key: 12, Name: "教育問題"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 11, Major: "福祉", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "高齢者"},
		{Key: 1, Name: "障害者"},
		{Key: 2, Name: "社会福祉"},
		{Key: 3, Name: "ボランティア"},
		{Key: 4, Name: "手話"},
		{Key: 5, Name: "文字（字幕）"},
		{Key: 6, Name: "音声解説"},
		{Key: 15, Name: "その他"},
	}},
	{Key: 14, Major: "拡張", Middle: []aribGenreMiddleEntry{
		{Key: 0, Name: "BS/地上デジタル放送用番組付属情報"},
		{Key: 1, Name: "広帯域CSデジタル放送用拡張"},
		{Key: 2, Name: "衛星デジタル音声放送用拡張"},
		{Key: 3, Name: "サーバー型番組付属情報"},
		{Key: 4, Name: "IP放送用番組付属情報"},
	}},
	{Key: 15, Major: "その他", Middle: []aribGenreMiddleEntry{
		{Key: 15, Name: "その他"},
	}},
}

// aribUserTypeEntry は中分類ではなく user_nibble で表現されるジャンルの 1 エントリ。
type aribUserTypeEntry struct {
	Key  int
	Name string
}

// aribUserType は ariblib.constants.USER_TYPE 相当 (挿入順を保持) 。
var aribUserType = []aribUserTypeEntry{
	{Key: 0, Name: "中止の可能性あり"},
	{Key: 1, Name: "延長の可能性あり"},
	{Key: 2, Name: "中断の可能性あり"},
	{Key: 3, Name: "同一シリーズの別話数放送の可能性あり"},
	{Key: 4, Name: "編成未定枠"},
	{Key: 5, Name: "繰り上げの可能性あり"},
	{Key: 16, Name: "中断ニュースあり"},
	{Key: 17, Name: "当該イベントに関連する臨時サービスあり"},
	{Key: 32, Name: "当該イベント中に3D映像あり"},
}
