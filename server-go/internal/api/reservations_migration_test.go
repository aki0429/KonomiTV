package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは、予約系 3 ルーター (ReservationsRouter / ReservationConditionsRouter /
// RecordingPresetsRouter) の Go 移植が Python 実装と JSON 同値であることを検証する。
//
// 期待値は reservations_python_fixture_test.go に埋め込んだ Python 実装の実出力
// (probe_compare.py で app.config をスタブ化して生成) 。
// 実機の EDCB には接続しない (ネットワーク層は fakeEDCBClient で差し替える) 。

// fixtureJSON は Python の json.dumps(..., ensure_ascii=False, separators=(',', ':')) と
// 同じ形式の JSON 文字列にする (FastAPI の JSONResponse と同じ出力形式) 。
func fixtureJSON(t *testing.T, value any) string {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatalf("JSON エンコードに失敗した: %v", err)
	}
	return strings.TrimRight(buffer.String(), "\n")
}

// usedPythonFixtures は実際に突き合わせたフィクスチャの名前。
//
// TestZZAllPythonFixturesConsumed で「使われていない期待値」を検出するために記録する。
var usedPythonFixtures = map[string]bool{}

// assertPythonFixture は Go の変換結果が Python 実装の出力と完全に一致することを検証する。
func assertPythonFixture(t *testing.T, name string, value any) {
	t.Helper()
	expected, ok := pythonReservationFixtures[name]
	if !ok {
		t.Fatalf("フィクスチャ %s が見つからない", name)
	}
	usedPythonFixtures[name] = true
	actual := fixtureJSON(t, value)
	if actual != expected {
		t.Errorf("%s が Python 実装と一致しない\n期待: %s\n実際: %s", name, expected, actual)
	}
}

// assertPythonFixtureSemantic は JSON の意味が等しいことだけを検証する (Go の map は
// キーがソートされて出力されるため、キー順が意味を持たないケースで使う) 。
func assertPythonFixtureSemantic(t *testing.T, name string, value any) {
	t.Helper()
	expected, ok := pythonReservationFixtures[name]
	if !ok {
		t.Fatalf("フィクスチャ %s が見つからない", name)
	}
	usedPythonFixtures[name] = true
	actual := fixtureJSON(t, value)
	var expectedValue any
	if err := json.Unmarshal([]byte(expected), &expectedValue); err != nil {
		t.Fatalf("フィクスチャ %s の JSON を読めない: %v", name, err)
	}
	var actualValue any
	if err := json.Unmarshal([]byte(actual), &actualValue); err != nil {
		t.Fatalf("Go の出力 %s の JSON を読めない: %v", name, err)
	}
	if !reflect.DeepEqual(expectedValue, actualValue) {
		t.Errorf("%s が Python 実装と意味的に一致しない\n期待: %s\n実際: %s", name, expected, actual)
	}
}

func intPointer(value int) *int       { return &value }
func strPointer(value string) *string { return &value }
func boolPointer(value bool) *bool    { return &value }

// fakeEDCBClient は実機の EDCB に接続しないテスト用クライアント。
//
// 呼び出されたメソッドを記録し、想定外の呼び出しを検出できるようにする。
// 戻り値はフィールドで差し替えられる (既定値は「取得できなかった」)。
type fakeEDCBClient struct {
	calls []string

	enumReserveData []reservations.ReserveData
	enumReserveOK   bool

	enumAutoAddData []reservations.AutoAddData
	enumAutoAddOK   bool

	fileCopy2Files map[string][]byte
	fileCopy2OK    bool
}

func (f *fakeEDCBClient) record(name string) { f.calls = append(f.calls, name) }

func (f *fakeEDCBClient) EnumReserve() ([]reservations.ReserveData, bool) {
	f.record("EnumReserve")
	return f.enumReserveData, f.enumReserveOK
}

func (f *fakeEDCBClient) AddReserve(reserveList []reservations.ReserveData) bool {
	f.record("AddReserve")
	return false
}

func (f *fakeEDCBClient) ChgReserve(reserveList []reservations.ReserveData) bool {
	f.record("ChgReserve")
	return false
}

func (f *fakeEDCBClient) DelReserve(reserveIDList []int) bool {
	f.record("DelReserve")
	return false
}

func (f *fakeEDCBClient) EnumPgInfoEx(serviceTimeList []int64) ([]reservations.ServiceEventInfo, bool) {
	f.record("EnumPgInfoEx")
	return nil, false
}

func (f *fakeEDCBClient) FileCopy(name string) ([]byte, bool) {
	f.record("FileCopy:" + name)
	return nil, false
}

func (f *fakeEDCBClient) FileCopy2(nameList []string) ([]reservations.FileData, bool) {
	f.record("FileCopy2:" + strings.Join(nameList, ","))
	if !f.fileCopy2OK {
		return nil, false
	}
	files := make([]reservations.FileData, 0, len(nameList))
	for _, name := range nameList {
		data, ok := f.fileCopy2Files[name]
		if !ok {
			return nil, false
		}
		files = append(files, reservations.FileData{Name: name, Data: data})
	}
	return files, true
}

func (f *fakeEDCBClient) GetRecFilePath(reserveID int) *string {
	f.record("GetRecFilePath")
	return nil
}

