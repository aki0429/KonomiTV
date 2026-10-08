package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/routers/ReservationConditionsRouter.py の 5 つのエンドポイントを移植したもの。
//
// - GET    /api/recording/conditions
// - POST   /api/recording/conditions
// - GET    /api/recording/conditions/{reservation_condition_id}
// - PUT    /api/recording/conditions/{reservation_condition_id}
// - DELETE /api/recording/conditions/{reservation_condition_id}

// noteExcludeKeywordPattern は EDCB の not_key の先頭にあるメモ欄 (:note: から始まる除外キーワード) を取り除く。
// 移植元: ReservationConditionsRouter.DecodeEDCBSearchKeyInfo() の re.sub(r'^:note:[^ 　]*[ 　]?', ”, ...)
var noteExcludeKeywordPattern = regexp.MustCompile("^:note:[^ \u3000]*[ \u3000]?")

// noteExtractPattern は EDCB の not_key の先頭にあるメモ欄を取り出す。
// 移植元: ReservationConditionsRouter.DecodeEDCBSearchKeyInfo() の re.match(r"^:note:([^ 　]*)", ...)
var noteExtractPattern = regexp.MustCompile("^:note:([^ \u3000]*)")

// errChSet5Unavailable は EDCB から ChSet5.txt を取得できなかったことを表す。
// 移植元: GetChSet5Services() が送出する HTTPException (500 / 'Failed to get ChSet5.txt from EDCB')
var errChSet5Unavailable = errors.New("Failed to get ChSet5.txt from EDCB")

// registerReservationConditionRoutes はキーワード自動予約条件系のルートを mux に登録する。
// 移植元: server/app/routers/ReservationConditionsRouter.py
func (s *Server) registerReservationConditionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/recording/conditions", s.handleReservationConditions)
	mux.HandleFunc("POST /api/recording/conditions", s.handleReservationConditionAdd)
	mux.HandleFunc("GET /api/recording/conditions/{reservation_condition_id}", s.handleReservationCondition)
	mux.HandleFunc("PUT /api/recording/conditions/{reservation_condition_id}", s.handleReservationConditionUpdate)
	mux.HandleFunc("DELETE /api/recording/conditions/{reservation_condition_id}", s.handleReservationConditionDelete)
}

// writeEDCBError はキーワード自動予約条件系の内部エラーを FastAPI 互換のレスポンスとして書き出す。
func (s *Server) writeEDCBError(w http.ResponseWriter, err error) {
	if errors.Is(err, errChSet5Unavailable) {
		writeError(w, http.StatusInternalServerError, errChSet5Unavailable.Error())
		return
	}
	s.logger.Error("[ReservationConditionsRouter] Unexpected error.", "error", err)
	writeError(w, http.StatusInternalServerError, "Internal Server Error")
}

// decodeEDCBAutoAddData は EDCB の AutoAddData を schemas.ReservationCondition 互換のレスポンスに変換する。
//
// 移植元: ReservationConditionsRouter.DecodeEDCBAutoAddData()
func (s *Server) decodeEDCBAutoAddData(
	ctx context.Context,
	autoAddData reservations.AutoAddData,
	client edcbClient,
	chset5Services *[]reservations.ChSet5Item,
) (*reservationConditionResponse, error) {
	// 番組検索条件
	programSearchCondition, err := s.decodeEDCBSearchKeyInfo(ctx, autoAddData.SearchInfo, client, chset5Services)
	if err != nil {
		return nil, err
	}

	return &reservationConditionResponse{
		// キーワード自動予約条件 ID
		ID: autoAddData.DataID,
		// このキーワード自動予約条件で登録されている録画予約の数
		ReservationCount:       autoAddData.AddCount,
		ProgramSearchCondition: programSearchCondition,
		// 録画設定
		RecordSettings: DecodeEDCBRecSettingData(autoAddData.RecSetting),
	}, nil
}

