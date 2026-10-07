package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

const tsinfoWindowBytes int64 = 32 << 20

func tsinfoChannelID(nid, sid int) string { return fmt.Sprintf("NID%d-SID%03d", nid, sid) }

// TSInfoProgramAnalyzer reads broadcast metadata from 188-byte MPEG-TS.
// It supports neither PSI archives (.psc) nor media decoding. The only shared
// state is the read-only DB handle and logger; all parser state is per call.
// Recording timestamps are written to the caller-owned video, never retained
// in a no-argument RecordingTimeAnalyzer last-result cache.
type TSInfoProgramAnalyzer struct {
	DB     *sql.DB
	Logger *slog.Logger
}

func NewTSInfoProgramAnalyzer(db *sql.DB, logger *slog.Logger) *TSInfoProgramAnalyzer {
	return &TSInfoProgramAnalyzer{DB: db, Logger: logger}
}

func (a *TSInfoProgramAnalyzer) AnalyzeProgram(video *RecordedVideo, endTSOffset *int64) (*RecordedProgram, bool) {
	return a.AnalyzeProgramContext(context.Background(), video, endTSOffset, nil)
}

func (a *TSInfoProgramAnalyzer) AnalyzeProgramContext(ctx context.Context, video *RecordedVideo, endTSOffset *int64, preferredServiceID *int) (*RecordedProgram, bool) {
	if ctx == nil || video == nil || video.ContainerFormat != "MPEG-TS" || ctx.Err() != nil || video.Duration <= 0 || math.IsNaN(video.Duration) || math.IsInf(video.Duration, 0) || video.Duration >= float64(math.MaxInt64)/float64(time.Second) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f, err := os.Open(video.FilePath)
	if err != nil {
		a.logFailure(err)
		return nil, false
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, false
	}
	end := min(video.FileSize, stat.Size())
	if endTSOffset != nil {
		end = min(end, *endTSOffset)
	}
	return a.analyzeTSInfo(ctx, f, video, end, preferredServiceID)
}

func (a *TSInfoProgramAnalyzer) logFailure(err error) {
	if a != nil && a.Logger != nil {
		a.Logger.Debug("MPEG-TS metadata unavailable; using filename fallback.", "error", err)
	}
}

func tsinfoScan(ctx context.Context, r io.ReadSeeker, end, offset int64, pids []uint16, visit func(psi.Section) error) error {
	n := min(tsinfoWindowBytes, end-offset)
	if offset < 0 || n <= 0 {
		return nil
	}
	_, err := psi.ScanWindow(ctx, r, offset, psi.ScanOptions{MaxBytes: n, MaxSections: 32768, PIDs: pids}, visit)
	return err
}

