package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/routers/RecordingPresetsRouter.py のエンドポイントを移植したもの。
//
// - GET /api/recording/presets

// recordSettingsPresetRouteRegistered は登録済みのルート数を表す (重複登録の防止用ではない) 。

// registerRecordingPresetRoutes は録画設定プリセット系のルートを mux に登録する。
// 移植元: server/app/routers/RecordingPresetsRouter.py
func (s *Server) registerRecordingPresetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/recording/presets", s.handleRecordingPresets)
}

// iniInt は ini セクションの値を Python の int() と同じように整数に変換する。
func iniInt(section map[string]string, key string, fallback string) (int, error) {
	value, ok := section[key]
	if !ok {
		value = fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid int value for %q: %q", key, value)
	}
	return parsed, nil
}

// iniString は ini セクションの文字列値を取得する (存在しない場合は fallback) 。
func iniString(section map[string]string, key string, fallback string) string {
	if value, ok := section[key]; ok {
		return value
	}
	return fallback
}

// parseGlobalDefaults は EpgTimerSrv.ini の [SET] セクションからグローバルデフォルト値をパースする。
//
// 移植元: RecordingPresetsRouter.ParseGlobalDefaults()
func parseGlobalDefaults(config *reservations.IniConfig) (recordSettingsGlobalDefaults, error) {
	setSection := config.Items("SET")

	// 録画開始マージン (デフォルト: 5秒)
	recordingStartMargin, err := iniInt(setSection, "StartMargin", "5")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}

	// 録画終了マージン (デフォルト: 2秒)
	recordingEndMargin, err := iniInt(setSection, "EndMargin", "2")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}

	// 字幕録画 (デフォルト: 1 = 録画する)
	captionValue, err := iniInt(setSection, "Caption", "1")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}
	captionRecordingMode := "Disable"
	if captionValue != 0 {
		captionRecordingMode = "Enable"
	}

	// データ放送録画 (デフォルト: 0 = 録画しない)
	dataValue, err := iniInt(setSection, "Data", "0")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}
	dataBroadcastingRecordingMode := "Disable"
	if dataValue != 0 {
		dataBroadcastingRecordingMode = "Enable"
	}

	// 録画後動作 (RecEndMode と Reboot の組み合わせ)
	// RecEndMode: 0=何もしない, 1=スタンバイ, 2=休止, 3=シャットダウン / Reboot: 0=再起動しない, 1=再起動する
	recEndMode, err := iniInt(setSection, "RecEndMode", "2")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}
	reboot, err := iniInt(setSection, "Reboot", "0")
	if err != nil {
		return recordSettingsGlobalDefaults{}, err
	}
	postRecordingMode := ""
	if recEndMode == 1 && reboot == 0 {
		postRecordingMode = "Standby"
	} else if recEndMode == 1 && reboot != 0 {
		postRecordingMode = "StandbyAndReboot"
	} else if recEndMode == 2 && reboot == 0 {
		postRecordingMode = "Suspend"
	} else if recEndMode == 2 && reboot != 0 {
		postRecordingMode = "SuspendAndReboot"
	} else if recEndMode == 3 {
		postRecordingMode = "Shutdown"
	} else {
		// rec_end_mode == 0 または範囲外 (何もしない)
		postRecordingMode = "Nothing"
	}

	return recordSettingsGlobalDefaults{
		RecordingStartMargin:          recordingStartMargin,
		RecordingEndMargin:            recordingEndMargin,
		CaptionRecordingMode:          captionRecordingMode,
		DataBroadcastingRecordingMode: dataBroadcastingRecordingMode,
		PostRecordingMode:             postRecordingMode,
	}, nil
}

