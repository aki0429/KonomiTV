package metadata

import (
	"bufio"
	"context"
	"io"
	"math"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/psi"
)

// tsinfoRecordingTime は録画開始/終了時刻を Python 版
// TSInfoAnalyzer.analyzeRecordingTime() (MPEG-TS 分岐) と同一の規則で算出する。
//
// Python 実装が採る規則は次の 3 点で、Go 側の従来実装とは異なっていた:
//  1. PID 選択なし: biim の `ts.pcr(packet)` を全パケットに対して呼ぶため、PCR は
//     PMT が示す PCR PID だけでなく「別 PID の PCR」も最初/最新の観測に混ざる。
//     実測では TOT セクション開始直前の最新 PCR が PMT の PCR PID とは別 PID の
//     もので、base で 20622 tick (=0.229133 s) ずれていたケースがあった。
//  2. PCR extension を無視: Python は 33bit の PCR base を 90000 で割るだけで、
//     9bit の extension (27MHz 精度) は使わない。Go は 27MHz tick を使っていたため
//     最大 11.07 µs のずれが出ていた (実測で 8.038 µs のずれ)。
//  3. マイクロ秒丸め: Python は `timedelta(seconds=elapsed)` でマイクロ秒へ
//     四捨五入 (banker's rounding) する。Go は ns へ切り捨てていた。
//
// 負の経過時間は Python と同じく 0 にクランプする (PCR の巻き戻しで過去時計を
// 作らない)。pcrPID は Python 互換のため参照しない (全 PID を観測する)。
func tsinfoRecordingTime(ctx context.Context, r io.ReadSeeker, end int64, _ uint16, duration float64) (*time.Time, *time.Time) {
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration >= float64(math.MaxInt64)/float64(time.Second) {
		return nil, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, nil
	}
	reader := bufio.NewReaderSize(io.LimitReader(r, min(end, tsinfoWindowBytes)), 32<<10)
	// PCRPID を指定しないことで、Python と同じく全 PID の PCR を観測する。
	// psi.Assembler は PCRPID が nil のとき a.current を全 PID で更新する。
	assembler := psi.NewAssembler(psi.AssemblerOptions{PIDs: []uint16{0x14}})
	// firstBase はファイル先頭から見て最初に観測した PCR base (90kHz, 全 PID 対象)。
	var firstBase *uint64
	var offset int64
	for ctx.Err() == nil {
		raw, err := reader.Peek(psi.PacketSize)
		if err != nil {
			return nil, nil
		}
		if raw[0] != 0x47 {
			// 同期バイト外れは 1 バイトずらして再同期する。
			_, _ = reader.Discard(1)
			offset++
			assembler.Reset()
			continue
		}
		packet, err := psi.ParsePacket(raw, offset)
		if err != nil {
			assembler.ResetPID(packet.PID)
		} else {
			if packet.PCR != nil && firstBase == nil {
				base := packet.PCR.Base
				firstBase = &base
			}
			sections, _ := assembler.Push(packet)
			for _, section := range sections {
				tot, err := psi.DecodeTOT(section)
				if err != nil || !tot.ClockValid || firstBase == nil || section.PCRAtPUSI == nil {
					continue
				}
				start, finish := tsinfoRecordingClock(tot.Clock, *firstBase, section.PCRAtPUSI.Value.Base, duration)
				if start == nil {
					continue
				}
				return start, finish
			}
		}
		_, _ = reader.Discard(psi.PacketSize)
		offset += psi.PacketSize
	}
	return nil, nil
}

// tsinfoRecordingClock は Python 版 analyzeRecordingTime() の算術をそのまま再現する。
//
// Python は first_pcr_sec = firstBase/90000、pcr_at_section_start_sec =
// sectionBase/90000 を float で求め、
//
//	elapsed = max(pcr_at_section_start_sec - first_pcr_sec, 0.0)
//	recording_start_time = TOT時刻 - timedelta(seconds=elapsed)
//	recording_end_time   = recording_start_time + timedelta(seconds=duration)
//
// とする。firstBase / sectionBase はいずれも 90kHz の PCR base で、9bit extension は
// 使わない。timedelta(seconds=...) の丸めはマイクロ秒への四捨五入 (banker's
// rounding) なので math.RoundToEven で一致させ、整数 ns への切り捨てを避ける。
func tsinfoRecordingClock(tot time.Time, firstBase, sectionBase uint64, duration float64) (*time.Time, *time.Time) {
	elapsed := float64(sectionBase)/90000.0 - float64(firstBase)/90000.0
	if elapsed < 0 {
		elapsed = 0
	}
	start := tot.Add(-time.Duration(math.RoundToEven(elapsed*1e6)) * time.Microsecond)
	finish := start.Add(time.Duration(math.RoundToEven(duration*1e6)) * time.Microsecond)
	return &start, &finish
}
