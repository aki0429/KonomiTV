package reservations

import "time"

// このファイルは server/app/utils/edcb/__init__.py の TypedDict 群に対応する Go の型定義。
//
// Python 版は辞書で受け渡ししているが、Go では構造体で表現する。
// Python 側で「キーが存在しない」ことが意味を持つ項目 (rec_setting.start_margin など) は
// ポインタで表現し、nil を「キーなし」として扱う。

// RecFileSetInfo は録画フォルダ情報 (Python 版 RecFileSetInfo) 。
type RecFileSetInfo struct {
	RecFolder     string
	WritePlugIn   string
	RecNamePlugIn string
}

// RecSettingData は録画設定 (Python 版 RecSettingData / RecSettingDataRequired) 。
type RecSettingData struct {
	RecMode          int
	Priority         int
	TuijyuuFlag      bool
	ServiceMode      int
	PittariFlag      bool
	BatFilePath      string
	RecFolderList    []RecFileSetInfo
	SuspendMode      int
	RebootFlag       bool
	StartMargin      *int
	EndMargin        *int
	ContinueRec      bool
	PartialRecFlag   int
	TunerID          int
	PartialRecFolder []RecFileSetInfo
}

// ReserveData は予約情報 (Python 版 ReserveData / ReserveDataRequired) 。
type ReserveData struct {
	Title           string
	StartTime       time.Time
	DurationSecond  int
	StationName     string
	Onid            int
	Tsid            int
	Sid             int
	Eid             int
	Comment         string
	ReserveID       int
	OverlapMode     int
	StartTimeEPG    time.Time
	RecSetting      RecSettingData
	RecFileNameList []string
}

// ContentData はジャンルの個別データ (Python 版 ContentData) 。
type ContentData struct {
	ContentNibble int
	UserNibble    int
}

// SearchDateInfo は番組検索の対象期間 (Python 版 SearchDateInfoRequired) 。
type SearchDateInfo struct {
	StartDayOfWeek int
	StartHour      int
	StartMin       int
	EndDayOfWeek   int
	EndHour        int
	EndMin         int
}

// SearchKeyInfo は番組検索条件 (Python 版 SearchKeyInfo / SearchKeyInfoRequired) 。
type SearchKeyInfo struct {
	AndKey          string
	NotKey          string
	KeyDisabled     bool
	CaseSensitive   bool
	RegExpFlag      bool
	TitleOnlyFlag   bool
	ContentList     []ContentData
	DateList        []SearchDateInfo
	ServiceList     []int64
	VideoList       []int
	AudioList       []int
	AimaiFlag       bool
	NotContetFlag   bool
	NotDateFlag     bool
	FreeCaFlag      int
	ChkRecEnd       bool
	ChkRecDay       int
	ChkRecNoService bool
	ChkDurationMin  int
	ChkDurationMax  int
}

// AutoAddData は自動予約登録情報 (Python 版 AutoAddData / AutoAddDataRequired) 。
type AutoAddData struct {
	DataID     int
	SearchInfo SearchKeyInfo
	RecSetting RecSettingData
	AddCount   int
}

// FileData は転送ファイルデータ (Python 版 FileData) 。
type FileData struct {
	Name string
	Data []byte
}

// NWPlayTimeShiftInfo は CMD_EPG_SRV_NWPLAY_TF_OPEN で受け取る情報 (Python 版 NWPlayTimeShiftInfo) 。
type NWPlayTimeShiftInfo struct {
	CtrlID   int
	FilePath string
}

// ShortEventInfo はイベントの基本情報 (Python 版 ShortEventInfo) 。
type ShortEventInfo struct {
	EventName string
	TextChar  string
}

// ExtendedEventInfo はイベントの拡張情報 (Python 版 ExtendedEventInfo) 。
type ExtendedEventInfo struct {
	TextChar string
}

// ContentInfo はジャンル情報 (Python 版 ContentInfo) 。
type ContentInfo struct {
	NibbleList []ContentData
}

// ComponentInfo は映像情報 (Python 版 ComponentInfo) 。
type ComponentInfo struct {
	StreamContent int
	ComponentType int
	ComponentTag  int
	TextChar      string
}

// AudioComponentInfoData は音声情報の個別データ (Python 版 AudioComponentInfoData) 。
type AudioComponentInfoData struct {
	StreamContent      int
	ComponentType      int
	ComponentTag       int
	StreamType         int
	SimulcastGroupTag  int
	EsMultiLingualFlag int
	MainComponentFlag  int
	QualityIndicator   int
	SamplingRate       int
	TextChar           string
}

// AudioComponentInfo は音声情報 (Python 版 AudioComponentInfo) 。
type AudioComponentInfo struct {
	ComponentList []AudioComponentInfoData
}

// EventData はイベントグループの個別データ (Python 版 EventData) 。
type EventData struct {
	Onid int
	Tsid int
	Sid  int
	Eid  int
}

// EventGroupInfo はイベントグループ情報 (Python 版 EventGroupInfo) 。
type EventGroupInfo struct {
	GroupType     int
	EventDataList []EventData
}

// EventInfo はイベント情報 (Python 版 EventInfo) 。
//
// Python 版では「情報がないときキー自体が存在しない」ため、ポインタで有無を表現する。
type EventInfo struct {
	Onid           int
	Tsid           int
	Sid            int
	Eid            int
	FreeCaFlag     int
	StartTime      *time.Time
	DurationSec    *int
	ShortInfo      *ShortEventInfo
	ExtInfo        *ExtendedEventInfo
	ContentInfo    *ContentInfo
	ComponentInfo  *ComponentInfo
	AudioInfo      *AudioComponentInfo
	EventGroupInfo *EventGroupInfo
	EventRelayInfo *EventGroupInfo
}

// ServiceInfo はサービス情報 (Python 版 ServiceInfo) 。
type ServiceInfo struct {
	Onid                 int
	Tsid                 int
	Sid                  int
	ServiceType          int
	PartialReceptionFlag int
	ServiceProviderName  string
	ServiceName          string
	NetworkName          string
	TsName               string
	RemoteControlKeyID   int
}

// ServiceEventInfo はサービスとそのイベント一覧 (Python 版 ServiceEventInfo) 。
type ServiceEventInfo struct {
	ServiceInfo ServiceInfo
	EventList   []EventInfo
}

// ChSet5Item は ChSet5.txt の一行の情報 (Python 版 ChSet5Item) 。
type ChSet5Item struct {
	ServiceName string
	NetworkName string
	Onid        int
	Tsid        int
	Sid         int
	ServiceType int
	PartialFlag bool
	EpgCapFlag  bool
	SearchFlag  bool
	RemoconID   int
}
