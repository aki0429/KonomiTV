package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

// このファイルは server/app/schemas.py の録画予約系モデルと、
// server/app/routers/ReservationsRouter.py の
// DecodeEDCBRecSettingData() / EncodeEDCBRecSettingData() を移植したもの。

// recordSettingsRequest は schemas.RecordSettings を受けるリクエスト用の構造体。
//
// Pydantic は未指定のフィールドに既定値を入れるため、ポインタで受けてから既定値を適用する。
type recordSettingsRequest struct {
	IsEnabled                                *bool                    `json:"is_enabled"`
	Priority                                 *int                     `json:"priority"`
	RecordingFolders                         *[]recordingFolderSchema `json:"recording_folders"`
	RecordingStartMargin                     *int                     `json:"recording_start_margin"`
	RecordingEndMargin                       *int                     `json:"recording_end_margin"`
	RecordingMode                            *string                  `json:"recording_mode"`
	CaptionRecordingMode                     *string                  `json:"caption_recording_mode"`
	DataBroadcastingRecordingMode            *string                  `json:"data_broadcasting_recording_mode"`
	PostRecordingMode                        *string                  `json:"post_recording_mode"`
	PostRecordingBatFilePath                 *string                  `json:"post_recording_bat_file_path"`
	IsEventRelayFollowEnabled                *bool                    `json:"is_event_relay_follow_enabled"`
	IsExactRecordingEnabled                  *bool                    `json:"is_exact_recording_enabled"`
	IsOnesegSeparateOutputEnabled            *bool                    `json:"is_oneseg_separate_output_enabled"`
	IsSequentialRecordingInSingleFileEnabled *bool                    `json:"is_sequential_recording_in_single_file_enabled"`
	ForcedTunerID                            *int                     `json:"forced_tuner_id"`
}

// recordSettings は schemas.RecordSettings 互換の構造体 (リクエスト・レスポンス共通) 。
type recordSettings struct {
	IsEnabled                                bool                    `json:"is_enabled"`
	Priority                                 int                     `json:"priority"`
	RecordingFolders                         []recordingFolderSchema `json:"recording_folders"`
	RecordingStartMargin                     *int                    `json:"recording_start_margin"`
	RecordingEndMargin                       *int                    `json:"recording_end_margin"`
	RecordingMode                            string                  `json:"recording_mode"`
	CaptionRecordingMode                     string                  `json:"caption_recording_mode"`
	DataBroadcastingRecordingMode            string                  `json:"data_broadcasting_recording_mode"`
	PostRecordingMode                        string                  `json:"post_recording_mode"`
	PostRecordingBatFilePath                 *string                 `json:"post_recording_bat_file_path"`
	IsEventRelayFollowEnabled                bool                    `json:"is_event_relay_follow_enabled"`
	IsExactRecordingEnabled                  bool                    `json:"is_exact_recording_enabled"`
	IsOnesegSeparateOutputEnabled            bool                    `json:"is_oneseg_separate_output_enabled"`
	IsSequentialRecordingInSingleFileEnabled bool                    `json:"is_sequential_recording_in_single_file_enabled"`
	ForcedTunerID                            *int                    `json:"forced_tuner_id"`
}

// recordingFolderSchema は schemas.RecordingFolder 互換の構造体。
type recordingFolderSchema struct {
	RecordingFolderPath             string  `json:"recording_folder_path"`
	RecordingFileNameTemplate       *string `json:"recording_file_name_template"`
	IsOnesegSeparateRecordingFolder bool    `json:"is_oneseg_separate_recording_folder"`
}

// validRecordingModes は schemas.RecordSettings.recording_mode の Literal 値。
var validRecordingModes = map[string]bool{
	"AllServices":                     true,
	"AllServicesWithoutDecoding":      true,
	"SpecifiedService":                true,
	"SpecifiedServiceWithoutDecoding": true,
	"View":                            true,
}

// validTriStateModes は字幕 / データ放送の録画設定の Literal 値。
var validTriStateModes = map[string]bool{"Default": true, "Enable": true, "Disable": true}

// validPostRecordingModes は録画後動作モードの Literal 値。
var validPostRecordingModes = map[string]bool{
	"Default": true, "Nothing": true, "Standby": true, "StandbyAndReboot": true,
	"Suspend": true, "SuspendAndReboot": true, "Shutdown": true,
}

