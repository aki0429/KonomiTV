package metadata

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psc"
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

type tsinfoPSIArchive struct {
	*bytes.Reader
	packets       []byte
	start, finish *time.Time
}

// tsinfoReadArchive は本家のコンストラクタと同じ採取範囲で仮想TSを構築する。
// 元動画のサイズや位置を変更せず、時刻は書庫のTOT到着時刻の差で補正する。
func tsinfoReadArchive(ctx context.Context, reader io.Reader, duration float64) (*tsinfoPSIArchive, bool, error) {
	a := &tsinfoPSIArchive{}
	counters := map[uint16]byte{}
	var firstClock, lastClock *time.Time
	var lastTime, firstTOT, lastTOT float64
	seenTOT := false
	valid, err := psc.Read(ctx, reader, []uint16{0, 0x10, 0x11, 0x14, 0x12, 0x26, 0x27}, func(s psc.Section) bool {
		lastTime = s.Time
		if s.PID == 0x12 || s.PID == 0x26 || s.PID == 0x27 {
			if s.Time < duration*0.2 {
				return true
			}
		} else if s.PID == 0x14 {
			if !seenTOT {
				firstTOT = s.Time
				seenTOT = true
			}
			lastTOT = s.Time
			if tot, err := psi.DecodeTOT(psi.Section{PID: s.PID, Raw: s.Data}); err == nil && tot.ClockValid {
				clock := tot.Clock
				if firstClock == nil {
					firstClock = &clock
				}
				lastClock = &clock
			}
		} else if s.Time > 60 {
			return true
		}
		for position := 0; position < len(s.Data); {
			packet := bytes.Repeat([]byte{0xff}, psi.PacketSize)
			packet[0], packet[1], packet[2], packet[3] = 0x47, byte(s.PID>>8), byte(s.PID), 0x10|counters[s.PID]
			counters[s.PID] = (counters[s.PID] + 1) & 15
			start := 4
			if position == 0 {
				packet[1] |= 0x40
				packet[4] = 0
				start = 5
			}
			position += copy(packet[start:], s.Data[position:])
			a.packets = append(a.packets, packet...)
		}
		return true
	})
	if firstClock != nil && lastClock != nil {
		start := firstClock.Add(-time.Duration(math.RoundToEven(firstTOT*1e6)) * time.Microsecond)
		finish := lastClock.Add(time.Duration(math.RoundToEven((lastTime-lastTOT)*1e6)) * time.Microsecond)
		a.start, a.finish = &start, &finish
	}
	a.Reader = bytes.NewReader(a.packets)
	return a, valid, err
}

func (a *TSInfoProgramAnalyzer) analyzeArchive(ctx context.Context, video *RecordedVideo) (*RecordedProgram, bool) {
	base := filepath.Base(video.FilePath)
	extension := filepath.Ext(base)
	if extension == base || extension == "." {
		extension = ""
	}
	path := strings.TrimSuffix(video.FilePath, extension) + ".psc"
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	archive, valid, err := tsinfoReadArchive(ctx, file, video.Duration)
	if ctx.Err() != nil {
		return nil, false
	}
	if err != nil || !valid {
		a.logFailure(err)
	}
	// 本家は不完全な書庫でも、既に復元できたセクションを使う。
	if archive.start != nil && archive.finish != nil {
		video.RecordingStartTime, video.RecordingEndTime = archive.start, archive.finish
	}
	if len(archive.packets) == 0 {
		return nil, false
	}
	return a.analyzeTSInfo(ctx, archive, video, int64(len(archive.packets)), nil)
}
