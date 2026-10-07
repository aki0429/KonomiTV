package metadata

import (
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

type tsinfoExtendedItem struct{ head, body []byte }
type tsinfoExtendedFragment struct {
	number, last byte
	language     string
	items        []tsinfoExtendedItem
	text         []byte
}

type tsinfoEventState struct {
	program                   *RecordedProgram
	fragments                 [16]*tsinfoExtendedFragment
	language                  string
	last                      byte
	descriptionDerived        bool
	startValid, durationValid bool
}

func (s *tsinfoEventState) merge(video *RecordedVideo, channel *Channel, event psi.Event) {
	if s.program == nil {
		s.program = tsinfoBasicEvent(video, channel, event)
		s.startValid, s.durationValid = event.StartTimeValid, event.DurationValid
	}
	if s.program == nil || *s.program.EventID != int(event.EventID) {
		return
	}
	if event.StartTimeValid {
		s.program.StartTime = event.StartTime
		s.startValid = true
	}
	if event.DurationValid {
		s.program.Duration = event.Duration.Seconds()
		s.durationValid = true
	}
	if s.startValid && !s.durationValid {
		if video.RecordingEndTime != nil {
			s.program.Duration = video.RecordingEndTime.Sub(s.program.StartTime).Seconds()
		} else {
			s.program.Duration = video.Duration
		}
	}
	if s.startValid {
		s.program.EndTime = s.program.StartTime.Add(time.Duration(s.program.Duration * float64(time.Second)))
	} else {
		s.program.EndTime = time.Time{}
	}
	s.program.IsFree = !event.FreeCA
	for _, d := range event.Descriptors {
		switch d.Tag {
		case 0x4d:
			n, text, ok := tsinfoShortText(d.Data)
			if ok {
				s.program.Title, s.program.Description = n, text
				s.descriptionDerived = false
			}
		case 0x4e:
			fragment, ok := tsinfoExtendedDecode(d.Data)
			if !ok {
				continue
			}
			if s.language == "" || (fragment.language == "jpn" && s.language != "jpn") {
				s.language = fragment.language
				s.last = fragment.last
				s.fragments = [16]*tsinfoExtendedFragment{}
			}
			if fragment.language == s.language {
				if fragment.last != s.last {
					s.last = fragment.last
					s.fragments = [16]*tsinfoExtendedFragment{}
				}
				s.fragments[fragment.number] = fragment
			}
		case 0x54:
			if len(d.Data)%2 == 0 {
				s.program.Genres = tsinfoGenres(d.Data)
			}
		case 0xc4:
			kind, language, main, ok := tsinfoAudio(d.Data)
			if ok {
				if main {
					s.program.PrimaryAudioType, s.program.PrimaryAudioLanguage = kind, language
				} else {
					s.program.SecondaryAudioType, s.program.SecondaryAudioLanguage = &kind, &language
				}
			}
		}
	}
	// Only a detail-derived description follows the selected fragment set.
	// Keep explicit short text (even when equal to old detail) and the initial
	// DefaultDescription; a new empty short descriptor permits derivation again.
	if s.descriptionDerived {
		s.program.Description = ""
		s.descriptionDerived = false
	}
	s.program.Detail = s.details()
	if strings.TrimSpace(s.program.Description) == "" && len(s.program.Detail) > 0 {
		s.program.Description = s.program.Detail[0].Value
		s.descriptionDerived = true
	}
}

func tsinfoShortText(b []byte) (string, string, bool) {
	if len(b) < 5 {
		return "", "", false
	}
	n := int(b[3])
	if 4+n >= len(b) {
		return "", "", false
	}
	m := int(b[4+n])
	if 5+n+m != len(b) {
		return "", "", false
	}
	return formatString(arib.Decode(b[4 : 4+n])), formatString(arib.Decode(b[5+n:])), true
}

func tsinfoExtendedDecode(b []byte) (*tsinfoExtendedFragment, bool) {
	if len(b) < 6 || b[0]>>4 > b[0]&15 {
		return nil, false
	}
	n := int(b[4])
	if 5+n >= len(b) {
		return nil, false
	}
	textLength := int(b[5+n])
	if 6+n+textLength != len(b) {
		return nil, false
	}
	f := &tsinfoExtendedFragment{number: b[0] >> 4, last: b[0] & 15, language: string(b[1:4]), text: append([]byte(nil), b[6+n:]...)}
	items := b[5 : 5+n]
	for len(items) > 0 {
		h := int(items[0])
		if 1+h >= len(items) {
			return nil, false
		}
		t := int(items[1+h])
		if 2+h+t > len(items) {
			return nil, false
		}
		f.items = append(f.items, tsinfoExtendedItem{head: append([]byte(nil), items[1:1+h]...), body: append([]byte(nil), items[2+h:2+h+t]...)})
		items = items[2+h+t:]
	}
	return f, true
}

func (s *tsinfoEventState) details() []DetailEntry {
	// The continuation body must be joined as ARIB bytes before decoding: an
	// escape sequence, a multibyte character or designation can span fragments.
	// Do not invent adjacency across gaps or combine different fragment sets.
	for i := 0; i <= int(s.last); i++ {
		f := s.fragments[i]
		if f == nil || f.last != s.last || f.language != s.language {
			return []DetailEntry{}
		}
	}
	var items []tsinfoExtendedItem
	for _, f := range s.fragments[:int(s.last)+1] {
		for _, item := range f.items {
			if len(item.head) == 0 {
				if len(items) > 0 {
					items[len(items)-1].body = append(items[len(items)-1].body, item.body...)
				}
			} else {
				items = append(items, tsinfoExtendedItem{head: item.head, body: append([]byte(nil), item.body...)})
			}
		}
	}
	out := make([]DetailEntry, 0, len(items))
	used := make(map[string]bool)
	for _, item := range items {
		head := strings.Trim(strings.ReplaceAll(formatString(arib.Decode(item.head)), "◇", ""), " \r\n")
		if head == "" {
			head = "番組内容"
		}
		for used[head] {
			head += "\t"
		}
		used[head] = true
		out = append(out, DetailEntry{Key: head, Value: strings.TrimSpace(formatString(arib.Decode(item.body)))})
	}
	return out
}

func tsinfoGenres(raw []byte) []Genre {
	genres := []Genre{}
	for i := 0; i+1 < len(raw); i += 2 {
		major, ok := reservations.FindGenreMajor(int(raw[i] >> 4))
		if !ok {
			continue
		}
		middle := ""
		for _, m := range major.Middle {
			if m.Key == int(raw[i]&15) {
				middle = m.Name
				break
			}
		}
		if major.Key == 14 {
			if raw[i]&15 != 0 {
				continue
			}
			middle = ""
			for _, u := range reservations.UserTypes() {
				if u.Key == int(raw[i+1]) {
					middle = u.Name
					break
				}
			}
		}
		genres = append(genres, Genre{Major: strings.ReplaceAll(major.Major, "／", "・"), Middle: strings.ReplaceAll(middle, "／", "・")})
	}
	return genres
}

func tsinfoAudio(b []byte) (string, string, bool, bool) {
	if len(b) < 9 || b[0]&15 != 2 {
		return "", "", false, false
	}
	kind := ""
	switch b[1] {
	case 1:
		kind = "1/0モード(シングルモノ)"
	case 2:
		kind = "1/0+1/0モード(デュアルモノ)"
	case 3:
		kind = DefaultPrimaryAudioType
	case 4:
		kind = "2/1モード"
	case 5:
		kind = "3/0モード"
	case 6:
		kind = "2/2モード"
	case 7:
		kind = "3/1モード"
	case 8:
		kind = "3/2モード"
	case 9:
		kind = "3/2+LFEモード(3/2.1モード)"
	case 0x40:
		kind = "視覚障害者用音声解説"
	case 0x41:
		kind = "聴覚障害者用音声"
	default:
		return "", "", false, false
	}
	multilingual := b[5]&0x80 != 0
	if multilingual && len(b) < 12 {
		return "", "", false, false
	}
	language := tsinfoLanguage(string(b[6:9]))
	if b[1] == 2 {
		if multilingual {
			language += "+" + tsinfoLanguage(string(b[9:12]))
		} else {
			language += "+副音声"
		}
	}
	return kind, language, b[5]&0x40 != 0, true
}

func tsinfoLanguage(code string) string {
	switch code {
	case "jpn":
		return "日本語"
	case "eng":
		return "英語"
	case "deu":
		return "ドイツ語"
	case "fra":
		return "フランス語"
	case "ita":
		return "イタリア語"
	case "rus":
		return "ロシア語"
	case "zho":
		return "中国語"
	case "kor":
		return "韓国語"
	case "spa":
		return "スペイン語"
	default:
		return "その他の言語"
	}
}
