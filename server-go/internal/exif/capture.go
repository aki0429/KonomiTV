package exif

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// CaptureInfo は本家のキャプチャ登録で必要な EXIF と、壊れたメタデータの区別を保持する。
type CaptureInfo struct {
	XPComment        *string
	DateTimeOriginal string
	Invalid          bool
}

// ParseCapture は JPEG/PNG の XPComment と DateTimeOriginal を境界検証付きで読む。
func ParseCapture(data []byte) CaptureInfo {
	var result CaptureInfo
	var tiff []byte
	if bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		for offset := 2; offset+4 <= len(data); {
			if data[offset] != 0xff || data[offset+1] == 0xda || data[offset+1] == 0xd9 {
				break
			}
			size := int(binary.BigEndian.Uint16(data[offset+2:]))
			if size < 2 || size > len(data)-offset-2 {
				break
			}
			segment := data[offset+4 : offset+2+size]
			if data[offset+1] == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
				tiff = segment[6:]
				break
			}
			offset += size + 2
		}
	} else if bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		for offset := 8; offset+12 <= len(data); {
			size := uint64(binary.BigEndian.Uint32(data[offset:]))
			if size > uint64(len(data)-offset-12) {
				break
			}
			if string(data[offset+4:offset+8]) == "eXIf" {
				tiff = data[offset+8 : offset+8+int(size)]
				break
			}
			offset += int(size) + 12
		}
	}
	if tiff == nil {
		return result
	}
	if len(tiff) < 8 {
		result.Invalid = true
		return result
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		result.Invalid = true
		return result
	}
	if order.Uint16(tiff[2:4]) != 42 {
		result.Invalid = true
		return result
	}
	var readDirectory func(uint32, bool)
	readDirectory = func(rawOffset uint32, isSubIFD bool) {
		offset := int64(rawOffset)
		if offset < 8 || offset+2 > int64(len(tiff)) {
			result.Invalid = true
			return
		}
		count := int64(order.Uint16(tiff[offset : offset+2]))
		if count > (int64(len(tiff))-offset-2)/12 {
			result.Invalid = true
			return
		}
		for index := int64(0); index < count; index++ {
			entry := tiff[offset+2+index*12 : offset+14+index*12]
			tag, kind := order.Uint16(entry), order.Uint16(entry[2:])
			number := uint64(order.Uint32(entry[4:]))
			if tag == 34665 && !isSubIFD && kind == 4 && number == 1 {
				readDirectory(order.Uint32(entry[8:]), true)
				continue
			}
			if tag != tagXPComment && tag != 36867 {
				continue
			}
			size := number * uint64(typeSize(kind))
			value := entry[8:12]
			if size <= 4 {
				value = value[:size]
			} else {
				start := uint64(order.Uint32(entry[8:]))
				if start > uint64(len(tiff)) || size > uint64(len(tiff))-start {
					result.Invalid = true
					continue
				}
				value = tiff[start : start+size]
			}
			if tag == 36867 && kind == 2 {
				result.DateTimeOriginal = strings.TrimRight(string(value), "\x00")
			}
			if tag == tagXPComment {
				if (kind != 1 && kind != 7) || len(value)%2 != 0 {
					result.Invalid = true
					continue
				}
				units := make([]uint16, len(value)/2)
				valid := true
				for index := range units {
					units[index] = binary.LittleEndian.Uint16(value[index*2:])
				}
				for index := 0; index < len(units); index++ {
					if units[index] >= 0xd800 && units[index] <= 0xdbff {
						if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
							valid = false
							break
						}
						index++
					} else if units[index] >= 0xdc00 && units[index] <= 0xdfff {
						valid = false
					}
				}
				if !valid {
					result.Invalid = true
					continue
				}
				comment := strings.TrimRight(string(utf16.Decode(units)), "\x00")
				result.XPComment = &comment
			}
		}
	}
	readDirectory(order.Uint32(tiff[4:]), false)
	return result
}
