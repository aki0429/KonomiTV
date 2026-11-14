package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
	"github.com/aki0429/KonomiTV/server-go/internal/exif"
)

type catalogMetadata struct {
	CapturedAt, StartTime                time.Time
	EndTime                              *time.Time
	Duration                             *float64
	PlaybackPosition                     float64
	NetworkID, ServiceID, EventID        int
	Title, Description                   string
	CaptionText                          *string
	CaptionComposited, CommentComposited bool
}

type catalogImageInfo struct {
	Hash           [32]byte
	Width, Height  int
	CapturedAt     time.Time
	MetadataStatus string
	Metadata       *catalogMetadata
}

var captureFilenameTime = regexp.MustCompile(`(?i)^Capture_(\d{8}-\d{6})(?:[-_].*)?\.(?:jpe?g|png)$`)

// captureJSONTime は EXIF に入る日時を本家の Pydantic と同じく JST へ揃える。
func captureJSONTime(raw json.RawMessage) (time.Time, error) {
	var text string
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "true" || string(raw) == "false" {
		return time.Time{}, fmt.Errorf("invalid capture datetime")
	}
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return time.Time{}, err
		}
	} else {
		text = string(raw)
	}
	value, err := parsePydanticDatetime(text)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid capture datetime: %s", err.Msg)
	}
	return value.In(constants.JST), nil
}

func captureJSONFloat(raw json.RawMessage) (float64, error) {
	var text string
	if string(raw) == "true" {
		return 1, nil
	}
	if string(raw) == "false" {
		return 0, nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
	} else {
		text = string(raw)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid capture number")
	}
	return value, nil
}

// captureBroadcastTime は Python timedelta のマイクロ秒丸めと datetime の範囲を保つ。
func captureBroadcastTime(start time.Time, position float64) (time.Time, error) {
	if math.Abs(position) > 1e12 {
		return time.Time{}, fmt.Errorf("capture position out of range")
	}
	seconds, fraction := math.Modf(position)
	microseconds := int64(math.RoundToEven(fraction * 1e6))
	value := time.Unix(start.Unix()+int64(seconds), int64(start.Nanosecond())+microseconds*1000).In(constants.JST)
	if value.Year() < 1 || value.Year() > 9999 {
		return time.Time{}, fmt.Errorf("capture position out of range")
	}
	return value, nil
}

func parseCatalogMetadata(comment string) (*catalogMetadata, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(comment), &fields); err != nil {
		return nil, err
	}
	var result catalogMetadata
	var err error
	captured := fields["captured_at"]
	if _, found := fields["captured_at"]; !found {
		captured = fields["capture_time"]
	}
	if result.CapturedAt, err = captureJSONTime(captured); err != nil {
		return nil, err
	}
	if result.StartTime, err = captureJSONTime(fields["start_time"]); err != nil {
		return nil, err
	}
	for key, target := range map[string]*int{"network_id": &result.NetworkID, "service_id": &result.ServiceID, "event_id": &result.EventID} {
		value, err := searchJSONInteger(fields[key])
		if err != nil || !value.IsInt64() || value.Int64() < -1 || value.Int64() > 65535 {
			return nil, fmt.Errorf("invalid capture service ID")
		}
		*target = int(value.Int64())
	}
	for key, target := range map[string]*string{"title": &result.Title, "description": &result.Description} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" || json.Unmarshal(fields[key], target) != nil {
			return nil, fmt.Errorf("invalid capture text")
		}
	}
	end, found := fields["end_time"]
	if !found {
		return nil, fmt.Errorf("missing capture end time")
	}
	if string(end) != "null" {
		value, err := captureJSONTime(end)
		if err != nil {
			return nil, err
		}
		result.EndTime = &value
	}
	duration, found := fields["duration"]
	if !found {
		return nil, fmt.Errorf("missing capture duration")
	}
	if string(duration) != "null" {
		value, err := captureJSONFloat(duration)
		if err != nil {
			return nil, err
		}
		result.Duration = &value
	} else {
		result.EndTime = nil
	}
	position, found := fields["captured_playback_position"]
	if !found && fields["capture_time"] != nil {
		// 2022 年までのライブ画像は撮影日時と番組開始日時の差を使う。
		var oldText, startText string
		if json.Unmarshal(fields["capture_time"], &oldText) != nil || json.Unmarshal(fields["start_time"], &startText) != nil {
			return nil, fmt.Errorf("invalid legacy capture time")
		}
		hasZone := func(value string) bool { return len(value) > 10 && strings.ContainsAny(value[10:], "+-Zz") }
		if hasZone(oldText) != hasZone(startText) {
			return nil, fmt.Errorf("mixed legacy capture time zones")
		}
		oldTime, err := captureJSONTime(fields["capture_time"])
		if err != nil {
			return nil, err
		}
		result.PlaybackPosition = float64(oldTime.Unix()-result.StartTime.Unix()) + float64(oldTime.Nanosecond()-result.StartTime.Nanosecond())/1e9
	} else if result.PlaybackPosition, err = captureJSONFloat(position); err != nil {
		return nil, err
	}
	if _, err := captureBroadcastTime(result.StartTime, result.PlaybackPosition); err != nil {
		return nil, err
	}
	if raw := fields["caption_text"]; raw != nil && string(raw) != "null" {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, fmt.Errorf("invalid capture caption")
		}
		result.CaptionText = &value
	}
	for key, target := range map[string]*bool{"is_caption_composited": &result.CaptionComposited, "is_comment_composited": &result.CommentComposited} {
		if raw := fields[key]; raw != nil {
			value, err := searchJSONBool(raw)
			if err != nil {
				return nil, err
			}
			*target = value
		}
	}
	return &result, nil
}

// readCatalogImage は完全な画像を検証し、壊れた EXIF とメタデータなしを区別する。
func readCatalogImage(data []byte, filename string, modified time.Time) (*catalogImageInfo, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return nil, fmt.Errorf("invalid capture image")
	}
	if config.Height <= 0 || config.Width <= 0 || config.Width > 178956970/config.Height {
		return nil, fmt.Errorf("capture image exceeds decoded pixel limit")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("invalid capture image: %w", err)
	}
	result := &catalogImageInfo{Hash: sha256.Sum256(data), Width: config.Width, Height: config.Height, CapturedAt: modified.In(constants.JST), MetadataStatus: "NoMetadata"}
	exifInfo := exif.ParseCapture(data)
	if exifInfo.Invalid {
		result.MetadataStatus = "Invalid"
	}
	if exifInfo.XPComment != nil {
		meta, err := parseCatalogMetadata(*exifInfo.XPComment)
		if err != nil {
			result.MetadataStatus = "Invalid"
		} else {
			result.Metadata = meta
			result.MetadataStatus = "Parsed"
			result.CapturedAt = meta.CapturedAt
		}
	}
	if result.Metadata == nil {
		if match := captureFilenameTime.FindStringSubmatch(filename); match != nil {
			if value, err := time.ParseInLocation("20060102-150405", match[1], constants.JST); err == nil {
				difference := math.Abs(value.Sub(result.CapturedAt).Seconds())
				if difference >= 600 && math.Abs(difference-12*3600) >= 600 {
					result.CapturedAt = value
				}
			}
		}
		if value, err := time.ParseInLocation("2006:01:02 15:04:05", exifInfo.DateTimeOriginal, constants.JST); err == nil {
			result.CapturedAt = value
		}
	}
	return result, nil
}