func (f *fakeEDCBClient) EnumAutoAdd() ([]reservations.AutoAddData, bool) {
	f.record("EnumAutoAdd")
	return f.enumAutoAddData, f.enumAutoAddOK
}

func (f *fakeEDCBClient) AddAutoAdd(dataList []reservations.AutoAddData) bool {
	f.record("AddAutoAdd")
	return false
}

func (f *fakeEDCBClient) ChgAutoAdd(dataList []reservations.AutoAddData) bool {
	f.record("ChgAutoAdd")
	return false
}

func (f *fakeEDCBClient) DelAutoAdd(idList []int) bool {
	f.record("DelAutoAdd")
	return false
}

var _ edcbClient = (*fakeEDCBClient)(nil)

// ---------------------------------------------------------------------------
// A. DecodeEDCBRecSettingData
// ---------------------------------------------------------------------------

// recSettingFixtureA は Python 側 probe の rec_setting_a と同じ入力。
func recSettingFixtureA() reservations.RecSettingData {
	return reservations.RecSettingData{
		RecMode:     0,
		Priority:    2,
		TuijyuuFlag: true,
		ServiceMode: 0x00000011 | 0x20,
		PittariFlag: true,
		BatFilePath: `C:\test\a.bat`,
		RecFolderList: []reservations.RecFileSetInfo{
			{RecFolder: `C:\rec`, WritePlugIn: "Write_Default.dll", RecNamePlugIn: "RecName_Macro.dll?$title$.ts"},
			{RecFolder: `C:\rec2`, WritePlugIn: "Write_Default.dll", RecNamePlugIn: "RecName_Macro.dll"},
		},
		PartialRecFolder: []reservations.RecFileSetInfo{
			{RecFolder: `C:\rec1seg`, WritePlugIn: "Write_Default.dll", RecNamePlugIn: "RecName_Macro.dll?$title$"},
		},
		SuspendMode:    2,
		RebootFlag:     true,
		StartMargin:    intPointer(10),
		EndMargin:      intPointer(5),
		ContinueRec:    true,
		PartialRecFlag: 1,
		TunerID:        3,
	}
}

// recSettingFixtureB は Python 側 probe の rec_setting_b (rec_mode を差し替えて使う) と同じ入力。
func recSettingFixtureB(recMode int) reservations.RecSettingData {
	return reservations.RecSettingData{
		RecMode:          recMode,
		Priority:         5,
		TuijyuuFlag:      false,
		ServiceMode:      0,
		PittariFlag:      false,
		BatFilePath:      "",
		RecFolderList:    []reservations.RecFileSetInfo{},
		SuspendMode:      4,
		RebootFlag:       false,
		ContinueRec:      false,
		PartialRecFlag:   2,
		TunerID:          0,
		PartialRecFolder: []reservations.RecFileSetInfo{},
	}
}

