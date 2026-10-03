package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
)

// channelResponse は schemas.Channel 互換のレスポンス。
// terrestrial_regions は呼び出し元に応じて設定する (設定しない場合は null) 。
// フィールドの順序は Pydantic のスキーマ定義と揃えること。
type channelResponse struct {
	ID                string   `json:"id"`
	DisplayChannelID  string   `json:"display_channel_id"`
	NetworkID         int      `json:"network_id"`
	ServiceID         int      `json:"service_id"`
	TransportStreamID *int     `json:"transport_stream_id"`
	RemoconID         int      `json:"remocon_id"`
	ChannelNumber     string   `json:"channel_number"`
	Type              string   `json:"type"`
	Name              string   `json:"name"`
	TerrestrialRegion []string `json:"terrestrial_regions"`
	JikkyoForce       *int     `json:"jikkyo_force"`
	IsSubchannel      bool     `json:"is_subchannel"`
	IsRadiochannel    bool     `json:"is_radiochannel"`
	IsWatchable       bool     `json:"is_watchable"`
}

// recordedVideoResponse は schemas.RecordedVideo 互換のレスポンス。
type recordedVideoResponse struct {
	ID                         int64           `json:"id"`
	Status                     string          `json:"status"`
	FilePath                   string          `json:"file_path"`
	FileHash                   string          `json:"file_hash"`
	FileSize                   int64           `json:"file_size"`
	FileCreatedAt              string          `json:"file_created_at"`
	FileModifiedAt             string          `json:"file_modified_at"`
	RecordingStartTime         *string         `json:"recording_start_time"`
	RecordingEndTime           *string         `json:"recording_end_time"`
	Duration                   pydanticFloat64 `json:"duration"`
	ContainerFormat            string          `json:"container_format"`
	VideoCodec                 string          `json:"video_codec"`
	VideoCodecProfile          string          `json:"video_codec_profile"`
	VideoScanType              string          `json:"video_scan_type"`
	VideoFrameRate             pydanticFloat64 `json:"video_frame_rate"`
	VideoResolutionWidth       int             `json:"video_resolution_width"`
	VideoResolutionHeight      int             `json:"video_resolution_height"`
	HasVideoStreamChanges      bool            `json:"has_video_stream_changes"`
	PrimaryAudioCodec          string          `json:"primary_audio_codec"`
	PrimaryAudioChannel        string          `json:"primary_audio_channel"`
	PrimaryAudioSamplingRate   int             `json:"primary_audio_sampling_rate"`
	SecondaryAudioCodec        *string         `json:"secondary_audio_codec"`
	SecondaryAudioChannel      *string         `json:"secondary_audio_channel"`
	SecondaryAudioSamplingRate *int            `json:"secondary_audio_sampling_rate"`
	CMSections                 json.RawMessage `json:"cm_sections"`
	ThumbnailInfo              json.RawMessage `json:"thumbnail_info"`
	CreatedAt                  string          `json:"created_at"`
	UpdatedAt                  string          `json:"updated_at"`
}

// recordedProgramResponse は schemas.RecordedProgram 互換のレスポンス。
type recordedProgramResponse struct {
	ID                      int64                  `json:"id"`
	RecordedVideo           *recordedVideoResponse `json:"recorded_video"`
	RecordingStartMargin    pydanticFloat64        `json:"recording_start_margin"`
	RecordingEndMargin      pydanticFloat64        `json:"recording_end_margin"`
	IsPartiallyRecorded     bool                   `json:"is_partially_recorded"`
	Channel                 *channelResponse       `json:"channel"`
	NetworkID               *int                   `json:"network_id"`
	ServiceID               *int                   `json:"service_id"`
	EventID                 *int                   `json:"event_id"`
	SeriesID                *int64                 `json:"series_id"`
	SeriesBroadcastPeriodID *int64                 `json:"series_broadcast_period_id"`
	Title                   string                 `json:"title"`
	SeriesTitle             *string                `json:"series_title"`
	EpisodeNumber           *string                `json:"episode_number"`
	Subtitle                *string                `json:"subtitle"`
	Description             string                 `json:"description"`
	Detail                  json.RawMessage        `json:"detail"`
	StartTime               string                 `json:"start_time"`
	EndTime                 string                 `json:"end_time"`
	Duration                pydanticFloat64        `json:"duration"`
	IsFree                  bool                   `json:"is_free"`
	Genres                  json.RawMessage        `json:"genres"`
	PrimaryAudioType        string                 `json:"primary_audio_type"`
	PrimaryAudioLanguage    string                 `json:"primary_audio_language"`
	SecondaryAudioType      *string                `json:"secondary_audio_type"`
	SecondaryAudioLanguage  *string                `json:"secondary_audio_language"`
	CreatedAt               string                 `json:"created_at"`
	UpdatedAt               string                 `json:"updated_at"`
}

// buildChannelResponse は Channel から schemas.Channel 互換のレスポンスを構築する。
func buildChannelResponse(channel *database.Channel) *channelResponse {
	if channel == nil {
		return nil
	}
	return &channelResponse{
		ID:                channel.ID,
		DisplayChannelID:  channel.DisplayChannelID,
		NetworkID:         channel.NetworkID,
		ServiceID:         channel.ServiceID,
		TransportStreamID: channel.TransportStreamID,
		RemoconID:         channel.RemoconID,
		ChannelNumber:     channel.ChannelNumber,
		Type:              channel.Type,
		Name:              channel.Name,
		TerrestrialRegion: nil,
		JikkyoForce:       channel.JikkyoForce,
		IsSubchannel:      channel.IsSubchannel,
		IsRadiochannel:    channel.IsRadiochannel,
		IsWatchable:       channel.IsWatchable,
	}
}