// toRecordSettings はリクエストの構造体に Pydantic と同じ既定値を適用して検証する。
func (request recordSettingsRequest) toRecordSettings() (recordSettings, error) {
	settings := recordSettings{
		IsEnabled:                     true,
		Priority:                      3,
		RecordingFolders:              []recordingFolderSchema{},
		RecordingMode:                 "SpecifiedService",
		CaptionRecordingMode:          "Default",
		DataBroadcastingRecordingMode: "Default",
		PostRecordingMode:             "Default",
		IsEventRelayFollowEnabled:     true,
	}
	if request.IsEnabled != nil {
		settings.IsEnabled = *request.IsEnabled
	}
	if request.Priority != nil {
		settings.Priority = *request.Priority
	}
	if settings.Priority < 1 || settings.Priority > 5 {
		return settings, fmt.Errorf("Input should be greater than or equal to 1 and less than or equal to 5")
	}
	if request.RecordingFolders != nil {
		settings.RecordingFolders = *request.RecordingFolders
	}
	settings.RecordingStartMargin = request.RecordingStartMargin
	settings.RecordingEndMargin = request.RecordingEndMargin
	if request.RecordingMode != nil {
		if !validRecordingModes[*request.RecordingMode] {
			return settings, fmt.Errorf("Input should be 'AllServices', 'AllServicesWithoutDecoding', 'SpecifiedService', 'SpecifiedServiceWithoutDecoding' or 'View'")
		}
		settings.RecordingMode = *request.RecordingMode
	}
	if request.CaptionRecordingMode != nil {
		if !validTriStateModes[*request.CaptionRecordingMode] {
			return settings, fmt.Errorf("Input should be 'Default', 'Enable' or 'Disable'")
		}
		settings.CaptionRecordingMode = *request.CaptionRecordingMode
	}
	if request.DataBroadcastingRecordingMode != nil {
		if !validTriStateModes[*request.DataBroadcastingRecordingMode] {
			return settings, fmt.Errorf("Input should be 'Default', 'Enable' or 'Disable'")
		}
		settings.DataBroadcastingRecordingMode = *request.DataBroadcastingRecordingMode
	}
	if request.PostRecordingMode != nil {
		if !validPostRecordingModes[*request.PostRecordingMode] {
			return settings, fmt.Errorf("Input should be 'Default', 'Nothing', 'Standby', 'StandbyAndReboot', 'Suspend', 'SuspendAndReboot' or 'Shutdown'")
		}
		settings.PostRecordingMode = *request.PostRecordingMode
	}
	settings.PostRecordingBatFilePath = request.PostRecordingBatFilePath
	if request.IsEventRelayFollowEnabled != nil {
		settings.IsEventRelayFollowEnabled = *request.IsEventRelayFollowEnabled
	}
	if request.IsExactRecordingEnabled != nil {
		settings.IsExactRecordingEnabled = *request.IsExactRecordingEnabled
	}
	if request.IsOnesegSeparateOutputEnabled != nil {
		settings.IsOnesegSeparateOutputEnabled = *request.IsOnesegSeparateOutputEnabled
	}
	if request.IsSequentialRecordingInSingleFileEnabled != nil {
		settings.IsSequentialRecordingInSingleFileEnabled = *request.IsSequentialRecordingInSingleFileEnabled
	}
	if request.ForcedTunerID != nil {
		if *request.ForcedTunerID < 0 {
			return settings, fmt.Errorf("Input should be greater than or equal to 0")
		}
		settings.ForcedTunerID = request.ForcedTunerID
	}
	return settings, nil
}