func TestPythonParityDecodeEDCBRecSettingData(t *testing.T) {
	assertPythonFixture(t, "decode_rec_setting_a", DecodeEDCBRecSettingData(recSettingFixtureA()))
	assertPythonFixture(t, "decode_rec_setting_b", DecodeEDCBRecSettingData(recSettingFixtureB(5)))

	// rec_mode (0 ~ 9) ごとの録画モード・有効無効の変換
	for recMode := 0; recMode <= 9; recMode++ {
		name := fmt.Sprintf("decode_rec_mode_%d", recMode)
		assertPythonFixture(t, name, DecodeEDCBRecSettingData(recSettingFixtureB(recMode)))
	}

	// suspend_mode / reboot_flag ごとの録画後動作モードの変換
	for suspendMode := 0; suspendMode <= 4; suspendMode++ {
		for _, rebootFlag := range []bool{false, true} {
			data := recSettingFixtureB(5)
			data.SuspendMode = suspendMode
			data.RebootFlag = rebootFlag
			name := fmt.Sprintf("decode_suspend_%d_%d", suspendMode, boolToInt(rebootFlag))
			assertPythonFixture(t, name, DecodeEDCBRecSettingData(data))
		}
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// B. EncodeEDCBRecSettingData
// ---------------------------------------------------------------------------

// pythonRecFileSetInfo / pythonRecSettingData は EDCB へ送る RecSettingData を Python 実装と
// 同じ JSON (snake_case) で比較するための写像。
//
// Python 版は start_margin / end_margin を None のときだけキー自体を作らないため、
// ここでも omitempty で同じ挙動にしている (キーの並び順も Python 版に合わせている) 。
type pythonRecFileSetInfo struct {
	RecFolder     string `json:"rec_folder"`
	WritePlugIn   string `json:"write_plug_in"`
	RecNamePlugIn string `json:"rec_name_plug_in"`
}

type pythonRecSettingData struct {
	RecMode          int                    `json:"rec_mode"`
	Priority         int                    `json:"priority"`
	TuijyuuFlag      bool                   `json:"tuijyuu_flag"`
	ServiceMode      int                    `json:"service_mode"`
	PittariFlag      bool                   `json:"pittari_flag"`
	BatFilePath      string                 `json:"bat_file_path"`
	RecFolderList    []pythonRecFileSetInfo `json:"rec_folder_list"`
	SuspendMode      int                    `json:"suspend_mode"`
	RebootFlag       bool                   `json:"reboot_flag"`
	ContinueRec      bool                   `json:"continue_rec_flag"`
	PartialRecFlag   int                    `json:"partial_rec_flag"`
	TunerID          int                    `json:"tuner_id"`
	PartialRecFolder []pythonRecFileSetInfo `json:"partial_rec_folder"`
	StartMargin      *int                   `json:"start_margin,omitempty"`
	EndMargin        *int                   `json:"end_margin,omitempty"`
}

func toPythonRecSettingData(data reservations.RecSettingData) pythonRecSettingData {
	folderList := make([]pythonRecFileSetInfo, 0, len(data.RecFolderList))
	for _, folder := range data.RecFolderList {
		folderList = append(folderList, pythonRecFileSetInfo{
			RecFolder:     folder.RecFolder,
			WritePlugIn:   folder.WritePlugIn,
			RecNamePlugIn: folder.RecNamePlugIn,
		})
	}
	partialRecFolder := make([]pythonRecFileSetInfo, 0, len(data.PartialRecFolder))
	for _, folder := range data.PartialRecFolder {
		partialRecFolder = append(partialRecFolder, pythonRecFileSetInfo{
			RecFolder:     folder.RecFolder,
			WritePlugIn:   folder.WritePlugIn,
			RecNamePlugIn: folder.RecNamePlugIn,
		})
	}
	return pythonRecSettingData{
		RecMode:          data.RecMode,
		Priority:         data.Priority,
		TuijyuuFlag:      data.TuijyuuFlag,
		ServiceMode:      data.ServiceMode,
		PittariFlag:      data.PittariFlag,
		BatFilePath:      data.BatFilePath,
		RecFolderList:    folderList,
		SuspendMode:      data.SuspendMode,
		RebootFlag:       data.RebootFlag,
		ContinueRec:      data.ContinueRec,
		PartialRecFlag:   data.PartialRecFlag,
		TunerID:          data.TunerID,
		PartialRecFolder: partialRecFolder,
		StartMargin:      data.StartMargin,
		EndMargin:        data.EndMargin,
	}
}

func TestPythonParityEncodeEDCBRecSettingData(t *testing.T) {
	// 録画設定を指定したケース
	request := recordSettingsRequest{
		IsEnabled: boolPointer(true),
		Priority:  intPointer(4),
		RecordingFolders: &[]recordingFolderSchema{
			{
				RecordingFolderPath:             `D:\record`,
				RecordingFileNameTemplate:       strPointer("$title$"),
				IsOnesegSeparateRecordingFolder: false,
			},
			{
				RecordingFolderPath:             `D:\record1seg`,
				RecordingFileNameTemplate:       nil,
				IsOnesegSeparateRecordingFolder: true,
			},
		},
		RecordingStartMargin:                     intPointer(15),
		RecordingMode:                            strPointer("AllServicesWithoutDecoding"),
		CaptionRecordingMode:                     strPointer("Enable"),
		DataBroadcastingRecordingMode:            strPointer("Disable"),
		PostRecordingMode:                        strPointer("SuspendAndReboot"),
		PostRecordingBatFilePath:                 strPointer(`C:\post.bat`),
		IsEventRelayFollowEnabled:                boolPointer(false),
		IsExactRecordingEnabled:                  boolPointer(true),
		IsOnesegSeparateOutputEnabled:            boolPointer(true),
		IsSequentialRecordingInSingleFileEnabled: boolPointer(true),
		ForcedTunerID:                            intPointer(0),
	}
	settings, err := request.toRecordSettings()
	if err != nil {
		t.Fatalf("録画設定の検証に失敗した: %v", err)
	}
	assertPythonFixture(t, "encode_rec_setting_a", toPythonRecSettingData(EncodeEDCBRecSettingData(settings)))

	// すべて既定値のケース (Pydantic の既定値適用と同じ結果になること)
	defaultSettings, err := (recordSettingsRequest{}).toRecordSettings()
	if err != nil {
		t.Fatalf("既定値の録画設定の検証に失敗した: %v", err)
	}
	assertPythonFixture(t, "encode_rec_setting_default", toPythonRecSettingData(EncodeEDCBRecSettingData(defaultSettings)))

	// 有効無効 × 録画モード
	for _, isEnabled := range []bool{true, false} {
		for _, recordingMode := range []string{
			"AllServices", "SpecifiedService", "AllServicesWithoutDecoding",
			"SpecifiedServiceWithoutDecoding", "View",
		} {
			request := recordSettingsRequest{
				IsEnabled:     boolPointer(isEnabled),
				RecordingMode: strPointer(recordingMode),
			}
			settings, err := request.toRecordSettings()
			if err != nil {
				t.Fatalf("録画設定の検証に失敗した: %v", err)
			}
			name := fmt.Sprintf("encode_mode_%d_%s", boolToInt(isEnabled), recordingMode)
			assertPythonFixture(t, name, toPythonRecSettingData(EncodeEDCBRecSettingData(settings)))
		}
	}

	// 字幕 × データ放送の録画モード
	for _, caption := range []string{"Default", "Enable", "Disable"} {
		for _, data := range []string{"Default", "Enable", "Disable"} {
			request := recordSettingsRequest{
				CaptionRecordingMode:          strPointer(caption),
				DataBroadcastingRecordingMode: strPointer(data),
			}
			settings, err := request.toRecordSettings()
			if err != nil {
				t.Fatalf("録画設定の検証に失敗した: %v", err)
			}
			name := fmt.Sprintf("encode_service_mode_%s_%s", caption, data)
			assertPythonFixture(t, name, toPythonRecSettingData(EncodeEDCBRecSettingData(settings)))
		}
	}

	// 録画後動作モード
	for _, postRecordingMode := range []string{
		"Default", "Nothing", "Standby", "StandbyAndReboot", "Suspend", "SuspendAndReboot", "Shutdown",
	} {
		request := recordSettingsRequest{PostRecordingMode: strPointer(postRecordingMode)}
		settings, err := request.toRecordSettings()
		if err != nil {
			t.Fatalf("録画設定の検証に失敗した: %v", err)
		}
		assertPythonFixture(t, "encode_post_"+postRecordingMode, toPythonRecSettingData(EncodeEDCBRecSettingData(settings)))
	}
}

// ---------------------------------------------------------------------------
// C. DecodeEDCBSearchKeyInfo
// ---------------------------------------------------------------------------

// chSet5FixtureServices は Python 側 probe の chset5_services と同じサービス一覧。
func chSet5FixtureServices() []reservations.ChSet5Item {
	return []reservations.ChSet5Item{
		{
			ServiceName: "NHK総合1", NetworkName: "東京", Onid: 32736, Tsid: 32737, Sid: 1024,
			ServiceType: 1, PartialFlag: false, EpgCapFlag: true, SearchFlag: true, RemoconID: 1,
		},
		{
			ServiceName: "NHK教育1", NetworkName: "東京", Onid: 32736, Tsid: 32738, Sid: 1032,
			ServiceType: 1, PartialFlag: false, EpgCapFlag: true, SearchFlag: true, RemoconID: 2,
		},
		{
			ServiceName: "NHK BS1", NetworkName: "BS", Onid: 4, Tsid: 16625, Sid: 101,
			ServiceType: 1, PartialFlag: false, EpgCapFlag: true, SearchFlag: true, RemoconID: 1,
		},
	}
}

// defaultServiceListFixture は ChSet5 の全チャンネル (ONID << 32 | TSID << 16 | SID) 。
func defaultServiceListFixture() []int64 {
	return []int64{
		32736<<32 | 32737<<16 | 1024,
		32736<<32 | 32738<<16 | 1032,
		4<<32 | 16625<<16 | 101,
	}
}

// searchKeyFixture は Python 側 probe の search_key() の既定値と同じ検索条件。
func searchKeyFixture() reservations.SearchKeyInfo {
	return reservations.SearchKeyInfo{
		AndKey:          "",
		NotKey:          "",
		KeyDisabled:     false,
		CaseSensitive:   false,
		RegExpFlag:      false,
		TitleOnlyFlag:   false,
		ContentList:     []reservations.ContentData{},
		DateList:        []reservations.SearchDateInfo{},
		ServiceList:     defaultServiceListFixture(),
		VideoList:       []int{},
		AudioList:       []int{},
		AimaiFlag:       false,
		NotContetFlag:   false,
		NotDateFlag:     false,
		FreeCaFlag:      0,
		ChkRecEnd:       false,
		ChkRecDay:       6,
		ChkRecNoService: false,
		ChkDurationMin:  0,
		ChkDurationMax:  0,
	}
}

func TestPythonParityDecodeEDCBSearchKeyInfo(t *testing.T) {
	server := &Server{}
	client := &fakeEDCBClient{}
	services := chSet5FixtureServices()
	ctx := context.Background()

	decode := func(name string, searchInfo reservations.SearchKeyInfo) {
		t.Helper()
		condition, err := server.decodeEDCBSearchKeyInfo(ctx, searchInfo, client, &services)
		if err != nil {
			t.Fatalf("%s の変換に失敗した: %v", name, err)
		}
		assertPythonFixture(t, name, condition)
	}

	// 全チャンネルが検索対象 (デフォルトと同じなので service_ranges は null になる)
	decode("decode_search_default_service", searchKeyFixture())

	// 一部のチャンネルのみ検索対象
	searchInfo := searchKeyFixture()
	searchInfo.ServiceList = []int64{32736<<32 | 32737<<16 | 1024}
	decode("decode_search_partial_service", searchInfo)

	// サービス範囲が空
	searchInfo = searchKeyFixture()
	searchInfo.ServiceList = []int64{}
	decode("decode_search_empty_service", searchInfo)

	// ジャンル範囲 (中分類あり / すべて / 拡張の user_nibble / 未定義の中分類)
	searchInfo = searchKeyFixture()
	searchInfo.ContentList = []reservations.ContentData{
		{ContentNibble: 0x0<<8 | 0x1, UserNibble: 0},
		{ContentNibble: 0x1<<8 | 0xFF, UserNibble: 0},
		{ContentNibble: 0xE<<8 | 0x0, UserNibble: 0x10},
		{ContentNibble: 0xE<<8 | 0x0, UserNibble: 0x11},
		{ContentNibble: 0xE<<8 | 0x5, UserNibble: 0},
		{ContentNibble: 0xF<<8 | 0xF, UserNibble: 0},
		{ContentNibble: 0x9<<8 | 0x3, UserNibble: 0},
		{ContentNibble: 0x0<<8 | 0xE, UserNibble: 0},
	}
	searchInfo.NotContetFlag = true
	decode("decode_search_genres", searchInfo)

	// 放送日時範囲
	searchInfo = searchKeyFixture()
	searchInfo.DateList = []reservations.SearchDateInfo{
		{StartDayOfWeek: 1, StartHour: 19, StartMin: 0, EndDayOfWeek: 1, EndHour: 22, EndMin: 59},
		{StartDayOfWeek: 6, StartHour: 0, StartMin: 30, EndDayOfWeek: 0, EndHour: 5, EndMin: 0},
	}
	searchInfo.NotDateFlag = true
	decode("decode_search_dates", searchInfo)

	// 番組長の範囲指定 / 放送種別 / 重複チェック
	searchInfo = searchKeyFixture()
	searchInfo.ChkDurationMin = 60
	searchInfo.ChkDurationMax = 600
	searchInfo.FreeCaFlag = 1
	searchInfo.ChkRecEnd = true
	searchInfo.ChkRecDay = 3
	decode("decode_search_duration_free", searchInfo)

	searchInfo = searchKeyFixture()
	searchInfo.FreeCaFlag = 2
	searchInfo.ChkRecEnd = true
	searchInfo.ChkRecDay = 0
	searchInfo.ChkRecNoService = true
	decode("decode_search_paid_all_channels", searchInfo)

	// 放送種別が未知の値の場合は「すべて」扱いになる
	searchInfo = searchKeyFixture()
	searchInfo.FreeCaFlag = 99
	decode("decode_search_free_ca_unknown", searchInfo)

	// メモ欄 (:note:) の解析
	searchInfo = searchKeyFixture()
	searchInfo.NotKey = ":note:メモ\\s込み\\m全角\\\\バックスラッシュ 除外キーワード"
	searchInfo.AndKey = "キーワード"
	searchInfo.KeyDisabled = true
	searchInfo.CaseSensitive = true
	searchInfo.RegExpFlag = true
	searchInfo.TitleOnlyFlag = true
	searchInfo.AimaiFlag = true
	decode("decode_search_note", searchInfo)

	searchInfo = searchKeyFixture()
	searchInfo.NotKey = ":note:メモのみ"
	decode("decode_search_note_only", searchInfo)

	searchInfo = searchKeyFixture()
	searchInfo.NotKey = "除外キーワード"
	decode("decode_search_exclude_only", searchInfo)

	// EDCB へは一切アクセスしていないことを確認する
	if len(client.calls) != 0 {
		t.Errorf("EDCB クライアントが呼び出された: %v", client.calls)
	}
}

// ---------------------------------------------------------------------------
// D. EncodeEDCBSearchKeyInfo
// ---------------------------------------------------------------------------

// pythonContentData / pythonSearchDateInfo / pythonSearchKeyInfo は EDCB へ送る
// SearchKeyInfo を Python 実装と同じ JSON (snake_case) で比較するための写像。
type pythonContentData struct {
	ContentNibble int `json:"content_nibble"`
	UserNibble    int `json:"user_nibble"`
}

type pythonSearchDateInfo struct {
	StartDayOfWeek int `json:"start_day_of_week"`
	StartHour      int `json:"start_hour"`
	StartMin       int `json:"start_min"`
	EndDayOfWeek   int `json:"end_day_of_week"`
	EndHour        int `json:"end_hour"`
	EndMin         int `json:"end_min"`
}

type pythonSearchKeyInfo struct {
	AndKey          string                 `json:"and_key"`
	NotKey          string                 `json:"not_key"`
	KeyDisabled     bool                   `json:"key_disabled"`
	CaseSensitive   bool                   `json:"case_sensitive"`
	RegExpFlag      bool                   `json:"reg_exp_flag"`
	TitleOnlyFlag   bool                   `json:"title_only_flag"`
	ContentList     []pythonContentData    `json:"content_list"`
	DateList        []pythonSearchDateInfo `json:"date_list"`
	ServiceList     []int64                `json:"service_list"`
	VideoList       []int                  `json:"video_list"`
	AudioList       []int                  `json:"audio_list"`
	AimaiFlag       bool                   `json:"aimai_flag"`
	NotContetFlag   bool                   `json:"not_contet_flag"`
	NotDateFlag     bool                   `json:"not_date_flag"`
	FreeCaFlag      int                    `json:"free_ca_flag"`
	ChkRecEnd       bool                   `json:"chk_rec_end"`
	ChkRecDay       int                    `json:"chk_rec_day"`
	ChkRecNoService bool                   `json:"chk_rec_no_service"`
	ChkDurationMin  int                    `json:"chk_duration_min"`
	ChkDurationMax  int                    `json:"chk_duration_max"`
}

func toPythonSearchKeyInfo(info reservations.SearchKeyInfo) pythonSearchKeyInfo {
	contentList := make([]pythonContentData, 0, len(info.ContentList))
	for _, content := range info.ContentList {
		contentList = append(contentList, pythonContentData{
			ContentNibble: content.ContentNibble,
			UserNibble:    content.UserNibble,
		})
	}
	dateList := make([]pythonSearchDateInfo, 0, len(info.DateList))
	for _, date := range info.DateList {
		dateList = append(dateList, pythonSearchDateInfo{
			StartDayOfWeek: date.StartDayOfWeek,
			StartHour:      date.StartHour,
			StartMin:       date.StartMin,
			EndDayOfWeek:   date.EndDayOfWeek,
			EndHour:        date.EndHour,
			EndMin:         date.EndMin,
		})
	}
	serviceList := make([]int64, 0, len(info.ServiceList))
	serviceList = append(serviceList, info.ServiceList...)
	return pythonSearchKeyInfo{
		AndKey:          info.AndKey,
		NotKey:          info.NotKey,
		KeyDisabled:     info.KeyDisabled,
		CaseSensitive:   info.CaseSensitive,
		RegExpFlag:      info.RegExpFlag,
		TitleOnlyFlag:   info.TitleOnlyFlag,
		ContentList:     contentList,
		DateList:        dateList,
		ServiceList:     serviceList,
		VideoList:       info.VideoList,
		AudioList:       info.AudioList,
		AimaiFlag:       info.AimaiFlag,
		NotContetFlag:   info.NotContetFlag,
		NotDateFlag:     info.NotDateFlag,
		FreeCaFlag:      info.FreeCaFlag,
		ChkRecEnd:       info.ChkRecEnd,
		ChkRecDay:       info.ChkRecDay,
		ChkRecNoService: info.ChkRecNoService,
		ChkDurationMin:  info.ChkDurationMin,
		ChkDurationMax:  info.ChkDurationMax,
	}
}

func TestPythonParityEncodeEDCBSearchKeyInfo(t *testing.T) {
	server := &Server{}
	client := &fakeEDCBClient{}
	services := chSet5FixtureServices()
	ctx := context.Background()

	encode := func(name string, request programSearchConditionRequest) {
		t.Helper()
		condition, err := request.toProgramSearchCondition()
		if err != nil {
			t.Fatalf("%s の検証に失敗した: %v", name, err)
		}
		searchInfo, err := server.encodeEDCBSearchKeyInfo(ctx, condition, client, &services)
		if err != nil {
			t.Fatalf("%s の変換に失敗した: %v", name, err)
		}
		assertPythonFixture(t, name, toPythonSearchKeyInfo(searchInfo))
	}

	// すべて既定値
	encode("encode_search_default", programSearchConditionRequest{})

	// service_ranges が null (全チャンネル) / 空リスト (検索対象なし) / 一部指定
	encode("encode_search_service_none", programSearchConditionRequest{ServiceRanges: nil})
	emptyServiceRanges := []programSearchConditionServiceSchema{}
	encode("encode_search_service_empty", programSearchConditionRequest{ServiceRanges: &emptyServiceRanges})
	encode("encode_search_service_partial", programSearchConditionRequest{
		ServiceRanges: &[]programSearchConditionServiceSchema{
			{NetworkID: 32736, TransportStreamID: 32737, ServiceID: 1024},
		},
	})

	// ジャンル範囲 (中分類あり / すべて / 拡張 / 未定義ジャンル)
	encode("encode_search_genres", programSearchConditionRequest{
		GenreRanges: &[]genreSchema{
			{Major: "ニュース・報道", Middle: "天気"},
			{Major: "スポーツ", Middle: "すべて"},
			{Major: "拡張", Middle: "延長の可能性あり"},
			{Major: "拡張", Middle: "存在しない中分類"},
			{Major: "存在しない大分類", Middle: "なにか"},
		},
	})

	// 放送日時範囲
	encode("encode_search_dates", programSearchConditionRequest{
		DateRanges: &[]programSearchConditionDateSchema{
			{
				StartDayOfWeek: 1, StartHour: 19, StartMinute: 0,
				EndDayOfWeek: 1, EndHour: 22, EndMinute: 59,
			},
		},
		IsExcludeDateRanges: boolPointer(true),
	})

	// メモ欄 / 除外キーワード / 放送種別 / 重複チェック / 番組長
	encode("encode_search_note", programSearchConditionRequest{
		Note:                          strPointer("メモ 込み　全角\\バックスラッシュ"),
		ExcludeKeyword:                strPointer("除外キーワード"),
		BroadcastType:                 strPointer("FreeOnly"),
		DuplicateTitleCheckScope:      strPointer("SameChannelOnly"),
		DuplicateTitleCheckPeriodDays: intPointer(3),
		DurationRangeMin:              intPointer(60),
		DurationRangeMax:              intPointer(600),
	})

	// メモ欄のみ
	encode("encode_search_note_only", programSearchConditionRequest{
		Note: strPointer("メモのみ"),
	})

	// 有料のみ / 全てのチャンネルで重複チェック / 重複チェック期間なし
	encode("encode_search_paid_all", programSearchConditionRequest{
		BroadcastType:                 strPointer("PaidOnly"),
		DuplicateTitleCheckScope:      strPointer("AllChannels"),
		DuplicateTitleCheckPeriodDays: intPointer(0),
	})

	// 検索条件が無効 (除外ジャンル範囲)
	encode("encode_search_disabled", programSearchConditionRequest{
		IsEnabled:            boolPointer(false),
		Keyword:              strPointer("キーワード"),
		IsTitleOnly:          boolPointer(true),
		IsCaseSensitive:      boolPointer(true),
		IsRegexSearchEnabled: boolPointer(true),
		IsFuzzySearchEnabled: boolPointer(true),
		IsExcludeGenreRanges: boolPointer(true),
	})

	// ChSet5 を渡しているので EDCB へはアクセスしない
	if len(client.calls) != 0 {
		t.Errorf("EDCB クライアントが呼び出された: %v", client.calls)
	}
}

// ---------------------------------------------------------------------------
// E. DecodeEDCBAutoAddData
// ---------------------------------------------------------------------------

func TestPythonParityDecodeEDCBAutoAddData(t *testing.T) {
	server := &Server{}
	client := &fakeEDCBClient{}
	services := chSet5FixtureServices()

	searchInfo := searchKeyFixture()
	searchInfo.AndKey = "番組名"
	searchInfo.FreeCaFlag = 1
	searchInfo.ChkDurationMin = 30

	autoAddData := reservations.AutoAddData{
		DataID:     7,
		SearchInfo: searchInfo,
		RecSetting: recSettingFixtureA(),
		AddCount:   5,
	}
	response, err := server.decodeEDCBAutoAddData(context.Background(), autoAddData, client, &services)
	if err != nil {
		t.Fatalf("キーワード自動予約条件の変換に失敗した: %v", err)
	}
	assertPythonFixture(t, "decode_auto_add", response)

	if len(client.calls) != 0 {
		t.Errorf("EDCB クライアントが呼び出された: %v", client.calls)
	}
}

// ---------------------------------------------------------------------------
// F. RecordingPresetsRouter
// ---------------------------------------------------------------------------

// epgTimerSrvIniFixture は Python 側 probe の EPG_TIMER_SRV_INI と同じ内容。
const epgTimerSrvIniFixture = `
[SET]
StartMargin=8
EndMargin=3
Caption=1
Data=0
RecEndMode=2
Reboot=0
PresetID=1,2,

[REC_DEF]
SetName=デフォルト
RecMode=1
Priority=2
TuijyuuFlag=1
ServiceMode=0
PittariFlag=0
BatFilePath=
SuspendMode=0
RebootFlag=0
UseMargineFlag=0
ContinueRec=0
PartialRec=0
TunerID=0

[REC_DEF_FOLDER]
Count=1
0=C:\rec
RecNamePlugIn0=RecName_Macro.dll?$title$.ts

[REC_DEF_FOLDER_1SEG]
Count=1
0=C:\rec1seg
RecNamePlugIn0=RecName_Macro.dll

[REC_DEF1]
SetName=プリセット1
RecMode=9
NoRecMode=2
Priority=9
TuijyuuFlag=0
ServiceMode=49
PittariFlag=1
BatFilePath=C:\post.bat
SuspendMode=2
RebootFlag=1
UseMargineFlag=1
StartMargine=15
EndMargine=7
ContinueRec=1
PartialRec=1
TunerID=2

[REC_DEF_FOLDER1]
Count=2
0=D:\rec1
1=D:\rec2
RecNamePlugIn1=RecName_Macro.dll?$title$_$service$

[REC_DEF_FOLDER_1SEG1]
Count=1
0=D:\rec1seg

[REC_DEF2]
SetName=壊れたプリセット
Priority=not-a-number
`

func TestPythonParityRecordingPresets(t *testing.T) {
	config, err := reservations.ParseEDCBIni(epgTimerSrvIniFixture, true)
	if err != nil {
		t.Fatalf("EDCB の ini を解析できなかった: %v", err)
	}

	// 全体設定の既定値
	globalDefaults, err := parseGlobalDefaults(config)
	if err != nil {
		t.Fatalf("全体設定の既定値を取得できなかった: %v", err)
	}
	assertPythonFixture(t, "parse_global_defaults", globalDefaults)

	// 録画プリセット
	for _, presetID := range []int{0, 1, 3} {
		preset, err := parsePreset(config, presetID)
		if err != nil {
			t.Fatalf("録画プリセット %d を取得できなかった: %v", presetID, err)
		}
		assertPythonFixture(t, fmt.Sprintf("parse_preset_%d", presetID), preset)
	}

	// 壊れた値 (Priority=not-a-number) は Python 版と同じくエラーになる
	if _, err := parsePreset(config, 2); err == nil {
		t.Error("parse_preset_2 はエラーになるべきだがエラーにならなかった")
	}

	// 録画フォルダ
	folders, err := parseRecordingFolders(config, "REC_DEF_FOLDER", false)
	if err != nil {
		t.Fatalf("録画フォルダを取得できなかった: %v", err)
	}
	assertPythonFixture(t, "parse_recording_folders_0", folders)

	onesegFolders, err := parseRecordingFolders(config, "REC_DEF_FOLDER_1SEG1", true)
	if err != nil {
		t.Fatalf("1 セグ用の録画フォルダを取得できなかった: %v", err)
	}
	assertPythonFixture(t, "parse_recording_folders_1seg_1", onesegFolders)

	missingFolders, err := parseRecordingFolders(config, "REC_DEF_FOLDER_MISSING", false)
	if err != nil {
		t.Fatalf("存在しないセクションの録画フォルダでエラーになった: %v", err)
	}
	assertPythonFixture(t, "parse_recording_folders_missing", missingFolders)

	// ini の解析結果 (キー順は意味を持たないので意味的に比較する)
	assertPythonFixtureSemantic(t, "parse_edcb_ini", map[string]map[string]string{
		"SET":      config.Items("SET"),
		"REC_DEF1": config.Items("REC_DEF1"),
	})
}

// ---------------------------------------------------------------------------
// G. TSInformation / EDCBUtil の移植部分
// ---------------------------------------------------------------------------

func TestPythonParityTSInformation(t *testing.T) {
	// formatString (全角英数字・全角スペースなどの変換)
	for _, value := range []string{
		"ＮＨＫ　総合１",
		"ＡＢＣＤＥＦＧＨＩＪＫＬＭＮＯＰＱＲＳＴＵＶＷＸＹＺ",
		"（テスト）　１２３！",
		"！＂＃＄％＆＇（）＊＋，－．／：；＜＝＞？＠［＼］＾＿｀｛｜｝～",
		"ﾊﾝｶｸｶﾅ",
		"ＮＨＫ　総合１　サブ",
		"全角カナ　半角カナ 混在",
	} {
		assertPythonFixture(t, "format_string_"+value, reservations.FormatString(value))
	}

	// ネットワーク種別
	for _, networkID := range []int{32736, 32737, 32738, 4, 6, 7, 32391, 32381, 32383, 32735} {
		assertPythonFixture(t, fmt.Sprintf("network_type_%d", networkID), reservations.GetNetworkType(networkID))
	}

	// リモコン ID (GR は Python 版でも未対応で例外になるため対象外)
	for _, channelType := range []string{"BS", "CS", "CATV", "SKY", "BS4K"} {
		for _, serviceID := range []int{101, 161} {
			name := fmt.Sprintf("remocon_id_%s_%d", channelType, serviceID)
			if _, ok := pythonReservationFixtures[name]; !ok {
				continue
			}
			assertPythonFixture(t, name, reservations.CalculateRemoconID(channelType, serviceID))
		}
	}

	// サブチャンネルかどうか
	for _, channelType := range []string{"BS", "CS", "CATV", "SKY", "BS4K", "GR"} {
		for _, serviceID := range []int{101, 161, 1024} {
			name := fmt.Sprintf("is_subchannel_%s_%d", channelType, serviceID)
			if _, ok := pythonReservationFixtures[name]; !ok {
				continue
			}
			assertPythonFixture(t, name, reservations.CalculateIsSubchannel(channelType, serviceID))
		}
	}
}

func TestPythonParityEDCBUtil(t *testing.T) {
	// 日時 → FILETIME (Python 版と同じく壁時計の時刻をそのまま使う)
	assertPythonFixture(t, "filetime_2026-01-02T03:04:05",
		reservations.DateTimeToFileTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), 0))

	// Python 版の datetimeToFileTime() は tzinfo を無視するため、+09:00 の壁時計でも同じ値になる。
	// Go 版はタイムゾーンを明示的に受け取るので、JST の時刻に +9 時間を渡すと同じ結果になる。
	jst := time.FixedZone("JST", 9*60*60)
	assertPythonFixture(t, "filetime_2026-01-02T03:04:05+09:00",
		reservations.DateTimeToFileTime(time.Date(2026, 1, 2, 3, 4, 5, 0, jst), 9*60*60))

	// ChSet5.txt の解析
	chset5Text := "NHK総合1\t東京\t32736\t32737\t1024\t1\t0\t1\t1\t1\r\nNHK BS1\tBS\t4\t16625\t101\t1\t0\t1\t1\t1\r\n"
	items := reservations.ParseChSet5(chset5Text)
	expected := []reservations.ChSet5Item{
		{
			ServiceName: "NHK総合1", NetworkName: "東京", Onid: 32736, Tsid: 32737, Sid: 1024,
			ServiceType: 1, PartialFlag: false, EpgCapFlag: true, SearchFlag: true, RemoconID: 1,
		},
		{
			ServiceName: "NHK BS1", NetworkName: "BS", Onid: 4, Tsid: 16625, Sid: 101,
			ServiceType: 1, PartialFlag: false, EpgCapFlag: true, SearchFlag: true, RemoconID: 1,
		},
	}
	if !reflect.DeepEqual(items, expected) {
		t.Errorf("ChSet5.txt の解析結果が Python 実装と異なる\n期待: %+v\n実際: %+v", expected, items)
	}
}

