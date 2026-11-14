package metadata

import (
	"context"
	"io"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

type tsinfoArchiveEventState struct {
	count                                        int
	done, title, detail, genres, audio, language bool
}

// tsinfoArchiveEvent は本家の初回EIT探索を再現する。必要な属性が揃うまでは、
// 同じp/f区分の次のイベントへ変わった場合もIDや取得済み属性を更新する。
func tsinfoArchiveEvent(section psi.Section, video *RecordedVideo, channel *Channel, states *[2]tsinfoEventState, trackers *[2]tsinfoArchiveEventState) {
	if len(section.Raw) == 0 || section.Raw[0] != 0x4e {
		return
	}
	eit, err := psi.DecodeEIT(section)
	if err != nil || !eit.Header.Current {
		return
	}
	for index := range states {
		tracker, state := &trackers[index], &states[index]
		if tracker.done {
			continue
		}
		if int(eit.Header.SectionNumber) == index && int(eit.ServiceID) == channel.ServiceID {
			if int(eit.TransportStreamID) != *channel.TransportStreamID || int(eit.OriginalNetworkID) != channel.NetworkID {
				continue
			}
			for _, event := range eit.Events {
				if state.program != nil {
					id := int(event.EventID)
					state.program.EventID = &id
				}
				state.merge(video, channel, event)
				for _, descriptor := range event.Descriptors {
					switch descriptor.Tag {
					case 0x4d:
						_, _, ok := tsinfoShortText(descriptor.Data)
						tracker.title = tracker.title || ok
					case 0x4e:
						fragment, ok := tsinfoExtendedDecode(descriptor.Data)
						// ariblib は項目が一つもない記述子に detail 属性を作らない。
						tracker.detail = tracker.detail || (ok && len(fragment.items) > 0)
					case 0x54:
						tracker.genres = tracker.genres || len(descriptor.Data)%2 == 0
					case 0xc4:
						_, _, main, ok := tsinfoAudio(descriptor.Data)
						tracker.audio = tracker.audio || (ok && main)
						tracker.language = tracker.language || (ok && main)
					}
				}
				if tracker.title && tracker.detail && tracker.genres && tracker.audio && tracker.language && state.startValid && (state.durationValid || tracker.count >= 100) {
					tracker.done = true
					break
				}
			}
			// 本家の for/else は未完了の一致セクションをカウントせず次へ進む。
			if !tracker.done {
				continue
			}
		}
		if !tracker.done {
			tracker.count++
			if tracker.count > 100 {
				tracker.detail = true
				tracker.genres = true
			}
			if tracker.count > 2000 {
				tracker.done = true
			}
		}
	}
}

// tsinfoArchiveEnd は番組長未定の現在番組だけを対象に、最後の同一IDと次番組を探す。
// 初回探索の終了後に全書庫を再走査する点を、本家と同じにする。
func tsinfoArchiveEnd(ctx context.Context, reader io.ReadSeeker, end int64, video *RecordedVideo, channel *Channel, state *tsinfoEventState) error {
	if state.program == nil || !state.startValid || state.durationValid {
		return nil
	}
	program := state.program
	var lastStart, nextStart *time.Time
	var lastDuration *time.Duration
	_, err := psi.ScanWindow(ctx, reader, 0, psi.ScanOptions{MaxBytes: end, PIDs: []uint16{0x12, 0x26, 0x27}}, func(section psi.Section) error {
		if len(section.Raw) == 0 || section.Raw[0] != 0x4e {
			return nil
		}
		eit, err := psi.DecodeEIT(section)
		if err != nil || eit.Header.SectionNumber != 0 || int(eit.ServiceID) != channel.ServiceID || int(eit.TransportStreamID) != *channel.TransportStreamID || int(eit.OriginalNetworkID) != channel.NetworkID {
			return nil
		}
		for _, event := range eit.Events {
			if int(event.EventID) == *program.EventID {
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
	// 後続の共通フォールバックで確定した末尾を上書きしない。
	state.durationValid = true
	return nil
}