// DecodeEDCBRecSettingData は EDCB の RecSettingData を schemas.RecordSettings 互換の構造体に変換する。
//
// 移植元: ReservationsRouter.DecodeEDCBRecSettingData()
func DecodeEDCBRecSettingData(data reservations.RecSettingData) recordSettings {
	// 録画予約が有効かどうか (0 ~ 4 なら有効)
	isEnabled := data.RecMode <= 4

	// 保存先の録画フォルダのパスのリスト
	recordingFolders := []recordingFolderSchema{}
	for _, key := range []string{"rec_folder_list", "partial_rec_folder"} {
		folderList := data.RecFolderList
		if key == "partial_rec_folder" {
			folderList = data.PartialRecFolder
		}
		for _, recFolder := range folderList {
			// rec_name_plug_in は ? 以降が録画ファイル名テンプレート (マクロ) の値になっている
			fileNameTemplate := ""
			if index := indexOf(recFolder.RecNamePlugIn, '?'); index >= 0 {
				fileNameTemplate = recFolder.RecNamePlugIn[index+1:]
			}
			var template *string
			if fileNameTemplate != "" {
				template = &fileNameTemplate
			}
			recordingFolders = append(recordingFolders, recordingFolderSchema{
				RecordingFolderPath:             recFolder.RecFolder,
				RecordingFileNameTemplate:       template,
				IsOnesegSeparateRecordingFolder: key == "partial_rec_folder",
			})
		}
	}

	// 録画モード
	recordingMode := "SpecifiedService"
	switch data.RecMode {
	case 0, 9:
		recordingMode = "AllServices"
	case 1, 5:
		recordingMode = "SpecifiedService"
	case 2, 6:
		recordingMode = "AllServicesWithoutDecoding"
	case 3, 7:
		recordingMode = "SpecifiedServiceWithoutDecoding"
	case 4, 8:
		recordingMode = "View"
	}

	// 字幕データ / データ放送の録画設定
	captionRecordingMode := "Default"
	dataBroadcastingRecordingMode := "Default"
	if data.ServiceMode&0x00000001 != 0 {
		captionRecordingMode = "Disable"
		if data.ServiceMode&0x00000010 != 0 {
			captionRecordingMode = "Enable"
		}
		dataBroadcastingRecordingMode = "Disable"
		if data.ServiceMode&0x00000020 != 0 {
			dataBroadcastingRecordingMode = "Enable"
		}
	}

	// 録画後動作モード
	postRecordingMode := "Default"
	switch {
	case data.SuspendMode == 0:
		postRecordingMode = "Default"
	case data.SuspendMode == 1 && !data.RebootFlag:
		postRecordingMode = "Standby"
	case data.SuspendMode == 1 && data.RebootFlag:
		postRecordingMode = "StandbyAndReboot"
	case data.SuspendMode == 2 && !data.RebootFlag:
		postRecordingMode = "Suspend"
	case data.SuspendMode == 2 && data.RebootFlag:
		postRecordingMode = "SuspendAndReboot"
	case data.SuspendMode == 3:
		postRecordingMode = "Shutdown"
	case data.SuspendMode == 4:
		postRecordingMode = "Nothing"
	}

	// 録画後に実行する bat ファイルのパス
	var batFilePath *string
	if data.BatFilePath != "" {
		batFilePath = &data.BatFilePath
	}

	// チューナーを強制指定する際のチューナー ID (0 は自動選択)
	var forcedTunerID *int
	if data.TunerID != 0 {
		forcedTunerID = &data.TunerID
	}

	return recordSettings{
		IsEnabled:                                isEnabled,
		Priority:                                 data.Priority,
		RecordingFolders:                         recordingFolders,
		RecordingStartMargin:                     data.StartMargin,
		RecordingEndMargin:                       data.EndMargin,
		RecordingMode:                            recordingMode,
		CaptionRecordingMode:                     captionRecordingMode,
		DataBroadcastingRecordingMode:            dataBroadcastingRecordingMode,
		PostRecordingMode:                        postRecordingMode,
		PostRecordingBatFilePath:                 batFilePath,
		IsEventRelayFollowEnabled:                data.TuijyuuFlag,
		IsExactRecordingEnabled:                  data.PittariFlag,
		IsOnesegSeparateOutputEnabled:            data.PartialRecFlag == 1,
		IsSequentialRecordingInSingleFileEnabled: data.ContinueRec,
		ForcedTunerID:                            forcedTunerID,
	}
}