// parseRecordingFolders は EpgTimerSrv.ini の録画フォルダセクションをパースする。
//
// 移植元: RecordingPresetsRouter.ParseRecordingFolders()
func parseRecordingFolders(config *reservations.IniConfig, sectionName string, isOneseg bool) ([]recordingFolderSchema, error) {
	folders := []recordingFolderSchema{}
	if !config.HasSection(sectionName) {
		return folders, nil
	}

	section := config.Items(sectionName)
	folderCount, err := iniInt(section, "Count", "0")
	if err != nil {
		return nil, err
	}
	for index := 0; index < folderCount; index++ {
		folderPath := iniString(section, strconv.Itoa(index), "")
		if folderPath == "" {
			continue
		}

		// RecNamePlugIn から ? 以降のファイル名テンプレート部分を抽出
		// (RecName_Macro.dll?$title$.ts のような形式で格納されている)
		recNamePlugIn := iniString(section, fmt.Sprintf("RecNamePlugIn%d", index), "")
		var recordingFileNameTemplate *string
		if questionIndex := strings.Index(recNamePlugIn, "?"); questionIndex >= 0 {
			template := recNamePlugIn[questionIndex+1:]
			if template != "" {
				recordingFileNameTemplate = &template
			}
		}

		folders = append(folders, recordingFolderSchema{
			RecordingFolderPath:             folderPath,
			RecordingFileNameTemplate:       recordingFileNameTemplate,
			IsOnesegSeparateRecordingFolder: isOneseg,
		})
	}
	return folders, nil
}

