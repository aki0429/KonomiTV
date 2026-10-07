// Package metadata は録画ファイルのメタデータ解析 (動画情報・CM 区間・サムネイル) と
// 録画フォルダのスキャン・DB 同期を扱う。
//
// 移植元: server/app/metadata/ (MetadataAnalyzer.py / CMSectionsDetector.py / ThumbnailGenerator.py /
// RecordedScanTask.py) と server/app/routers/VideosRouter.py・MaintenanceRouter.py の該当 API。
//
// 外部プロセス (FFprobe / FFmpeg など) の起動は ProcessRunner 経由で行うため、
// テストでは実プロセスを起動せずフェイクに差し替えられる。
package metadata

import "time"

// Genre は番組ジャンル (schemas.Genre) 。
type Genre struct {
	Major  string
	Middle string
}

// DetailEntry は番組詳細 (dict[str, str]) の 1 エントリ。
// Python の dict は挿入順を保持し、DB の JSON 文字列にも反映されるため、順序付きスライスで保持する。
type DetailEntry struct {
	Key   string
	Value string
}

// Channel は録画メタデータから得たチャンネル情報 (schemas.Channel) 。
type Channel struct {
	ID                string
	DisplayChannelID  string
	NetworkID         int
	ServiceID         int
	TransportStreamID *int
	RemoconID         int
	ChannelNumber     string
	Type              string
	Name              string
	JikkyoForce       *int
	IsSubchannel      bool
	IsRadiochannel    bool
	IsWatchable       bool
}

// RecordedVideo は録画ファイル情報 (schemas.RecordedVideo) 。
type RecordedVideo struct {
	Status                     string
	FilePath                   string
	FileHash                   string
	FileSize                   int64
	FileCreatedAt              time.Time
	FileModifiedAt             time.Time
	RecordingStartTime         *time.Time
	RecordingEndTime           *time.Time
	Duration                   float64
	ContainerFormat            string
	VideoCodec                 string
	VideoCodecProfile          string
	VideoScanType              string
	VideoFrameRate             float64
	VideoResolutionWidth       int
	VideoResolutionHeight      int
	HasVideoStreamChanges      bool
	PrimaryAudioCodec          string
	PrimaryAudioChannel        string
	PrimaryAudioSamplingRate   int
	SecondaryAudioCodec        *string
	SecondaryAudioChannel      *string
	SecondaryAudioSamplingRate *int
}

// RecordedProgram は録画番組情報 (schemas.RecordedProgram) 。
type RecordedProgram struct {
	ID                      int64
	Video                   RecordedVideo
	RecordingStartMargin    float64
	RecordingEndMargin      float64
	IsPartiallyRecorded     bool
	Channel                 *Channel
	NetworkID               *int
	ServiceID               *int
	EventID                 *int
	SeriesID                *int64
	SeriesBroadcastPeriodID *int64
	Title                   string
	SeriesTitle             *string
	EpisodeNumber           *string
	Subtitle                *string
	Description             string
	Detail                  []DetailEntry
	StartTime               time.Time
	EndTime                 time.Time
	Duration                float64
	IsFree                  bool
	Genres                  []Genre
	PrimaryAudioType        string
	PrimaryAudioLanguage    string
	SecondaryAudioType      *string
	SecondaryAudioLanguage  *string
}

// CMSection は CM 区間 (schemas.CMSection) 。
type CMSection struct {
	StartTime float64
	EndTime   float64
}

// 録画番組メタデータ解析のデフォルト値 (schemas.RecordedProgram のデフォルト値と同じ) 。
const (
	DefaultDescription          = "番組概要を取得できませんでした。"
	DefaultPrimaryAudioType     = "2/0モード(ステレオ)"
	DefaultPrimaryAudioLanguage = "日本語"
)