// EncodeEDCBRecSettingData は schemas.RecordSettings 互換の構造体を EDCB の RecSettingData に変換する。
//
// 移植元: ReservationsRouter.EncodeEDCBRecSettingData()
func EncodeEDCBRecSettingData(settings recordSettings) reservations.RecSettingData {
	// 録画モード (5 以降の値は予約無効)
	recMode := 1
	if settings.IsEnabled {
		switch settings.RecordingMode {
		case "AllServices":
			recMode = 0
		case "SpecifiedService":
			recMode = 1
		case "AllServicesWithoutDecoding":
			recMode = 2
		case "SpecifiedServiceWithoutDecoding":
			recMode = 3
		case "View":
			recMode = 4
		}
	} else {
		switch settings.RecordingMode {
		case "AllServices":
			recMode = 9
		case "SpecifiedService":
			recMode = 5
		case "AllServicesWithoutDecoding":
			recMode = 6
		case "SpecifiedServiceWithoutDecoding":
			recMode = 7
		case "View":
			recMode = 8
		}
	}

	// 字幕データ / データ放送の録画設定 (ビットフラグ)
	serviceMode := 0
	if settings.CaptionRecordingMode != "Default" && settings.DataBroadcastingRecordingMode != "Default" {
		serviceMode = 1
	}
	if settings.CaptionRecordingMode == "Enable" {
		serviceMode |= 0x00000010
	}
	if settings.DataBroadcastingRecordingMode == "Enable" {
		serviceMode |= 0x00000020
	}

	// 録画後に実行する bat ファイルのパス
	batFilePath := ""
	if settings.PostRecordingBatFilePath != nil {
		batFilePath = *settings.PostRecordingBatFilePath
	}

	// 保存先の録画フォルダのパスのリスト
	recFolderList := []reservations.RecFileSetInfo{}
	partialRecFolder := []reservations.RecFileSetInfo{}
	for _, folder := range settings.RecordingFolders {
		writePlugIn := "Write_Default.dll"
		recNamePlugIn := "RecName_Macro.dll"
		if folder.RecordingFileNameTemplate != nil {
			recNamePlugIn = "RecName_Macro.dll?" + *folder.RecordingFileNameTemplate
		}
		entry := reservations.RecFileSetInfo{
			RecFolder:     folder.RecordingFolderPath,
			WritePlugIn:   writePlugIn,
			RecNamePlugIn: recNamePlugIn,
		}
		if !folder.IsOnesegSeparateRecordingFolder {
			recFolderList = append(recFolderList, entry)
		} else {
			partialRecFolder = append(partialRecFolder, entry)
		}
	}

	// 録画後動作モード
	suspendMode := 0
	rebootFlag := false
	switch settings.PostRecordingMode {
	case "Default":
		suspendMode = 0
	case "Nothing":
		suspendMode = 4
	case "Standby":
		suspendMode = 1
	case "StandbyAndReboot":
		suspendMode = 1
		rebootFlag = true
	case "Suspend":
		suspendMode = 2
	case "SuspendAndReboot":
		suspendMode = 2
		rebootFlag = true
	case "Shutdown":
		suspendMode = 3
	}

	partialRecFlag := 0
	if settings.IsOnesegSeparateOutputEnabled {
		partialRecFlag = 1
	}

	tunerID := 0
	if settings.ForcedTunerID != nil {
		tunerID = *settings.ForcedTunerID
	}

	return reservations.RecSettingData{
		RecMode:          recMode,
		Priority:         settings.Priority,
		TuijyuuFlag:      settings.IsEventRelayFollowEnabled,
		ServiceMode:      serviceMode,
		PittariFlag:      settings.IsExactRecordingEnabled,
		BatFilePath:      batFilePath,
		RecFolderList:    recFolderList,
		SuspendMode:      suspendMode,
		RebootFlag:       rebootFlag,
		StartMargin:      settings.RecordingStartMargin,
		EndMargin:        settings.RecordingEndMargin,
		ContinueRec:      settings.IsSequentialRecordingInSingleFileEnabled,
		PartialRecFlag:   partialRecFlag,
		TunerID:          tunerID,
		PartialRecFolder: partialRecFolder,
	}
}

// reservationAddProgram は schemas.ReservationAddProgram 互換のリクエスト構造体。
type reservationAddProgram struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	NetworkID int     `json:"network_id"`
	ServiceID int     `json:"service_id"`
	EventID   int     `json:"event_id"`
	Title     string  `json:"title"`
	StartTime string  `json:"start_time"`
	Duration  float64 `json:"duration"`
}