// pythonFixturesNotCompared は意図的に突き合わせていないフィクスチャと、その理由。
var pythonFixturesNotCompared = map[string]string{
	// ChSet5.txt は Go 側では JSON 化されない構造体を経由するため、TestPythonParityEDCBUtil 内で
	// フィールド単位の比較 (reflect.DeepEqual) を行っている。
	"parse_chset5": "構造体としてフィールド単位で比較している",
	// 壊れた値 (Priority=not-a-number) は Python 版でも例外になるため、期待値は例外の型名。
	"parse_preset_2": "Python 版も例外になるため、エラーになることだけを確認している",
}

// TestZZAllPythonFixturesConsumed は、機械生成した期待値がすべて実際に突き合わせられたことを確認する。
//
// フィクスチャが黙って使われないまま残ると、移植漏れを見逃すため。
// (Go はファイル名→宣言順にテストを実行するため、このテストは最後に実行される)
func TestZZAllPythonFixturesConsumed(t *testing.T) {
	for name := range pythonReservationFixtures {
		if usedPythonFixtures[name] {
			continue
		}
		if reason, ok := pythonFixturesNotCompared[name]; ok {
			t.Logf("%s は意図的に突き合わせていない (%s)", name, reason)
			continue
		}
		t.Errorf("フィクスチャ %s がどのテストでも突き合わせられていない", name)
	}
}
