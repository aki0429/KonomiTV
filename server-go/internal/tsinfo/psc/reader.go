// Package psc は psisiarc の PSI/SI 書庫から時刻付きのセクションを取り出す。
package psc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Section は書庫の先頭時刻を基準とした秒数と元のPID、セクション本体。
// Data は辞書と共有しているため、コールバック内で変更してはならない。
type Section struct {
	Time float64
	PID  uint16
	Data []byte
}

type entry struct {
	pid  int
	data []byte
	size int
}

// Read は本家 TSInfoAnalyzer.readPSIData と同じ辞書の引き継ぎと30 bit時刻を処理する。
// 形式不正またはコールバックの中断は false、正常な終端は true を返す。
// 終端の短いヘッダー・異なるmagicは、本家と同じく正常な終端として扱う。
func Read(ctx context.Context, reader io.Reader, targetPIDs []uint16, callback func(Section) bool) (bool, error) {
	if ctx == nil || reader == nil || callback == nil {
		return false, fmt.Errorf("invalid PSI archive reader")
	}
	read := func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := io.ReadFull(reader, data)
		return err
	}
	invalid := func(err error) (bool, error) {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	targets := map[int]bool{}
	for _, pid := range targetPIDs {
		targets[int(pid)] = true
	}
	var previous []entry
	initial := int64(-1)
	for {
		var header [32]byte
		if err := read(header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			return false, err
		}
		if !bytes.Equal(header[:8], []byte("Pssc\r\n\x9a\n")) {
			return true, nil
		}
		u16 := func(p int) int { return int(binary.LittleEndian.Uint16(header[p:])) }
		times, length, window := u16(10), u16(12), u16(14)
		dataSize := int64(binary.LittleEndian.Uint32(header[16:]))
		bufferSize := int64(binary.LittleEndian.Uint32(header[20:]))
		codes := int64(binary.LittleEndian.Uint32(header[24:]))
		if window < length || bufferSize < dataSize || window > 65536-4096 {
			return false, nil
		}
		table := make([]byte, times*4+length*2)
		if err := read(table); err != nil {
			return invalid(err)
		}
		remaining := dataSize
		dictionary := make([]entry, 0, window)
		for index := range length {
			code := int(binary.LittleEndian.Uint16(table[times*4+index*2:])) - 4096
			if code >= 0 {
				if code >= len(previous) || previous[code].pid < 0 {
					return false, nil
				}
				dictionary = append(dictionary, previous[code])
				previous[code].pid = -1
			} else {
				remaining -= 2
				var pid [2]byte
				if err := read(pid[:]); err != nil {
					return invalid(err)
				}
				if remaining < 0 {
					return false, nil
				}
				dictionary = append(dictionary, entry{pid: int(binary.LittleEndian.Uint16(pid[:]) % 0x2000), size: code + 4097})
			}
		}
		for index := range length {
			if dictionary[index].size == 0 {
				continue
			}
			size := dictionary[index].size
			remaining -= int64(size)
			data := make([]byte, size)
			if err := read(data); err != nil {
				return invalid(err)
			}
			if remaining < 0 {
				return false, nil
			}
			if targets[dictionary[index].pid] {
				dictionary[index].data = data
			}
			dictionary[index].size = 0
		}
		for index := 0; index < window-length; index++ {
			if index >= len(previous) {
				return false, nil
			}
			if previous[index].pid >= 0 {
				dictionary = append(dictionary, previous[index])
			}
		}
		previous = dictionary
		// 不要データは小さいバッファで読み捨て、大きな宣言値でも一括確保しない。
		remaining += dataSize % 2
		var scratch [32768]byte
		for remaining > 0 {
			n := min(remaining, int64(len(scratch)))
			if err := read(scratch[:n]); err != nil {
				return invalid(err)
			}
			remaining -= n
		}
		current := int64(-1)
		for position := 0; position < times*4; position += 4 {
			absolute := binary.LittleEndian.Uint32(table[position:])
			if absolute == 0xffffffff {
				current = -1
				continue
			}
			if absolute >= 0x80000000 {
				current = int64(absolute % 0x40000000)
				if initial < 0 {
					initial = current
				}
				continue
			}
			if current >= 0 {
				current += int64(binary.LittleEndian.Uint16(table[position:]))
			}
			n := int(binary.LittleEndian.Uint16(table[position+2:])) + 1
			data := make([]byte, n*2)
			if err := read(data); err != nil {
				return invalid(err)
			}
			seconds := float64((current+0x40000000-initial)%0x40000000) / 11250
			for index := range n {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				code := int(binary.LittleEndian.Uint16(data[index*2:])) - 4096
				if code < 0 || code >= len(dictionary) {
					return false, nil
				}
				if dictionary[code].data != nil && !callback(Section{seconds, uint16(dictionary[code].pid), dictionary[code].data}) {
					return false, nil
				}
			}
		}
		trailer := 4 - (int64(length*2)+(dataSize+1)/2*2+codes*2)%4
		if err := read(scratch[:trailer]); err != nil {
			return invalid(err)
		}
	}
}