// buildRecordedVideoResponse は RecordedVideo から schemas.RecordedVideo 互換のレスポンスを構築する。
func buildRecordedVideoResponse(video *database.RecordedVideo) (*recordedVideoResponse, error) {
	if video == nil {
		return nil, nil
	}
	// cm_sections / thumbnail_info は SQLite に JSON 文字列として保存されている (NULL は未解析)
	cmSections, err := jsonOrNull(video.CMSectionsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON in recorded_videos.cm_sections (%d): %w", video.ID, err)
	}
	thumbnailInfo, err := jsonOrNull(video.ThumbnailInfoJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON in recorded_videos.thumbnail_info (%d): %w", video.ID, err)
	}
	return &recordedVideoResponse{
		ID:                         video.ID,
		Status:                     video.Status,
		FilePath:                   video.FilePath,
		FileHash:                   video.FileHash,
		FileSize:                   video.FileSize,
		FileCreatedAt:              database.FormatJSONTime(video.FileCreatedAt),
		FileModifiedAt:             database.FormatJSONTime(video.FileModifiedAt),
		RecordingStartTime:         formatOptionalJSONTime(video.RecordingStartTime),
		RecordingEndTime:           formatOptionalJSONTime(video.RecordingEndTime),
		Duration:                   pydanticFloat64(video.Duration),
		ContainerFormat:            video.ContainerFormat,
		VideoCodec:                 video.VideoCodec,
		VideoCodecProfile:          video.VideoCodecProfile,
		VideoScanType:              video.VideoScanType,
		VideoFrameRate:             pydanticFloat64(video.VideoFrameRate),
		VideoResolutionWidth:       video.VideoResolutionWidth,
		VideoResolutionHeight:      video.VideoResolutionHeight,
		HasVideoStreamChanges:      video.HasVideoStreamChanges,
		PrimaryAudioCodec:          video.PrimaryAudioCodec,
		PrimaryAudioChannel:        video.PrimaryAudioChannel,
		PrimaryAudioSamplingRate:   video.PrimaryAudioSamplingRate,
		SecondaryAudioCodec:        video.SecondaryAudioCodec,
		SecondaryAudioChannel:      video.SecondaryAudioChannel,
		SecondaryAudioSamplingRate: video.SecondaryAudioSamplingRate,
		CMSections:                 cmSections,
		ThumbnailInfo:              thumbnailInfo,
		CreatedAt:                  database.FormatJSONTime(video.CreatedAt),
		UpdatedAt:                  database.FormatJSONTime(video.UpdatedAt),
	}, nil
}

// buildRecordedProgramResponse は RecordedProgram から schemas.RecordedProgram 互換のレスポンスを構築する。
func buildRecordedProgramResponse(
	program *database.RecordedProgram,
	video *database.RecordedVideo,
	channel *database.Channel,
) (*recordedProgramResponse, error) {
	if program == nil {
		return nil, nil
	}
	videoResponse, err := buildRecordedVideoResponse(video)
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(program.Detail)) {
		return nil, fmt.Errorf("invalid JSON in recorded_programs.detail (%d)", program.ID)
	}
	if !json.Valid([]byte(program.Genres)) {
		return nil, fmt.Errorf("invalid JSON in recorded_programs.genres (%d)", program.ID)
	}
	return &recordedProgramResponse{
		ID:                      program.ID,
		RecordedVideo:           videoResponse,
		RecordingStartMargin:    pydanticFloat64(program.RecordingStartMargin),
		RecordingEndMargin:      pydanticFloat64(program.RecordingEndMargin),
		IsPartiallyRecorded:     program.IsPartiallyRecorded,
		Channel:                 buildChannelResponse(channel),
		NetworkID:               program.NetworkID,
		ServiceID:               program.ServiceID,
		EventID:                 program.EventID,
		SeriesID:                program.SeriesID,
		SeriesBroadcastPeriodID: program.SeriesBroadcastPeriodID,
		Title:                   program.Title,
		SeriesTitle:             program.SeriesTitle,
		EpisodeNumber:           program.EpisodeNumber,
		Subtitle:                program.Subtitle,
		Description:             program.Description,
		Detail:                  json.RawMessage(program.Detail),
		StartTime:               database.FormatJSONTime(program.StartTime),
		EndTime:                 database.FormatJSONTime(program.EndTime),
		Duration:                pydanticFloat64(program.Duration),
		IsFree:                  program.IsFree,
		Genres:                  json.RawMessage(program.Genres),
		PrimaryAudioType:        program.PrimaryAudioType,
		PrimaryAudioLanguage:    program.PrimaryAudioLanguage,
		SecondaryAudioType:      program.SecondaryAudioType,
		SecondaryAudioLanguage:  program.SecondaryAudioLanguage,
		CreatedAt:               database.FormatJSONTime(program.CreatedAt),
		UpdatedAt:               database.FormatJSONTime(program.UpdatedAt),
	}, nil
}

// jsonOrNull は JSON 文字列を json.RawMessage に変換する。
// 空文字列または nil の場合は null を返す。
func jsonOrNull(value *string) (json.RawMessage, error) {
	if value == nil || *value == "" {
		return json.RawMessage("null"), nil
	}
	if !json.Valid([]byte(*value)) {
		return nil, fmt.Errorf("invalid JSON: %q", *value)
	}
	return json.RawMessage(*value), nil
}

// formatOptionalJSONTime は *time.Time を Pydantic v2 互換の ISO8601 文字列に変換する。
func formatOptionalJSONTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := database.FormatJSONTime(*value)
	return &formatted
}

// dereferenceChannelResponse は *channelResponse を値型に変換する (番組表のレスポンス構築用) 。
func dereferenceChannelResponse(response *channelResponse) channelResponse {
	if response == nil {
		return channelResponse{}
	}
	return *response
}