// reservationAddRequest は schemas.ReservationAddRequest 互換のリクエスト構造体。
type reservationAddRequest struct {
	ProgramID      *string                `json:"program_id"`
	RecordSettings *recordSettingsRequest `json:"record_settings"`
	Program        *struct {
		ID        *string  `json:"id"`
		ChannelID *string  `json:"channel_id"`
		NetworkID *int     `json:"network_id"`
		ServiceID *int     `json:"service_id"`
		EventID   *int     `json:"event_id"`
		Title     *string  `json:"title"`
		StartTime *string  `json:"start_time"`
		Duration  *float64 `json:"duration"`
	} `json:"program"`
}

// parsedAddProgram は検証済みの補助番組情報。
type parsedAddProgram struct {
	ID        string
	ChannelID string
	NetworkID int
	ServiceID int
	EventID   int
	Title     string
	StartTime time.Time
	Duration  float64
}

// reservationUpdateRequest は schemas.ReservationUpdateRequest 互換のリクエスト構造体。
type reservationUpdateRequest struct {
	RecordSettings *recordSettingsRequest `json:"record_settings"`
}

// reservationResponse は schemas.Reservation 互換のレスポンス。
type reservationResponse struct {
	ID                         int              `json:"id"`
	Channel                    *channelResponse `json:"channel"`
	Program                    *programResponse `json:"program"`
	IsRecordingInProgress      bool             `json:"is_recording_in_progress"`
	RecordingAvailability      string           `json:"recording_availability"`
	Comment                    string           `json:"comment"`
	ScheduledRecordingFileName string           `json:"scheduled_recording_file_name"`
	EstimatedRecordingFileSize int              `json:"estimated_recording_file_size"`
	RecordSettings             recordSettings   `json:"record_settings"`
}

// reservationsResponse は schemas.Reservations 互換のレスポンス。
type reservationsResponse struct {
	Total        int                   `json:"total"`
	Reservations []reservationResponse `json:"reservations"`
}

// genreSchema は schemas.Genre (TypedDict) 互換。
type genreSchema struct {
	Major  string `json:"major"`
	Middle string `json:"middle"`
}

// programSearchConditionServiceSchema は schemas.ProgramSearchConditionService 互換。
type programSearchConditionServiceSchema struct {
	NetworkID         int `json:"network_id"`
	TransportStreamID int `json:"transport_stream_id"`
	ServiceID         int `json:"service_id"`
}

// programSearchConditionDateSchema は schemas.ProgramSearchConditionDate 互換。
type programSearchConditionDateSchema struct {
	StartDayOfWeek int `json:"start_day_of_week"`
	StartHour      int `json:"start_hour"`
	StartMinute    int `json:"start_minute"`
	EndDayOfWeek   int `json:"end_day_of_week"`
	EndHour        int `json:"end_hour"`
	EndMinute      int `json:"end_minute"`
}

// programSearchCondition は schemas.ProgramSearchCondition 互換 (リクエスト・レスポンス共通) 。
type programSearchCondition struct {
	IsEnabled                     bool                                  `json:"is_enabled"`
	Keyword                       string                                `json:"keyword"`
	ExcludeKeyword                string                                `json:"exclude_keyword"`
	Note                          string                                `json:"note"`
	IsTitleOnly                   bool                                  `json:"is_title_only"`
	IsCaseSensitive               bool                                  `json:"is_case_sensitive"`
	IsFuzzySearchEnabled          bool                                  `json:"is_fuzzy_search_enabled"`
	IsRegexSearchEnabled          bool                                  `json:"is_regex_search_enabled"`
	ServiceRanges                 []programSearchConditionServiceSchema `json:"service_ranges"`
	GenreRanges                   []genreSchema                         `json:"genre_ranges"`
	IsExcludeGenreRanges          bool                                  `json:"is_exclude_genre_ranges"`
	DateRanges                    []programSearchConditionDateSchema    `json:"date_ranges"`
	IsExcludeDateRanges           bool                                  `json:"is_exclude_date_ranges"`
	DurationRangeMin              *int                                  `json:"duration_range_min"`
	DurationRangeMax              *int                                  `json:"duration_range_max"`
	BroadcastType                 string                                `json:"broadcast_type"`
	DuplicateTitleCheckScope      string                                `json:"duplicate_title_check_scope"`
	DuplicateTitleCheckPeriodDays int                                   `json:"duplicate_title_check_period_days"`
}