func (a *TSInfoProgramAnalyzer) analyzeTSInfo(ctx context.Context, r io.ReadSeeker, video *RecordedVideo, end int64, preferred *int) (*RecordedProgram, bool) {
	if ctx == nil || video == nil || r == nil || end < psi.PacketSize || ctx.Err() != nil {
		return nil, false
	}
	// Work on a per-call copy: clock inference is provisional until EIT,
	// schedule timing, reader and context validation have all succeeded.
	callerVideo := video
	localVideo := *video
	video = &localVideo
	source := r
	cached, err := tsinfoReadWindows(ctx, r, end)
	if err != nil {
		a.logFailure(err)
		return nil, false
	}
	r = cached
	var pat *psi.PAT
	var sdts []*psi.SDT
	var nits []*psi.NIT
	err = tsinfoScan(ctx, r, end, 0, []uint16{0, 0x10, 0x11}, func(s psi.Section) error {
		switch s.Raw[0] {
		case 0:
			if s.PID != 0 {
				return nil
			}
			v, err := tsinfoDecodePAT(s)
			if err == nil && v.Header.Current && len(v.Programs) > 0 && pat == nil {
				pat = v
			}
		case 0x42:
			if s.PID != 0x11 {
				return nil
			}
			v, err := psi.DecodeSDT(s)
			if err == nil && v.Header.Current && len(sdts) < 256 {
				sdts = append(sdts, v)
			}
		case 0x40:
			if s.PID != 0x10 {
				return nil
			}
			v, err := psi.DecodeNIT(s)
			if err == nil && v.Header.Current && len(nits) < 256 {
				nits = append(nits, v)
			}
		}
		return nil
	})
	if err != nil || pat == nil {
		a.logFailure(err)
		return nil, false
	}
	sid := pat.Programs[0].ServiceID
	preferredFound := false
	if preferred != nil {
		for _, p := range pat.Programs {
			if int(p.ServiceID) == *preferred {
				sid = p.ServiceID
				preferredFound = true
				break
			}
		}
	}
	if !preferredFound && len(pat.Programs) > 1 {
		selected, err := tsinfoSelectService(ctx, r, end, pat)
		if err != nil {
			a.logFailure(err)
			return nil, false
		}
		if selected != nil {
			sid = *selected
		}
	}
	var channel *Channel
	for _, sdt := range sdts {
		if sdt.TransportStreamID != pat.TransportStreamID {
			continue
		}
		kind := reservations.GetNetworkType(int(sdt.OriginalNetworkID))
		if kind == "OTHER" {
			continue
		}
		for _, service := range sdt.Services {
			if service.ServiceID != sid || service.ServiceName == nil {
				continue
			}
			onid, serviceID, tsid := int(sdt.OriginalNetworkID), int(sid), int(pat.TransportStreamID)
			remocon := reservations.CalculateRemoconID(kind, serviceID)
			if kind == "GR" {
				remocon = 0
				found := false
				for _, nit := range nits {
					for _, stream := range nit.TransportStreams {
						if stream.TransportStreamID == pat.TransportStreamID && stream.OriginalNetworkID == sdt.OriginalNetworkID && stream.HasRemoteControlKeyID {
							remocon, found = int(stream.RemoteControlKeyID), true
							break
						}
					}
					if found {
						break
					}
				}
			}
			number, err := reservations.CalculateChannelNumber(ctx, a.DB, kind, onid, serviceID, remocon)
			if err != nil {
				a.logFailure(err)
				return nil, false
			}
			channel = &Channel{ID: tsinfoChannelID(onid, serviceID), DisplayChannelID: strings.ToLower(kind) + number, NetworkID: onid, ServiceID: serviceID, TransportStreamID: &tsid, RemoconID: remocon, ChannelNumber: number, Type: kind, Name: formatString(arib.Decode(service.ServiceName)), IsSubchannel: reservations.CalculateIsSubchannel(kind, serviceID)}
			break
		}
		if channel != nil {
			break
		}
	}
	if channel == nil {
		return nil, false
	}
	pmts, err := tsinfoPMTs(ctx, r, end, pat)
	if err != nil {
		a.logFailure(err)
		return nil, false
	}
	if pmt := pmts[sid]; pmt != nil {
		start, finish := tsinfoRecordingTime(ctx, r, end, pmt.PCRPID, video.Duration)
		if start != nil && finish != nil {
			video.RecordingStartTime, video.RecordingEndTime = start, finish
		}
	}
	var states [2]tsinfoEventState
	offset := closestMultiple(end/5, psi.PacketSize)
	visitEIT := func(s psi.Section) error {
		eit, err := psi.DecodeEIT(s)
		if err != nil || !eit.Header.Current || eit.Header.SectionNumber > 1 || int(eit.ServiceID) != channel.ServiceID || int(eit.TransportStreamID) != *channel.TransportStreamID || int(eit.OriginalNetworkID) != channel.NetworkID {
			return nil
		}
		i := eit.Header.SectionNumber
		for _, event := range eit.Events {
			states[i].merge(video, channel, event)
		}
		return nil
	}
	err = tsinfoScan(ctx, r, end, offset, []uint16{0x12, 0x26, 0x27}, visitEIT)
	if err != nil {
		a.logFailure(err)
		return nil, false
	}
	// Three 8-MiB schedule probes cap total source I/O at 88 MiB.
	for attempt := int64(0); attempt < 3; attempt++ {
		unknown := false
		for _, state := range states {
			if state.program != nil && !state.durationValid {
				unknown = true
			}
		}
		if !unknown {
			break
		}
		next := offset + tsinfoWindowBytes + 188*1000000 + attempt*((8<<20)+188*1000000)
		if next >= end {
			break
		}
		w, err := tsinfoLoadWindow(ctx, source, next, min(8<<20, end-next))
		if err != nil {
			a.logFailure(err)
			return nil, false
		}
		cached.windows = append(cached.windows, w)
		err = tsinfoScan(ctx, cached, min(end, next+(8<<20)), next, []uint16{0x12, 0x26, 0x27}, visitEIT)
		cached.windows = cached.windows[:len(cached.windows)-1]
		if err != nil {
			a.logFailure(err)
			return nil, false
		}
	}
	for i := range states {
		if p := states[i].program; p != nil && states[i].startValid && !states[i].durationValid && video.RecordingEndTime != nil {
			p.EndTime = *video.RecordingEndTime
			p.Duration = p.EndTime.Sub(p.StartTime).Seconds()
		}
	}
	events := [2]*RecordedProgram{states[0].program, states[1].program}
	p := events[0]
	if video.RecordingStartTime != nil && events[1] != nil && states[1].startValid {
		delta := events[1].StartTime.Sub(*video.RecordingStartTime)
		if delta >= 0 && delta <= time.Minute {
			p = events[1]
		}
	}
	if p == nil || p.StartTime.IsZero() || p.Duration <= 0 || ctx.Err() != nil {
		return nil, false
	}
	callerVideo.RecordingStartTime, callerVideo.RecordingEndTime = video.RecordingStartTime, video.RecordingEndTime
	p.Video = *video
	return p, true
}

func tsinfoBasicEvent(video *RecordedVideo, channel *Channel, event psi.Event) *RecordedProgram {
	sid, nid, eid := channel.ServiceID, channel.NetworkID, int(event.EventID)
	p := &RecordedProgram{Channel: channel, NetworkID: &nid, ServiceID: &sid, EventID: &eid,
		Title: formatString(fileNameWithoutExtension(video.FilePath)), Description: DefaultDescription,
		Detail: []DetailEntry{}, Genres: []Genre{}, StartTime: event.StartTime, IsFree: !event.FreeCA,
		PrimaryAudioType: DefaultPrimaryAudioType, PrimaryAudioLanguage: DefaultPrimaryAudioLanguage}
	if event.DurationValid {
		p.Duration = event.Duration.Seconds()
	} else {
		p.Duration = video.Duration
	}
	p.EndTime = p.StartTime.Add(time.Duration(p.Duration * float64(time.Second)))
	for _, d := range event.Descriptors {
		if d.Tag != 0x4d || len(d.Data) < 5 {
			continue
		}
		n := int(d.Data[3])
		if 4+n >= len(d.Data) {
			continue
		}
		m := int(d.Data[4+n])
		if 5+n+m != len(d.Data) {
			continue
		}
		p.Title = formatString(arib.Decode(d.Data[4 : 4+n]))
		p.Description = formatString(arib.Decode(d.Data[5+n:]))
	}
	return p
}