// decodeEDCBSearchKeyInfo は EDCB の SearchKeyInfo を schemas.ProgramSearchCondition 互換の構造体に変換する。
//
// 移植元: ReservationConditionsRouter.DecodeEDCBSearchKeyInfo()
func (s *Server) decodeEDCBSearchKeyInfo(
	ctx context.Context,
	searchInfo reservations.SearchKeyInfo,
	client edcbClient,
	chset5Services *[]reservations.ChSet5Item,
) (programSearchCondition, error) {
	condition := programSearchCondition{}

	// 番組検索条件が有効かどうか
	condition.IsEnabled = !searchInfo.KeyDisabled

	// 検索キーワード
	condition.Keyword = searchInfo.AndKey

	// 除外キーワード (後述のメモ欄が :note: から始まる除外キーワードになっているので除去している)
	condition.ExcludeKeyword = noteExcludeKeywordPattern.ReplaceAllString(searchInfo.NotKey, "")

	// メモ欄 (EDCB の内部実装上は :note: から始まる除外キーワードになっているので抽出する)
	condition.Note = ""
	if match := noteExtractPattern.FindStringSubmatch(searchInfo.NotKey); match != nil {
		note := match[1]
		note = strings.ReplaceAll(note, `\s`, " ")
		note = strings.ReplaceAll(note, `\m`, "　")
		note = strings.ReplaceAll(note, `\\`, `\`)
		condition.Note = note
	}

	// 番組名のみを検索対象とするかどうか
	condition.IsTitleOnly = searchInfo.TitleOnlyFlag

	// 大文字小文字を区別するかどうか
	condition.IsCaseSensitive = searchInfo.CaseSensitive

	// あいまい検索を行うかどうか
	condition.IsFuzzySearchEnabled = searchInfo.AimaiFlag

	// 正規表現で検索するかどうか
	condition.IsRegexSearchEnabled = searchInfo.RegExpFlag

	// 検索対象を絞り込むチャンネル範囲のリスト
	// (service_list は (ONID << 32 | TSID << 16 | SID) のリストになっているので分解する)
	serviceRanges := []programSearchConditionServiceSchema{}
	for _, service := range searchInfo.ServiceList {
		serviceRanges = append(serviceRanges, programSearchConditionServiceSchema{
			NetworkID:         int(service >> 32),
			TransportStreamID: int((service >> 16) & 0xffff),
			ServiceID:         int(service & 0xffff),
		})
	}

	// この時点で service_ranges の内容がデフォルトのチャンネル範囲 (全チャンネルが検索対象) と一致する場合、
	// 全チャンネルを検索対象にしているのと同義なので None に変換する
	defaultServiceRanges, err := s.getDefaultServiceRanges(ctx, client, chset5Services)
	if err != nil {
		return condition, err
	}
	if sameServiceRanges(serviceRanges, defaultServiceRanges) {
		condition.ServiceRanges = nil
	} else {
		condition.ServiceRanges = serviceRanges
	}

	// 検索対象を絞り込むジャンル範囲のリスト (None を指定すると全てのジャンルが検索対象になる)
	var genreRanges []genreSchema
	for _, content := range searchInfo.ContentList {
		major, found := reservations.FindGenreMajor(content.ContentNibble >> 8)
		if !found {
			continue
		}
		// major … 大分類 / middle … 中分類
		genre := genreSchema{
			Major:  strings.ReplaceAll(major.Major, "／", "・"),
			Middle: "未定義",
		}
		for _, middle := range major.Middle {
			if middle.Key == content.ContentNibble&0xf {
				genre.Middle = strings.ReplaceAll(middle.Name, "／", "・")
				break
			}
		}
		// もし content_nibble & 0xff が 0xff なら、その大分類ジャンルの配下のすべての中分類ジャンルが検索対象になる
		if content.ContentNibble&0xff == 0xff {
			genre.Middle = "すべて"
		}
		// BS/地上デジタル放送用番組付属情報がジャンルに含まれている場合、user_nibble から値を取得して書き換える
		// (たとえば「中止の可能性あり」や「延長の可能性あり」といった情報が取れる)
		if genre.Major == "拡張" {
			if genre.Middle == "BS/地上デジタル放送用番組付属情報" {
				userNibble := (content.UserNibble >> 8 << 4) | (content.UserNibble & 0xf)
				genre.Middle = "未定義"
				for _, userType := range reservations.UserTypes() {
					if userType.Key == userNibble {
						genre.Middle = userType.Name
						break
					}
				}
			} else {
				// 「拡張」はあるがBS/地上デジタル放送用番組付属情報でない場合はなんの値なのかわからないのでパス
				continue
			}
		}
		// ジャンルを追加
		if genreRanges == nil {
			genreRanges = []genreSchema{}
		}
		genreRanges = append(genreRanges, genre)
	}
	condition.GenreRanges = genreRanges

	// genre_ranges で指定したジャンルを逆に検索対象から除外するかどうか
	condition.IsExcludeGenreRanges = searchInfo.NotContetFlag

	// 検索対象を絞り込む放送日時範囲のリスト (None を指定すると全ての放送日時が検索対象になる)
	var dateRanges []programSearchConditionDateSchema
	for _, date := range searchInfo.DateList {
		if dateRanges == nil {
			dateRanges = []programSearchConditionDateSchema{}
		}
		dateRanges = append(dateRanges, programSearchConditionDateSchema{
			StartDayOfWeek: date.StartDayOfWeek,
			StartHour:      date.StartHour,
			StartMinute:    date.StartMin,
			EndDayOfWeek:   date.EndDayOfWeek,
			EndHour:        date.EndHour,
			EndMinute:      date.EndMin,
		})
	}
	condition.DateRanges = dateRanges

	// date_ranges で指定した放送日時を逆に検索対象から除外するかどうか
	condition.IsExcludeDateRanges = searchInfo.NotDateFlag

	// 番組長で絞り込む最小範囲・最大範囲 (秒) / 指定しない場合は None になる
	condition.DurationRangeMin = nil
	if searchInfo.ChkDurationMin > 0 {
		value := searchInfo.ChkDurationMin
		condition.DurationRangeMin = &value
	}
	condition.DurationRangeMax = nil
	if searchInfo.ChkDurationMax > 0 {
		value := searchInfo.ChkDurationMax
		condition.DurationRangeMax = &value
	}

	// 番組の放送種別で絞り込む: すべて / 無料のみ / 有料のみ
	condition.BroadcastType = "All"
	if searchInfo.FreeCaFlag == 1 {
		condition.BroadcastType = "FreeOnly"
	} else if searchInfo.FreeCaFlag == 2 {
		condition.BroadcastType = "PaidOnly"
	}

	// 同じ番組名の既存録画との重複チェック: 何もしない / 同じチャンネルのみ対象にする / 全てのチャンネルを対象にする
	condition.DuplicateTitleCheckScope = "None"
	if searchInfo.ChkRecEnd {
		if searchInfo.ChkRecNoService {
			condition.DuplicateTitleCheckScope = "AllChannels"
		} else {
			condition.DuplicateTitleCheckScope = "SameChannelOnly"
		}
	}

	// 同じ番組名の既存録画との重複チェックの対象期間 (日単位)
	condition.DuplicateTitleCheckPeriodDays = searchInfo.ChkRecDay

	return condition, nil
}

// encodeEDCBSearchKeyInfo は schemas.ProgramSearchCondition 互換の構造体を EDCB の SearchKeyInfo に変換する。
//
// 移植元: ReservationConditionsRouter.EncodeEDCBSearchKeyInfo()
func (s *Server) encodeEDCBSearchKeyInfo(
	ctx context.Context,
	condition programSearchCondition,
	client edcbClient,
	chset5Services *[]reservations.ChSet5Item,
) (reservations.SearchKeyInfo, error) {
	// メモ欄は EDCB の内部実装上は :note: から始まる除外キーワードになっているので再構築する
	notKey := condition.ExcludeKeyword
	if condition.Note != "" {
		note := strings.ReplaceAll(condition.Note, `\`, `\\`)
		note = strings.ReplaceAll(note, " ", `\s`)
		note = strings.ReplaceAll(note, "　", `\m`)
		notKey = ":note:" + note
		if condition.ExcludeKeyword != "" {
			// 半角スペースを挟んでから元の除外キーワードを追加
			notKey += " " + condition.ExcludeKeyword
		}
	}

	// 番組の放送種別で絞り込む: すべて / 無料のみ / 有料のみ
	freeCaFlag := 0
	if condition.BroadcastType == "FreeOnly" {
		freeCaFlag = 1
	} else if condition.BroadcastType == "PaidOnly" {
		freeCaFlag = 2
	}

	// 検索対象を絞り込むチャンネル範囲のリスト
	// (service_ranges が None の場合だけ、KonomiTV の視聴可能チャンネル全体を検索対象にする)
	serviceRanges := condition.ServiceRanges
	if serviceRanges == nil {
		defaultServiceRanges, err := s.getDefaultServiceRanges(ctx, client, chset5Services)
		if err != nil {
			return reservations.SearchKeyInfo{}, err
		}
		serviceRanges = defaultServiceRanges
	}
	serviceList := make([]int64, 0, len(serviceRanges))
	for _, channel := range serviceRanges {
		service, err := channel.edcbServiceID()
		if err != nil {
			return reservations.SearchKeyInfo{}, err
		}
		serviceList = append(serviceList, service)
	}

	// 検索対象を絞り込むジャンル範囲のリスト (空リストを指定すると全てのジャンルが検索対象になる)
	contentList := []reservations.ContentData{}
	if condition.GenreRanges != nil {
		for _, genre := range condition.GenreRanges {
			// KonomiTV では見栄えのために ／ を ・ に置換しているので、ここで元に戻す
			major := strings.ReplaceAll(genre.Major, "・", "／")
			middle := strings.ReplaceAll(genre.Middle, "・", "／")
			// 万が一見つからなかった場合のデフォルト値
			contentNibbleLevel1 := 0xF // "その他"
			contentNibbleLevel2 := 0xF // "その他"
			userNibble := 0x0          // user_nibble はユーザージャンルがある場合のみ値が入る
			for _, entry := range reservations.GenreMajors() {
				if entry.Major != major {
					continue
				}
				// content_nibble_level1 には大分類の値を入れる
				contentNibbleLevel1 = entry.Key
				if contentNibbleLevel1 == 0xE {
					// 大分類が "拡張" の時のみ、中分類の文字列に当てはまるBS/地上デジタル放送用番組付属情報を探す
					for _, userType := range reservations.UserTypes() {
						if userType.Name == middle {
							// content_nibble_level2 にはBS/地上デジタル放送用番組付属情報を示す値を入れる
							contentNibbleLevel2 = 0x0
							// user_nibble には中分類の値を入れる
							userNibble = userType.Key
							break
						}
					}
				} else if middle == "すべて" {
					// 0xFF は全ての中分類を示す (おそらく EDCB 独自仕様？)
					contentNibbleLevel2 = 0xFF
					break
				} else {
					// 中分類の値を探す
					for _, middleEntry := range entry.Middle {
						if middleEntry.Name == middle {
							contentNibbleLevel2 = middleEntry.Key
							break
						}
					}
				}
			}
			// EDCB の ContentData の content_nibble は content_nibble_level1 * 256 + content_nibble_level2
			contentList = append(contentList, reservations.ContentData{
				ContentNibble: contentNibbleLevel1*256 + contentNibbleLevel2,
				UserNibble:    userNibble,
			})
		}
	}

	// 検索対象を絞り込む放送日時範囲のリスト (空リストを指定すると全ての放送日時が検索対象になる)
	dateList := []reservations.SearchDateInfo{}
	if condition.DateRanges != nil {
		for _, date := range condition.DateRanges {
			dateList = append(dateList, reservations.SearchDateInfo{
				StartDayOfWeek: date.StartDayOfWeek,
				StartHour:      date.StartHour,
				StartMin:       date.StartMinute,
				EndDayOfWeek:   date.EndDayOfWeek,
				EndHour:        date.EndHour,
				EndMin:         date.EndMinute,
			})
		}
	}

	searchInfo := reservations.SearchKeyInfo{
		AndKey:        condition.Keyword,
		NotKey:        notKey,
		KeyDisabled:   !condition.IsEnabled,
		CaseSensitive: condition.IsCaseSensitive,
		RegExpFlag:    condition.IsRegexSearchEnabled,
		TitleOnlyFlag: condition.IsTitleOnly,
		ContentList:   contentList,
		DateList:      dateList,
		ServiceList:   serviceList,
		// KonomiTV の番組検索 UI では映像 / 音声コンポーネント条件を提供していないため、常に空配列で送信する
		VideoList: []int{},
		AudioList: []int{},
		AimaiFlag: condition.IsFuzzySearchEnabled,
		// not_contet_flag は EDCB 側のスペルミスをそのまま踏襲している
		NotContetFlag:   condition.IsExcludeGenreRanges,
		NotDateFlag:     condition.IsExcludeDateRanges,
		FreeCaFlag:      freeCaFlag,
		ChkRecEnd:       condition.DuplicateTitleCheckScope != "None",
		ChkRecDay:       condition.DuplicateTitleCheckPeriodDays,
		ChkRecNoService: condition.DuplicateTitleCheckScope == "AllChannels",
		ChkDurationMin:  durationRangeValue(condition.DurationRangeMin),
		ChkDurationMax:  durationRangeValue(condition.DurationRangeMax),
	}
	// 任意精度の期間値は wire の modulo に必要な下位桁だけへ縮約する。
	if text, exists := condition.largeIntegers["duration_range_min"]; exists {
		searchInfo.ChkDurationMin = searchIntegerModulo(text, 10000)
	}
	if text, exists := condition.largeIntegers["duration_range_max"]; exists {
		searchInfo.ChkDurationMax = searchIntegerModulo(text, 100000000)
	}
	if text, exists := condition.largeIntegers["duplicate_title_check_period_days"]; exists {
		if searchInfo.ChkRecNoService {
			searchInfo.ChkRecDay = searchIntegerModulo(text, 10000)
		} else if searchInfo.ChkRecEnd {
			return reservations.SearchKeyInfo{}, fmt.Errorf("edcb: value is out of range for the field")
		}
	}
	return searchInfo, nil
}

// durationRangeValue は番組長の範囲指定を EDCB に送る整数値に変換する (未指定は 0) 。
func durationRangeValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// sameServiceRanges はチャンネル範囲のリストが等しいかどうかを判定する。
//
// Python 版と同じく (network_id, transport_stream_id, service_id) でソートしてから比較する。
func sameServiceRanges(left []programSearchConditionServiceSchema, right []programSearchConditionServiceSchema) bool {
	if len(left) != len(right) {
		return false
	}
	leftSorted := make([]programSearchConditionServiceSchema, len(left))
	copy(leftSorted, left)
	rightSorted := make([]programSearchConditionServiceSchema, len(right))
	copy(rightSorted, right)
	sortServiceRanges(leftSorted)
	sortServiceRanges(rightSorted)
	for index := range leftSorted {
		if leftSorted[index] != rightSorted[index] {
			return false
		}
	}
	return true
}

// sortServiceRanges はチャンネル範囲のリストを (network_id, transport_stream_id, service_id) 順にソートする。
func sortServiceRanges(ranges []programSearchConditionServiceSchema) {
	sort.SliceStable(ranges, func(i int, j int) bool {
		if ranges[i].NetworkID != ranges[j].NetworkID {
			return ranges[i].NetworkID < ranges[j].NetworkID
		}
		if ranges[i].TransportStreamID != ranges[j].TransportStreamID {
			return ranges[i].TransportStreamID < ranges[j].TransportStreamID
		}
		return ranges[i].ServiceID < ranges[j].ServiceID
	})
}

// getChSet5Services は ChSet5.txt を解析したサービス一覧を取得する。
//
// 移植元: ReservationConditionsRouter.GetChSet5Services()
func (s *Server) getChSet5Services(client edcbClient, chset5Services *[]reservations.ChSet5Item) ([]reservations.ChSet5Item, error) {
	// 呼び出し元で既に取得済みの場合は再取得しない
	if chset5Services != nil {
		return *chset5Services, nil
	}

	chset5Txt, ok := client.FileCopy("ChSet5.txt")
	if !ok {
		s.logger.Error("[ReservationConditionsRouter][GetChSet5Services] Failed to get ChSet5.txt from EDCB.")
		return nil, errChSet5Unavailable
	}
	return reservations.ParseChSet5(reservations.ConvertEDCBBytesToString(chset5Txt)), nil
}

// getDefaultServiceRanges はデフォルトの番組検索条件のチャンネル範囲のリスト (全チャンネルが検索対象) を取得する。
//
// 移植元: ReservationConditionsRouter.GetDefaultServiceRanges()
func (s *Server) getDefaultServiceRanges(ctx context.Context, client edcbClient, chset5Services *[]reservations.ChSet5Item) ([]programSearchConditionServiceSchema, error) {
	services, err := s.getChSet5Services(client, chset5Services)
	if err != nil {
		return nil, err
	}

	// EpgTimer の ChSet5 は同一サービスが重複する可能性があるため、ONID / TSID / SID の組で先勝ちにして重複を除去する
	uniqueServiceKeys := map[[3]int]bool{}
	defaultServiceRanges := []programSearchConditionServiceSchema{}
	for _, service := range services {
		serviceKey := [3]int{service.Onid, service.Tsid, service.Sid}
		if uniqueServiceKeys[serviceKey] {
			continue
		}
		uniqueServiceKeys[serviceKey] = true
		defaultServiceRanges = append(defaultServiceRanges, programSearchConditionServiceSchema{
			NetworkID:         service.Onid,
			TransportStreamID: service.Tsid,
			ServiceID:         service.Sid,
		})
	}
	return defaultServiceRanges, nil
}

// getAutoAddDataList はすべてのキーワード自動予約条件の情報を取得する。
//
// 移植元: ReservationConditionsRouter.GetAutoAddDataList()
func (s *Server) getAutoAddDataList(w http.ResponseWriter, client edcbClient) ([]reservations.AutoAddData, bool) {
	autoAddDataList, ok := client.EnumAutoAdd()
	if !ok {
		s.logger.Error("[ReservationConditionsRouter][GetAutoAddDataList] Failed to get the list of reserve conditions.")
		writeError(w, http.StatusInternalServerError, "Failed to get the list of reserve conditions")
		return nil, false
	}
	return autoAddDataList, true
}

// getAutoAddData は指定されたキーワード自動予約条件の情報を取得する。
//
// 移植元: ReservationConditionsRouter.GetAutoAddData()
func (s *Server) getAutoAddData(w http.ResponseWriter, client edcbClient, reservationConditionID int) (reservations.AutoAddData, bool) {
	autoAddDataList, ok := s.getAutoAddDataList(w, client)
	if !ok {
		return reservations.AutoAddData{}, false
	}
	for _, autoAddData := range autoAddDataList {
		if autoAddData.DataID == reservationConditionID {
			return autoAddData, true
		}
	}

	// 指定されたキーワード自動予約条件が見つからなかった場合はエラーを返す
	s.logger.Error(
		"[ReservationConditionsRouter][GetAutoAddData] Specified reservation_condition_id was not found.",
		"reservation_condition_id", reservationConditionID,
	)
	writeError(w, http.StatusUnprocessableEntity, "Specified reservation_condition_id was not found")
	return reservations.AutoAddData{}, false
}

// handleReservationConditions はキーワード自動予約条件一覧 API を処理する。
// 移植元: ReservationConditionsRouter.ReservationConditionsAPI()
func (s *Server) handleReservationConditions(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}

	// EDCB から現在のすべてのキーワード自動予約条件の情報を取得
	autoAddDataList, ok := client.EnumAutoAdd()
	if !ok {
		// 取得できなかった場合は空のリストを返す
		writeJSON(w, http.StatusOK, reservationConditionsResponse{Total: 0, ReservationConditions: []reservationConditionResponse{}})
		return
	}

	// 同一リクエスト内での ChSet5.txt 再取得を避けるため、先に 1 回だけ取得して全変換処理で使い回す
	chset5Services, err := s.getChSet5Services(client, nil)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}

	reserveConditions := make([]reservationConditionResponse, 0, len(autoAddDataList))
	for _, autoAddData := range autoAddDataList {
		condition, err := s.decodeEDCBAutoAddData(r.Context(), autoAddData, client, &chset5Services)
		if err != nil {
			s.writeEDCBError(w, err)
			return
		}
		reserveConditions = append(reserveConditions, *condition)
	}

	writeJSON(w, http.StatusOK, reservationConditionsResponse{
		Total:                 len(reserveConditions),
		ReservationConditions: reserveConditions,
	})
}

// handleReservationConditionAdd はキーワード自動予約条件登録 API を処理する。
// 移植元: ReservationConditionsRouter.RegisterReservationConditionAPI()
func (s *Server) handleReservationConditionAdd(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}

	var request reservationConditionAddRequest
	if !decodeJSONBody(r, &request) || request.ProgramSearchCondition == nil || request.RecordSettings == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	condition, err := request.ProgramSearchCondition.toProgramSearchCondition()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	recordSettings, err := request.RecordSettings.toRecordSettings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// EDCB の AutoAddData を組み立てる (data_id は EDCB 側で自動で割り振られるため省略している)
	chset5Services, err := s.getChSet5Services(client, nil)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	searchInfo, err := s.encodeEDCBSearchKeyInfo(r.Context(), condition, client, &chset5Services)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	autoAddData := reservations.AutoAddData{
		SearchInfo: searchInfo,
		RecSetting: EncodeEDCBRecSettingData(recordSettings),
	}

	// EDCB にキーワード自動予約条件を登録するように指示
	if !client.AddAutoAdd([]reservations.AutoAddData{autoAddData}) {
		s.logger.Error("[ReservationConditionsRouter][RegisterReservationConditionAPI] Failed to register the reserve condition.")
		writeError(w, http.StatusInternalServerError, "Failed to register the reserve condition")
		return
	}
	// どのキーワード自動予約条件 ID で追加されたかは sendAddAutoAdd() のレスポンスからは取れないので、201 Created を返す
	writeJSON(w, http.StatusCreated, nil)
}

// handleReservationCondition はキーワード自動予約条件取得 API を処理する。
// 移植元: ReservationConditionsRouter.ReservationConditionAPI()
func (s *Server) handleReservationCondition(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	conditionID, ok := parsePathInt(w, r, "reservation_condition_id")
	if !ok {
		return
	}
	autoAddData, ok := s.getAutoAddData(w, client, int(conditionID))
	if !ok {
		return
	}

	// EDCB の AutoAddData を schemas.ReservationCondition 互換のレスポンスに変換して返す
	chset5Services, err := s.getChSet5Services(client, nil)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	response, err := s.decodeEDCBAutoAddData(r.Context(), autoAddData, client, &chset5Services)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleReservationConditionUpdate はキーワード自動予約条件更新 API を処理する。
// 移植元: ReservationConditionsRouter.UpdateReservationConditionAPI()
func (s *Server) handleReservationConditionUpdate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	conditionID, ok := parsePathInt(w, r, "reservation_condition_id")
	if !ok {
		return
	}

	var request reservationConditionAddRequest
	if !decodeJSONBody(r, &request) || request.ProgramSearchCondition == nil || request.RecordSettings == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid request body")
		return
	}
	condition, err := request.ProgramSearchCondition.toProgramSearchCondition()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	recordSettings, err := request.RecordSettings.toRecordSettings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	autoAddData, ok := s.getAutoAddData(w, client, int(conditionID))
	if !ok {
		return
	}

	// 現在のキーワード自動予約条件の AutoAddData に新しい検索条件・録画設定を上書きマージする形で EDCB に送信する
	chset5Services, err := s.getChSet5Services(client, nil)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	searchInfo, err := s.encodeEDCBSearchKeyInfo(r.Context(), condition, client, &chset5Services)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	autoAddData.SearchInfo = searchInfo
	autoAddData.RecSetting = EncodeEDCBRecSettingData(recordSettings)

	// EDCB に指定されたキーワード自動予約条件を更新するように指示
	if !client.ChgAutoAdd([]reservations.AutoAddData{autoAddData}) {
		s.logger.Error(
			"[ReservationConditionsRouter][UpdateReservationConditionAPI] Failed to update the specified reserve condition.",
			"reservation_condition_id", autoAddData.DataID,
		)
		writeError(w, http.StatusInternalServerError, "Failed to update the specified reserve condition")
		return
	}

	// 更新されたキーワード自動予約条件の情報を schemas.ReservationCondition 互換のレスポンスに変換して返す
	updatedAutoAddData, ok := s.getAutoAddData(w, client, autoAddData.DataID)
	if !ok {
		return
	}
	response, err := s.decodeEDCBAutoAddData(r.Context(), updatedAutoAddData, client, &chset5Services)
	if err != nil {
		s.writeEDCBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleReservationConditionDelete はキーワード自動予約条件削除 API を処理する。
// 移植元: ReservationConditionsRouter.DeleteReservationConditionAPI()
func (s *Server) handleReservationConditionDelete(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}
	conditionID, ok := parsePathInt(w, r, "reservation_condition_id")
	if !ok {
		return
	}
	autoAddData, ok := s.getAutoAddData(w, client, int(conditionID))
	if !ok {
		return
	}

	// EDCB に指定されたキーワード自動予約条件を削除するように指示
	if !client.DelAutoAdd([]int{autoAddData.DataID}) {
		s.logger.Error(
			"[ReservationConditionsRouter][DeleteReservationConditionAPI] Failed to delete the specified reserve condition.",
			"reservation_condition_id", autoAddData.DataID,
		)
		writeError(w, http.StatusInternalServerError, "Failed to delete the specified reserve condition")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
