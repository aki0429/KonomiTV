package metadata

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

type tsinfoServiceSource struct {
	sdt *psi.SDT
	sit *psi.SIT
}

// tsinfoSITChannel はSITのサービス記述子と元の放送IDをSDTと共通の形式へ写す。
func tsinfoSITChannel(table *psi.SIT, sid uint16) (*psi.SDT, int) {
	for _, service := range table.Services {
		if service.ServiceID != sid {
			continue
		}
		var ids, name []byte
		for _, descriptor := range service.Descriptors {
			if descriptor.Tag == 0x85 && ids == nil && len(descriptor.Data) >= 7 {
				ids = descriptor.Data
			}
			if descriptor.Tag == 0x48 && name == nil && len(descriptor.Data) >= 3 {
				data := descriptor.Data
				provider := int(data[1])
				if provider+3 <= len(data) {
					length := int(data[provider+2])
					if provider+3+length <= len(data) {
						name = bytes.Clone(data[provider+3 : provider+3+length])
					}
				}
			}
		}
		if ids == nil || name == nil {
			continue
		}
		remocon := 0
		for _, descriptor := range table.TransmissionDescriptors {
			if descriptor.Tag == 0xcd && len(descriptor.Data) >= 2 {
				remocon = int(descriptor.Data[0])
				break
			}
		}
		return &psi.SDT{OriginalNetworkID: uint16(ids[0])<<8 | uint16(ids[1]), TransportStreamID: uint16(ids[2])<<8 | uint16(ids[3]), Services: []psi.Service{{ServiceID: sid, ServiceName: name}}}, remocon
	}
	return nil, 0
}

func tsinfoSITEvent(service psi.SITService) (psi.Event, bool) {
	var event psi.Event
	var timeSeen bool
	event.Descriptors = service.Descriptors
	found := false
	for _, descriptor := range service.Descriptors {
		switch descriptor.Tag {
		case 0x85:
			if !found && len(descriptor.Data) >= 7 {
				event.EventID = uint16(descriptor.Data[4])<<8 | uint16(descriptor.Data[5])
				found = true
			}
		case 0x53:
			event.FreeCA = true
		case 0xc3:
			if timeSeen {
				continue
			}
			timeSeen = true
			value, err := psi.DecodePartialTSTime(descriptor.Data)
			if err == nil {
				event.StartTime, event.StartTimeValid = value.Start, value.StartValid
				event.Duration, event.DurationValid = value.Duration, value.DurationValid
			}
		}
	}
	return event, found
}

func tsinfoServiceMatches(channel *Channel, sid uint16) bool {
	return int(sid) == channel.ServiceID || (channel.Type == "BS" && int(sid) == channel.ServiceID & ^0x180)
}

// tsinfoSITEnd は本家と同じく末尾から50000パケットずつ戻り、
// 対象番組を含む最新の区間で最後の同一IDと次番組の開始時刻を調べる。
func tsinfoSITEnd(ctx context.Context, source io.ReadSeeker, end, lower int64, video *RecordedVideo, channel *Channel, state *tsinfoEventState) error {
	if state.program == nil || !state.startValid || state.durationValid {
		return nil
	}
	program := state.program
	var lastStart, nextStart *time.Time
	var lastDuration *time.Duration
	const window = 188 * 50000
	offset := closestMultiple(max(end-window, lower), 188)
	found := false
	for !found {
		data, err := tsinfoLoadWindow(ctx, source, offset, min(window, end-offset))
		if err != nil {
			return err
		}
		_, err = psi.Scan(ctx, bytes.NewReader(data.data), psi.ScanOptions{MaxBytes: int64(len(data.data)), PIDs: []uint16{0x1f}}, func(section psi.Section) error {
			table, err := psi.DecodeSIT(section)
			if err != nil {
				return nil
			}
			for _, service := range table.Services {
				if !tsinfoServiceMatches(channel, service.ServiceID) {
					continue
				}
				event, ok := tsinfoSITEvent(service)
				if !ok {
					continue
				}
				if int(event.EventID) == *program.EventID {
					found = true
					lastStart, lastDuration = nil, nil
					if event.StartTimeValid {
						clock := event.StartTime
						lastStart = &clock
					}
					if event.DurationValid {
						duration := event.Duration
						lastDuration = &duration
					}
				} else if event.StartTimeValid && event.StartTime.After(program.StartTime) && (nextStart == nil || event.StartTime.Before(*nextStart)) {
					clock := event.StartTime
					nextStart = &clock
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if offset <= lower {
			break
		}
		offset = closestMultiple(max(offset-window, lower), 188)
	}
	if lastStart != nil && lastDuration != nil {
		program.StartTime = *lastStart
		program.EndTime = lastStart.Add(*lastDuration)
	} else if nextStart != nil {
		program.EndTime = *nextStart
	} else if video.RecordingEndTime != nil {
		program.EndTime = *video.RecordingEndTime
	} else {
		program.EndTime = program.StartTime.Add(time.Duration(video.Duration * float64(time.Second)))
	}
	program.Duration = program.EndTime.Sub(program.StartTime).Seconds()
	state.durationValid = true
	return nil
}