// parsePreset は EpgTimerSrv.ini から指定された ID の録画設定プリセットをパースする。
//
// 移植元: RecordingPresetsRouter.ParsePreset() (EDCB の PresetItem.cs の LoadPresetData() のロジック)
func parsePreset(config *reservations.IniConfig, presetID int) (recordSettingsPresetResponse, error) {
	// セクション名の決定 (ID=0 のデフォルトプリセットは [REC_DEF], それ以外は [REC_DEF{ID}])
	idSuffix := ""
	if presetID != 0 {
		idSuffix = strconv.Itoa(presetID)
	}
	sectionName := "REC_DEF" + idSuffix
	section := map[string]string{}
	if config.HasSection(sectionName) {
		section = config.Items(sectionName)
	}

	// プリセット名 (デフォルト: 「デフォルト」)
	presetName := iniString(section, "SetName", "デフォルト")

	// RecMode: 0-4=有効, 5-9=無効
	// EDCB の PresetItem.cs のロジック: IsEnable = RecMode / 5 % 2 == 0 (つまり RecMode <= 4 なら有効)
	// 無効時は NoRecMode を使って実際のモードを取得
	rawRecMode, err := iniInt(section, "RecMode", "1")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	noRecMode, err := iniInt(section, "NoRecMode", "1")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	isEnabled := rawRecMode <= 4
	effectiveRecMode := rawRecMode
	if !isEnabled {
		effectiveRecMode = noRecMode
	}

	// 録画モードの変換
	recordingModeMap := map[int]string{
		0: "AllServices",
		1: "SpecifiedService",
		2: "AllServicesWithoutDecoding",
		3: "SpecifiedServiceWithoutDecoding",
		4: "View",
	}
	recordingMode := recordingModeMap[effectiveRecMode]
	if recordingMode == "" {
		recordingMode = "SpecifiedService"
	}

	// 優先度 (デフォルト: 2, 範囲: 1-5)
	priorityValue, err := iniInt(section, "Priority", "2")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	priority := priorityValue
	if priority < 1 {
		priority = 1
	}
	if priority > 5 {
		priority = 5
	}

	// イベントリレー追従 (デフォルト: 1 = 有効)
	tuijyuuFlag, err := iniInt(section, "TuijyuuFlag", "1")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	isEventRelayFollowEnabled := tuijyuuFlag != 0

	// 字幕/データ放送の service_mode ビットフラグ
	// 0x00000001: 個別の設定値を使用 / 0x00000010: 字幕データを含む / 0x00000020: データカルーセルを含む
	serviceMode, err := iniInt(section, "ServiceMode", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	captionRecordingMode := "Default"
	dataBroadcastingRecordingMode := "Default"
	if serviceMode&0x01 != 0 {
		captionRecordingMode = "Disable"
		if serviceMode&0x10 != 0 {
			captionRecordingMode = "Enable"
		}
		dataBroadcastingRecordingMode = "Disable"
		if serviceMode&0x20 != 0 {
			dataBroadcastingRecordingMode = "Enable"
		}
	}

	// ぴったり録画 (デフォルト: 0 = 無効)
	pittariFlag, err := iniInt(section, "PittariFlag", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	isExactRecordingEnabled := pittariFlag != 0

	// 録画後 bat ファイルパス
	var postRecordingBatFilePath *string
	if batFilePath := iniString(section, "BatFilePath", ""); batFilePath != "" {
		postRecordingBatFilePath = &batFilePath
	}

	// 録画後動作
	// SuspendMode: 0=デフォルト設定を使う, 1=スタンバイ, 2=休止, 3=シャットダウン, 4=何もしない
	// RebootFlag: 0=再起動しない, 1=復帰後再起動する
	suspendMode, err := iniInt(section, "SuspendMode", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	rebootFlagValue, err := iniInt(section, "RebootFlag", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	rebootFlag := rebootFlagValue != 0
	postRecordingMode := ""
	if suspendMode == 0 {
		postRecordingMode = "Default"
	} else if suspendMode == 1 && !rebootFlag {
		postRecordingMode = "Standby"
	} else if suspendMode == 1 && rebootFlag {
		postRecordingMode = "StandbyAndReboot"
	} else if suspendMode == 2 && !rebootFlag {
		postRecordingMode = "Suspend"
	} else if suspendMode == 2 && rebootFlag {
		postRecordingMode = "SuspendAndReboot"
	} else if suspendMode == 3 {
		postRecordingMode = "Shutdown"
	} else if suspendMode == 4 {
		postRecordingMode = "Nothing"
	} else {
		postRecordingMode = "Default"
	}

	// マージン設定
	// UseMargineFlag: 0=グローバルデフォルトを使う, 1=個別指定
	// (EDCB のスペルミス ("Margine") をそのまま使う)
	useMarginFlagValue, err := iniInt(section, "UseMargineFlag", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	var recordingStartMargin *int
	var recordingEndMargin *int
	if useMarginFlagValue != 0 {
		startMargin, err := iniInt(section, "StartMargine", "5")
		if err != nil {
			return recordSettingsPresetResponse{}, err
		}
		endMargin, err := iniInt(section, "EndMargine", "2")
		if err != nil {
			return recordSettingsPresetResponse{}, err
		}
		recordingStartMargin = &startMargin
		recordingEndMargin = &endMargin
	}

	// 連続録画 (デフォルト: 0 = 無効)
	continueRec, err := iniInt(section, "ContinueRec", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}

	// ワンセグ分離出力 (デフォルト: 0 = 無効)
	partialRec, err := iniInt(section, "PartialRec", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}

	// チューナー強制指定 (デフォルト: 0 = 自動選択)
	tunerID, err := iniInt(section, "TunerID", "0")
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	var forcedTunerID *int
	if tunerID != 0 {
		forcedTunerID = &tunerID
	}

	// 録画フォルダ情報のパース
	// 通常録画フォルダ: [REC_DEF_FOLDER] or [REC_DEF_FOLDER{ID}]
	recordingFolders, err := parseRecordingFolders(config, "REC_DEF_FOLDER"+idSuffix, false)
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	// ワンセグ録画フォルダ: [REC_DEF_FOLDER_1SEG] or [REC_DEF_FOLDER_1SEG{ID}]
	onesegFolders, err := parseRecordingFolders(config, "REC_DEF_FOLDER_1SEG"+idSuffix, true)
	if err != nil {
		return recordSettingsPresetResponse{}, err
	}
	recordingFolders = append(recordingFolders, onesegFolders...)

	return recordSettingsPresetResponse{
		ID:   presetID,
		Name: presetName,
		RecordSettings: recordSettings{
			IsEnabled:                                isEnabled,
			Priority:                                 priority,
			RecordingFolders:                         recordingFolders,
			RecordingStartMargin:                     recordingStartMargin,
			RecordingEndMargin:                       recordingEndMargin,
			RecordingMode:                            recordingMode,
			CaptionRecordingMode:                     captionRecordingMode,
			DataBroadcastingRecordingMode:            dataBroadcastingRecordingMode,
			PostRecordingMode:                        postRecordingMode,
			PostRecordingBatFilePath:                 postRecordingBatFilePath,
			IsEventRelayFollowEnabled:                isEventRelayFollowEnabled,
			IsExactRecordingEnabled:                  isExactRecordingEnabled,
			IsOnesegSeparateOutputEnabled:            partialRec == 1,
			IsSequentialRecordingInSingleFileEnabled: continueRec != 0,
			ForcedTunerID:                            forcedTunerID,
		},
	}, nil
}

// handleRecordingPresets は録画設定プリセット一覧 API を処理する。
// 移植元: RecordingPresetsRouter.RecordingPresetsAPI()
func (s *Server) handleRecordingPresets(w http.ResponseWriter, r *http.Request) {
	client, ok := s.getEDCBClient(w)
	if !ok {
		return
	}

	// EDCB から EpgTimerSrv.ini を取得
	files, ok := client.FileCopy2([]string{"EpgTimerSrv.ini"})
	if !ok || len(files) == 0 {
		s.logger.Error("[RecordingPresetsRouter][RecordingPresetsAPI] Failed to get EpgTimerSrv.ini from EDCB.")
		writeError(w, http.StatusInternalServerError, "Failed to get EpgTimerSrv.ini from EDCB")
		return
	}
	iniData := files[0].Data
	if len(iniData) == 0 {
		s.logger.Error("[RecordingPresetsRouter][RecordingPresetsAPI] EpgTimerSrv.ini is empty.")
		writeError(w, http.StatusInternalServerError, "EpgTimerSrv.ini is empty")
		return
	}

	// バイナリデータを文字列に変換 (BOM 判定付き)
	iniText := reservations.ConvertEDCBBytesToString(iniData)

	// EDCB 由来の ini を重複キーを許容して解析
	// (EpgTimerSrv.ini のキー名は PascalCase なので、録画設定プリセットの読み取りでは大文字小文字を保持する)
	config, err := reservations.ParseEDCBIni(iniText, true)
	if err != nil {
		s.logger.Error("[RecordingPresetsRouter][RecordingPresetsAPI] Failed to parse EpgTimerSrv.ini.", "error", err)
		writeError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// グローバルデフォルト値のパース
	globalDefaults, err := parseGlobalDefaults(config)
	if err != nil {
		s.logger.Error("[RecordingPresetsRouter][RecordingPresetsAPI] Failed to parse global defaults from EpgTimerSrv.ini.", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to parse global defaults from EpgTimerSrv.ini")
		return
	}

	// プリセット一覧のパース
	presets := []recordSettingsPresetResponse{}

	// ID=0 のデフォルトプリセットは仕様上常に存在する
	defaultPreset, err := parsePreset(config, 0)
	if err != nil {
		s.logger.Error("[RecordingPresetsRouter][RecordingPresetsAPI] Failed to parse default preset (ID=0).", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to parse default preset from EpgTimerSrv.ini")
		return
	}
	presets = append(presets, defaultPreset)

	// カスタムプリセットの ID リストを [SET] セクションの PresetID キーから取得
	// (PresetID の値はカンマ区切りの ID リスト (例: "1,2,3,") 。末尾にカンマが付くことがあるため空文字列のスキップが必要)
	setSection := config.Items("SET")
	presetIDStr := iniString(setSection, "PresetID", "")
	if presetIDStr != "" {
		for _, idStr := range strings.Split(presetIDStr, ",") {
			idStr = strings.TrimSpace(idStr)
			if idStr == "" {
				continue
			}
			presetID, err := strconv.Atoi(idStr)
			if err != nil {
				continue
			}
			// ID=0 は既に追加済みなのでスキップ
			if presetID == 0 {
				continue
			}
			// カスタムプリセットのパース (失敗した場合はスキップして続行)
			preset, err := parsePreset(config, presetID)
			if err != nil {
				s.logger.Warn(
					"[RecordingPresetsRouter][RecordingPresetsAPI] Failed to parse preset, skipping.",
					"preset_id", presetID, "error", err,
				)
				continue
			}
			presets = append(presets, preset)
		}
	}

	writeJSON(w, http.StatusOK, recordSettingsPresetsResponse{
		GlobalDefaults: globalDefaults,
		Presets:        presets,
	})
}
