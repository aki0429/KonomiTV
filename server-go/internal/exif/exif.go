// Package exif は画像ファイルから EXIF の最小限の情報を読み取る。
//
// KonomiTV のキャプチャ画像は、クライアント側の CaptureCompositor が
// EXIF の XPComment タグ (0x9C9C) に UTF-16LE エンコードした JSON メタデータを格納している。
// また、カメラ由来の JPEG には回転情報 (Orientation タグ 0x0112) が含まれることがある。
// Go の標準ライブラリでは EXIF を扱えないため、必要なタグのみを自前で解析する。
package exif

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"unicode/utf16"
)

// Orientation は EXIF の Orientation タグ (0x0112) の値。
// 値の意味は EXIF 仕様に準拠する (1: 正位置、2: 左右反転、3: 180 度回転、4: 上下反転、
// 5: 90 度回転 + 左右反転、6: 90 度回転、7: 270 度回転 + 左右反転、8: 270 度回転) 。
type Orientation int

// タグ番号
const (
	tagOrientation = 0x0112
	tagXPComment   = 0x9C9C
)

// Info は画像から読み取った EXIF の情報。
type Info struct {
	// Orientation は回転情報 (タグがない場合は 1) 。
	Orientation Orientation
	// XPComment は XPComment タグの文字列 (タグがない場合は空文字) 。
	XPComment string
}

// ReadFile は指定されたパスの画像ファイルから EXIF の情報を読み取る。
// EXIF が存在しない場合や解析に失敗した場合は、空の Info を返す (エラーにはしない) 。
func ReadFile(path string) Info {
	file, err := os.Open(path)
	if err != nil {
		return Info{Orientation: 1}
	}
	defer func() { _ = file.Close() }()

	// EXIF は APP1 セグメント (最大 64KB) に格納されているため、先頭 128KB のみを読み取る
	data, err := io.ReadAll(io.LimitReader(file, 128*1024))
	if err != nil {
		return Info{Orientation: 1}
	}
	return Parse(data)
}

// Parse は画像データから EXIF の情報を読み取る。
// JPEG の APP1 セグメントと PNG の eXIf チャンクに対応する。
func Parse(data []byte) Info {
	info := Info{Orientation: 1}

	// JPEG の場合
	if len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8 {
		for offset := 2; offset+4 <= len(data); {
			// マーカーを探す
			if data[offset] != 0xFF {
				break
			}
			marker := data[offset+1]
			// マーカー長 (自身を含む 2 バイト) を取得する
			if offset+4 > len(data) {
				break
			}
			segmentLength := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
			if segmentLength < 2 || offset+2+segmentLength > len(data) {
				break
			}
			// APP1 (0xE1) かつ "Exif\0\0" で始まる場合は EXIF として解析する
			if marker == 0xE1 && segmentLength >= 8 {
				segment := data[offset+4 : offset+2+segmentLength]
				if bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
					parseTIFF(segment[6:], &info)
					return info
				}
			}
			// 画像データの開始 (SOS) に達した場合は終了する
			if marker == 0xDA {
				break
			}
			offset += 2 + segmentLength
		}
		return info
	}

	// PNG の場合 (eXIf チャンクに EXIF が格納される)
	if len(data) > 8 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		for offset := 8; offset+8 <= len(data); {
			chunkLength := int(binary.BigEndian.Uint32(data[offset : offset+4]))
			chunkType := string(data[offset+4 : offset+8])
			if offset+12+chunkLength > len(data) {
				break
			}
			if chunkType == "eXIf" {
				parseTIFF(data[offset+8:offset+8+chunkLength], &info)
				return info
			}
			// IEND に達した場合は終了する
			if chunkType == "IEND" {
				break
			}
			offset += 12 + chunkLength
		}
	}
	return info
}

// parseTIFF は TIFF ヘッダー以降のデータから必要なタグを読み取る。
func parseTIFF(data []byte, info *Info) {
	if len(data) < 8 {
		return
	}
	// バイトオーダーを判定する
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return
	}
	// 42 (TIFF のマジックナンバー) を確認する
	if order.Uint16(data[2:4]) != 42 {
		return
	}
	ifdOffset := int(order.Uint32(data[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(data) {
		return
	}
	readIFD(data, ifdOffset, order, info)
}

// readIFD は IFD (Image File Directory) を走査して必要なタグを読み取る。
func readIFD(data []byte, offset int, order binary.ByteOrder, info *Info) {
	if offset+2 > len(data) {
		return
	}
	entryCount := int(order.Uint16(data[offset : offset+2]))
	for index := range entryCount {
		entryOffset := offset + 2 + index*12
		if entryOffset+12 > len(data) {
			return
		}
		entry := data[entryOffset : entryOffset+12]
		tag := order.Uint16(entry[0:2])
		valueType := order.Uint16(entry[2:4])
		count := int(order.Uint32(entry[4:8]))

		// タグの値が格納されている位置を求める
		// 4 バイト以内に収まる値はエントリー内に直接格納されている
		valueSize := typeSize(valueType) * count
		var value []byte
		if valueSize <= 4 {
			value = entry[8 : 8+valueSize]
		} else {
			valueOffset := int(order.Uint32(entry[8:12]))
			if valueOffset+valueSize > len(data) {
				continue
			}
			value = data[valueOffset : valueOffset+valueSize]
		}

		switch tag {
		case tagOrientation:
			// Orientation は SHORT (2 バイト) の整数
			if len(value) >= 2 {
				info.Orientation = Orientation(order.Uint16(value[0:2]))
			}
		case tagXPComment:
			// XPComment は UCS-2 (UTF-16LE) で格納されている (バイトオーダーは常にリトルエンディアン)
			info.XPComment = decodeUTF16LE(value)
		}
	}
}

// typeSize は TIFF のフィールド型 1 要素あたりのバイト数を返す。
func typeSize(valueType uint16) int {
	switch valueType {
	case 1, 2, 6, 7: // BYTE, ASCII, SBYTE, UNDEFINED
		return 1
	case 3, 8: // SHORT, SSHORT
		return 2
	case 4, 9, 11: // LONG, SLONG, FLOAT
		return 4
	case 5, 10, 12: // RATIONAL, SRATIONAL, DOUBLE
		return 8
	default:
		return 1
	}
}

// decodeUTF16LE は UTF-16LE のバイト列を文字列に変換する (末尾のヌル文字は除去する) 。
func decodeUTF16LE(value []byte) string {
	if len(value)%2 != 0 {
		value = value[:len(value)-1]
	}
	units := make([]uint16, 0, len(value)/2)
	for index := 0; index+2 <= len(value); index += 2 {
		units = append(units, binary.LittleEndian.Uint16(value[index:index+2]))
	}
	// 末尾のヌル文字を除去する
	for len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	return string(utf16.Decode(units))
}