// programSearchConditionRequest は schemas.ProgramSearchCondition を受けるリクエスト用の構造体。
type programSearchConditionRequest struct {
	IsEnabled                     *bool                                  `json:"is_enabled"`
	Keyword                       *string                                `json:"keyword"`
	ExcludeKeyword                *string                                `json:"exclude_keyword"`
	Note                          *string                                `json:"note"`
	IsTitleOnly                   *bool                                  `json:"is_title_only"`
	IsCaseSensitive               *bool                                  `json:"is_case_sensitive"`
	IsFuzzySearchEnabled          *bool                                  `json:"is_fuzzy_search_enabled"`
	IsRegexSearchEnabled          *bool                                  `json:"is_regex_search_enabled"`
	ServiceRanges                 *[]programSearchConditionServiceSchema `json:"service_ranges"`
	GenreRanges                   *[]genreSchema                         `json:"genre_ranges"`
	IsExcludeGenreRanges          *bool                                  `json:"is_exclude_genre_ranges"`
	DateRanges                    *[]programSearchConditionDateSchema    `json:"date_ranges"`
	IsExcludeDateRanges           *bool                                  `json:"is_exclude_date_ranges"`
	DurationRangeMin              *int                                   `json:"duration_range_min"`
	DurationRangeMax              *int                                   `json:"duration_range_max"`
	BroadcastType                 *string                                `json:"broadcast_type"`
	DuplicateTitleCheckScope      *string                                `json:"duplicate_title_check_scope"`
	DuplicateTitleCheckPeriodDays *int                                   `json:"duplicate_title_check_period_days"`
}

// validBroadcastTypes は broadcast_type の Literal 値。
var validBroadcastTypes = map[string]bool{"All": true, "FreeOnly": true, "PaidOnly": true}

// validDuplicateTitleCheckScopes は duplicate_title_check_scope の Literal 値。
var validDuplicateTitleCheckScopes = map[string]bool{
	"None": true, "SameChannelOnly": true, "AllChannels": true,
}

// toProgramSearchCondition はリクエストに Pydantic と同じ既定値を適用して検証する。
func (request programSearchConditionRequest) toProgramSearchCondition() (programSearchCondition, error) {
	condition := programSearchCondition{
		IsEnabled:                     true,
		BroadcastType:                 "All",
		DuplicateTitleCheckScope:      "None",
		DuplicateTitleCheckPeriodDays: 6,
	}
	if request.IsEnabled != nil {
		condition.IsEnabled = *request.IsEnabled
	}
	if request.Keyword != nil {
		condition.Keyword = *request.Keyword
	}
	if request.ExcludeKeyword != nil {
		condition.ExcludeKeyword = *request.ExcludeKeyword
	}
	if request.Note != nil {
		condition.Note = *request.Note
	}
	if request.IsTitleOnly != nil {
		condition.IsTitleOnly = *request.IsTitleOnly
	}
	if request.IsCaseSensitive != nil {
		condition.IsCaseSensitive = *request.IsCaseSensitive
	}
	if request.IsFuzzySearchEnabled != nil {
		condition.IsFuzzySearchEnabled = *request.IsFuzzySearchEnabled
	}
	if request.IsRegexSearchEnabled != nil {
		condition.IsRegexSearchEnabled = *request.IsRegexSearchEnabled
	}
	condition.ServiceRanges = nil
	if request.ServiceRanges != nil {
		condition.ServiceRanges = *request.ServiceRanges
	}
	if request.GenreRanges != nil {
		condition.GenreRanges = *request.GenreRanges
	}
	if request.IsExcludeGenreRanges != nil {
		condition.IsExcludeGenreRanges = *request.IsExcludeGenreRanges
	}
	if request.DateRanges != nil {
		condition.DateRanges = *request.DateRanges
		for _, date := range *request.DateRanges {
			if date.StartDayOfWeek < 0 || date.StartDayOfWeek > 6 ||
				date.EndDayOfWeek < 0 || date.EndDayOfWeek > 6 {
				return condition, fmt.Errorf("Input should be less than or equal to 6")
			}
			if date.StartHour < 0 || date.StartHour > 23 || date.EndHour < 0 || date.EndHour > 23 {
				return condition, fmt.Errorf("Input should be less than or equal to 23")
			}
			if date.StartMinute < 0 || date.StartMinute > 59 || date.EndMinute < 0 || date.EndMinute > 59 {
				return condition, fmt.Errorf("Input should be less than or equal to 59")
			}
		}
	}
	if request.IsExcludeDateRanges != nil {
		condition.IsExcludeDateRanges = *request.IsExcludeDateRanges
	}
	if request.DurationRangeMin != nil {
		if *request.DurationRangeMin < 0 {
			return condition, fmt.Errorf("Input should be greater than or equal to 0")
		}
		condition.DurationRangeMin = request.DurationRangeMin
	}
	if request.DurationRangeMax != nil {
		if *request.DurationRangeMax < 0 {
			return condition, fmt.Errorf("Input should be greater than or equal to 0")
		}
		condition.DurationRangeMax = request.DurationRangeMax
	}
	if request.BroadcastType != nil {
		if !validBroadcastTypes[*request.BroadcastType] {
			return condition, fmt.Errorf("Input should be 'All', 'FreeOnly' or 'PaidOnly'")
		}
		condition.BroadcastType = *request.BroadcastType
	}
	if request.DuplicateTitleCheckScope != nil {
		if !validDuplicateTitleCheckScopes[*request.DuplicateTitleCheckScope] {
			return condition, fmt.Errorf("Input should be 'None', 'SameChannelOnly' or 'AllChannels'")
		}
		condition.DuplicateTitleCheckScope = *request.DuplicateTitleCheckScope
	}
	if request.DuplicateTitleCheckPeriodDays != nil {
		if *request.DuplicateTitleCheckPeriodDays < 0 {
			return condition, fmt.Errorf("Input should be greater than or equal to 0")
		}
		condition.DuplicateTitleCheckPeriodDays = *request.DuplicateTitleCheckPeriodDays
	}
	return condition, nil
}

// reservationConditionAddRequest は schemas.ReservationConditionAddRequest / UpdateRequest 互換。
type reservationConditionAddRequest struct {
	ProgramSearchCondition *programSearchConditionRequest `json:"program_search_condition"`
	RecordSettings         *recordSettingsRequest         `json:"record_settings"`
}

// reservationConditionResponse は schemas.ReservationCondition 互換のレスポンス。
type reservationConditionResponse struct {
	ID                     int                    `json:"id"`
	ReservationCount       int                    `json:"reservation_count"`
	ProgramSearchCondition programSearchCondition `json:"program_search_condition"`
	RecordSettings         recordSettings         `json:"record_settings"`
}

// reservationConditionsResponse は schemas.ReservationConditions 互換のレスポンス。
type reservationConditionsResponse struct {
	Total                 int                            `json:"total"`
	ReservationConditions []reservationConditionResponse `json:"reservation_conditions"`
}

// recordSettingsPresetResponse は schemas.RecordSettingsPreset 互換のレスポンス。
type recordSettingsPresetResponse struct {
	ID             int            `json:"id"`
	Name           string         `json:"name"`
	RecordSettings recordSettings `json:"record_settings"`
}

// recordSettingsPresetsResponse は schemas.RecordSettingsPresets 互換のレスポンス。
type recordSettingsPresetsResponse struct {
	GlobalDefaults recordSettingsGlobalDefaults   `json:"global_defaults"`
	Presets        []recordSettingsPresetResponse `json:"presets"`
}

// recordSettingsGlobalDefaults は schemas.RecordSettingsGlobalDefaults 互換のレスポンス。
type recordSettingsGlobalDefaults struct {
	RecordingStartMargin          int    `json:"recording_start_margin"`
	RecordingEndMargin            int    `json:"recording_end_margin"`
	CaptionRecordingMode          string `json:"caption_recording_mode"`
	DataBroadcastingRecordingMode string `json:"data_broadcasting_recording_mode"`
	PostRecordingMode             string `json:"post_recording_mode"`
}

// emptyJSONObject / emptyJSONArray は捏造した番組情報の detail / genres に使う。
var (
	emptyJSONObject = json.RawMessage("{}")
	emptyJSONArray  = json.RawMessage("[]")
)

// indexOf は文字列中の指定文字の位置を返す (見つからない場合は -1) 。
func indexOf(value string, target rune) int {
	for index, r := range value {
		if r == target {
			return index
		}
	}
	return -1
}

// jstNow は現在時刻 (JST) を返す。テストで固定できるよう変数にしている。
var jstNow = func() time.Time { return time.Now().In(constants.JST) }
