package reservations

import (
	"time"
	"unicode/utf16"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// このファイルは server/app/utils/edcb/CtrlCmdUtil.py のバイナリ読み書き (__writeXxx / __readXxx)
// をそのまま移植したもの。リトルエンディアンで、各構造体は先頭に 4 バイトのサイズを持つ。

// readError はバッファを EDCB のデータ構造として読み取るのに失敗したことを表す内部エラー。
//
// Python 版の CtrlCmdUtil.__ReadError と同じ役割で、読み取り中は panic で伝播させ、
// 各コマンドの入口で捕捉して「デコード失敗 (Python 版の None) 」に変換する。
type readError struct{}

func (readError) Error() string { return "edcb: failed to read the response buffer" }

// writeError は Python 側の OverflowError (to_bytes の範囲外) に対応する内部エラー。
type writeError struct{}

func (writeError) Error() string { return "edcb: value is out of range for the field" }

// wireReader は EDCB のレスポンスバッファを読み進めるリーダー。
//
// pos は現在位置、size は現在の構造体の終端を表す (Python 版が各読み取り関数に渡している size と同じ) 。
type wireReader struct {
	buf []byte
	pos int
}

func (r *wireReader) remaining(size int) int { return size - r.pos }

func (r *wireReader) readByte(size int) int {
	if r.remaining(size) < 1 {
		panic(readError{})
	}
	value := r.buf[r.pos]
	r.pos++
	return int(value)
}

func (r *wireReader) readUshort(size int) int {
	if r.remaining(size) < 2 {
		panic(readError{})
	}
	value := int(r.buf[r.pos]) | int(r.buf[r.pos+1])<<8
	r.pos += 2
	return value
}

func (r *wireReader) readInt(size int) int {
	if r.remaining(size) < 4 {
		panic(readError{})
	}
	value := int32(uint32(r.buf[r.pos]) | uint32(r.buf[r.pos+1])<<8 |
		uint32(r.buf[r.pos+2])<<16 | uint32(r.buf[r.pos+3])<<24)
	r.pos += 4
	return int(value)
}

func (r *wireReader) readUint(size int) int {
	if r.remaining(size) < 4 {
		panic(readError{})
	}
	value := int(uint32(r.buf[r.pos]) | uint32(r.buf[r.pos+1])<<8 |
		uint32(r.buf[r.pos+2])<<16 | uint32(r.buf[r.pos+3])<<24)
	r.pos += 4
	return value
}

func (r *wireReader) readLong(size int) int64 {
	if r.remaining(size) < 8 {
		panic(readError{})
	}
	var value uint64
	for index := 7; index >= 0; index-- {
		value = value<<8 | uint64(r.buf[r.pos+index])
	}
	r.pos += 8
	return int64(value)
}

// unixEpoch は Python 版 CtrlCmdUtil.UNIX_EPOCH (1970-01-01 09:00 JST) 。
var unixEpoch = time.Date(1970, 1, 1, 9, 0, 0, 0, constants.JST)

// readSystemTime は EDCB の SystemTime 構造体 (16 バイト) を読み取る。
//
// 読み取った日付が不正な場合は Python 版と同じく UNIX_EPOCH を返す。
func (r *wireReader) readSystemTime(size int) time.Time {
	if r.remaining(size) < 16 {
		panic(readError{})
	}
	base := r.pos
	year := int(r.buf[base]) | int(r.buf[base+1])<<8
	month := int(r.buf[base+2]) | int(r.buf[base+3])<<8
	day := int(r.buf[base+6]) | int(r.buf[base+7])<<8
	hour := int(r.buf[base+8]) | int(r.buf[base+9])<<8
	minute := int(r.buf[base+10]) | int(r.buf[base+11])<<8
	second := int(r.buf[base+12]) | int(r.buf[base+13])<<8
	r.pos += 16

	// Python の datetime() は範囲外の値で ValueError になる
	if year < 1 || year > 9999 || month < 1 || month > 12 ||
		day < 1 || day > daysInMonth(year, month) ||
		hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return unixEpoch
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, constants.JST)
}

// daysInMonth は指定された年月の日数を返す。
func daysInMonth(year int, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// readString は長さプレフィックス付きの UTF-16LE 文字列を読み取る。
func (r *wireReader) readString(size int) string {
	length := r.readInt(size)
	if length < 6 || r.remaining(size) < length-4 {
		panic(readError{})
	}
	text := decodeUTF16LE(r.buf[r.pos : r.pos+length-6])
	r.pos += length - 4
	return text
}

// decodeUTF16LE は UTF-16LE のバイト列を文字列に変換する (Python の bytes.decode('utf_16_le') 相当) 。
func decodeUTF16LE(buffer []byte) string {
	// Python strict decoder と同じく、奇数長と孤立 surrogate を補完しない。
	if len(buffer)%2 != 0 {
		panic(readError{})
	}
	units := make([]uint16, 0, len(buffer)/2)
	for index := 0; index+1 < len(buffer); index += 2 {
		units = append(units, uint16(buffer[index])|uint16(buffer[index+1])<<8)
	}
	for index := 0; index < len(units); index++ {
		unit := units[index]
		if unit >= 0xD800 && unit <= 0xDBFF {
			if index+1 >= len(units) || units[index+1] < 0xDC00 || units[index+1] > 0xDFFF {
				panic(readError{})
			}
			index++
		} else if unit >= 0xDC00 && unit <= 0xDFFF {
			panic(readError{})
		}
	}
	return string(utf16.Decode(units))
}

// readVector は長さプレフィックス付きの配列を読み取る。
func readVector[T any](r *wireReader, size int, read func(*wireReader, int) T) []T {
	vectorSize := r.readInt(size)
	count := r.readInt(size)
	if vectorSize < 8 || count < 0 || r.remaining(size) < vectorSize-8 {
		panic(readError{})
	}
	end := r.pos + vectorSize - 8
	// 壊れたレスポンスで巨大なメモリを確保しないよう、初期容量は控えめにする
	capacity := count
	if capacity > 1024 {
		capacity = 1024
	}
	values := make([]T, 0, capacity)
	for index := 0; index < count; index++ {
		values = append(values, read(r, end))
	}
	r.pos = end
	return values
}

// readStructIntro は構造体のサイズを読み取り、その構造体の終端位置を返す。
func (r *wireReader) readStructIntro(size int) int {
	structSize := r.readInt(size)
	if structSize < 4 || r.remaining(size) < structSize-4 {
		panic(readError{})
	}
	return r.pos + structSize - 4
}

// ***** 各構造体のリーダー *****

func readFileData(r *wireReader, size int) FileData {
	size = r.readStructIntro(size)
	name := r.readString(size)
	dataSize := r.readInt(size)
	r.readInt(size)
	if dataSize < 0 || r.remaining(size) < dataSize {
		panic(readError{})
	}
	value := FileData{Name: name, Data: append([]byte(nil), r.buf[r.pos:r.pos+dataSize]...)}
	r.pos = size
	return value
}

func readRecFileSetInfo(r *wireReader, size int) RecFileSetInfo {
	size = r.readStructIntro(size)
	value := RecFileSetInfo{
		RecFolder:     r.readString(size),
		WritePlugIn:   r.readString(size),
		RecNamePlugIn: r.readString(size),
	}
	r.readString(size)
	r.pos = size
	return value
}

func readRecSettingData(r *wireReader, size int) RecSettingData {
	size = r.readStructIntro(size)
	value := RecSettingData{
		RecMode:       r.readByte(size),
		Priority:      r.readByte(size),
		TuijyuuFlag:   r.readByte(size) != 0,
		ServiceMode:   r.readUint(size),
		PittariFlag:   r.readByte(size) != 0,
		BatFilePath:   r.readString(size),
		RecFolderList: readVector(r, size, readRecFileSetInfo),
		SuspendMode:   r.readByte(size),
		RebootFlag:    r.readByte(size) != 0,
	}
	useMarginFlag := r.readByte(size) != 0
	startMargin := r.readInt(size)
	endMargin := r.readInt(size)
	if useMarginFlag {
		value.StartMargin = &startMargin
		value.EndMargin = &endMargin
	}
	value.ContinueRec = r.readByte(size) != 0
	value.PartialRecFlag = r.readByte(size)
	value.TunerID = r.readUint(size)
	value.PartialRecFolder = readVector(r, size, readRecFileSetInfo)
	r.pos = size
	return value
}

func readReserveData(r *wireReader, size int) ReserveData {
	size = r.readStructIntro(size)
	value := ReserveData{
		Title:          r.readString(size),
		StartTime:      r.readSystemTime(size),
		DurationSecond: r.readUint(size),
		StationName:    r.readString(size),
		Onid:           r.readUshort(size),
		Tsid:           r.readUshort(size),
		Sid:            r.readUshort(size),
		Eid:            r.readUshort(size),
		Comment:        r.readString(size),
		ReserveID:      r.readInt(size),
	}
	r.readByte(size)
	value.OverlapMode = r.readByte(size)
	r.readString(size)
	value.StartTimeEPG = r.readSystemTime(size)
	value.RecSetting = readRecSettingData(r, size)
	r.readInt(size)
	value.RecFileNameList = readVector(r, size, func(r *wireReader, size int) string {
		return r.readString(size)
	})
	r.readInt(size)
	r.pos = size
	return value
}

func readServiceInfo(r *wireReader, size int) ServiceInfo {
	size = r.readStructIntro(size)
	value := ServiceInfo{
		Onid:                 r.readUshort(size),
		Tsid:                 r.readUshort(size),
		Sid:                  r.readUshort(size),
		ServiceType:          r.readByte(size),
		PartialReceptionFlag: r.readByte(size),
		ServiceProviderName:  r.readString(size),
		ServiceName:          r.readString(size),
		NetworkName:          r.readString(size),
		TsName:               r.readString(size),
		RemoteControlKeyID:   r.readByte(size),
	}
	r.pos = size
	return value
}

func readShortEventInfo(r *wireReader, size int) ShortEventInfo {
	size = r.readStructIntro(size)
	value := ShortEventInfo{
		EventName: r.readString(size),
		TextChar:  r.readString(size),
	}
	r.pos = size
	return value
}

func readExtendedEventInfo(r *wireReader, size int) ExtendedEventInfo {
	size = r.readStructIntro(size)
	value := ExtendedEventInfo{TextChar: r.readString(size)}
	r.pos = size
	return value
}

func readContentData(r *wireReader, size int) ContentData {
	size = r.readStructIntro(size)
	contentNibble := r.readUshort(size)
	userNibble := r.readUshort(size)
	value := ContentData{
		ContentNibble: (contentNibble>>8 | contentNibble<<8) & 0xFFFF,
		UserNibble:    (userNibble>>8 | userNibble<<8) & 0xFFFF,
	}
	r.pos = size
	return value
}

func readContentInfo(r *wireReader, size int) ContentInfo {
	size = r.readStructIntro(size)
	value := ContentInfo{NibbleList: readVector(r, size, readContentData)}
	r.pos = size
	return value
}

func readComponentInfo(r *wireReader, size int) ComponentInfo {
	size = r.readStructIntro(size)
	value := ComponentInfo{
		StreamContent: r.readByte(size),
		ComponentType: r.readByte(size),
		ComponentTag:  r.readByte(size),
		TextChar:      r.readString(size),
	}
	r.pos = size
	return value
}

func readAudioComponentInfoData(r *wireReader, size int) AudioComponentInfoData {
	size = r.readStructIntro(size)
	value := AudioComponentInfoData{
		StreamContent:      r.readByte(size),
		ComponentType:      r.readByte(size),
		ComponentTag:       r.readByte(size),
		StreamType:         r.readByte(size),
		SimulcastGroupTag:  r.readByte(size),
		EsMultiLingualFlag: r.readByte(size),
		MainComponentFlag:  r.readByte(size),
		QualityIndicator:   r.readByte(size),
		SamplingRate:       r.readByte(size),
		TextChar:           r.readString(size),
	}
	r.pos = size
	return value
}

func readAudioComponentInfo(r *wireReader, size int) AudioComponentInfo {
	size = r.readStructIntro(size)
	value := AudioComponentInfo{ComponentList: readVector(r, size, readAudioComponentInfoData)}
	r.pos = size
	return value
}

func readEventData(r *wireReader, size int) EventData {
	size = r.readStructIntro(size)
	value := EventData{
		Onid: r.readUshort(size),
		Tsid: r.readUshort(size),
		Sid:  r.readUshort(size),
		Eid:  r.readUshort(size),
	}
	r.pos = size
	return value
}

func readEventGroupInfo(r *wireReader, size int) EventGroupInfo {
	size = r.readStructIntro(size)
	value := EventGroupInfo{
		GroupType:     r.readByte(size),
		EventDataList: readVector(r, size, readEventData),
	}
	r.pos = size
	return value
}

func readEventInfo(r *wireReader, size int) EventInfo {
	size = r.readStructIntro(size)
	value := EventInfo{
		Onid:       r.readUshort(size),
		Tsid:       r.readUshort(size),
		Sid:        r.readUshort(size),
		Eid:        r.readUshort(size),
		FreeCaFlag: 0,
	}

	startTimeFlag := r.readByte(size)
	startTime := r.readSystemTime(size)
	if startTimeFlag != 0 {
		value.StartTime = &startTime
	}

	durationFlag := r.readByte(size)
	durationSec := r.readInt(size)
	if durationFlag != 0 {
		value.DurationSec = &durationSec
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		shortInfo := readShortEventInfo(r, size)
		value.ShortInfo = &shortInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		extInfo := readExtendedEventInfo(r, size)
		value.ExtInfo = &extInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		contentInfo := readContentInfo(r, size)
		value.ContentInfo = &contentInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		componentInfo := readComponentInfo(r, size)
		value.ComponentInfo = &componentInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		audioInfo := readAudioComponentInfo(r, size)
		value.AudioInfo = &audioInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		eventGroupInfo := readEventGroupInfo(r, size)
		value.EventGroupInfo = &eventGroupInfo
	}

	if r.readInt(size) != 4 {
		r.pos -= 4
		eventRelayInfo := readEventGroupInfo(r, size)
		value.EventRelayInfo = &eventRelayInfo
	}

	value.FreeCaFlag = r.readByte(size)
	r.pos = size
	return value
}

func readServiceEventInfo(r *wireReader, size int) ServiceEventInfo {
	size = r.readStructIntro(size)
	value := ServiceEventInfo{
		ServiceInfo: readServiceInfo(r, size),
		EventList:   readVector(r, size, readEventInfo),
	}
	r.pos = size
	return value
}

func readSearchDateInfo(r *wireReader, size int) SearchDateInfo {
	size = r.readStructIntro(size)
	value := SearchDateInfo{
		StartDayOfWeek: r.readByte(size),
		StartHour:      r.readUshort(size),
		StartMin:       r.readUshort(size),
		EndDayOfWeek:   r.readByte(size),
		EndHour:        r.readUshort(size),
		EndMin:         r.readUshort(size),
	}
	r.pos = size
	return value
}

func readSearchKeyInfo(r *wireReader, size int) SearchKeyInfo {
	size = r.readStructIntro(size)
	andKey := r.readString(size)
	keyDisabled := len(andKey) >= 7 && andKey[:7] == "^!{999}"
	andKey = trimPrefix(andKey, "^!{999}")
	caseSensitive := len(andKey) >= 7 && andKey[:7] == "C!{999}"
	andKey = trimPrefix(andKey, "C!{999}")
	chkDurationMin := 0
	chkDurationMax := 0
	// Python 版と同じく 'D!{1########}' (13 文字) の形のときだけ番組長を取り出す
	if len(andKey) >= 13 && andKey[:4] == "D!{1" && andKey[12] == '}' && allDigits(andKey[4:12]) {
		chkDurationMax = atoiSafe(andKey[3:12])
		andKey = andKey[13:]
		chkDurationMin = chkDurationMax / 10000 % 10000
		chkDurationMax = chkDurationMax % 10000
	}
	value := SearchKeyInfo{
		AndKey:         andKey,
		NotKey:         r.readString(size),
		KeyDisabled:    keyDisabled,
		CaseSensitive:  caseSensitive,
		RegExpFlag:     r.readInt(size) != 0,
		TitleOnlyFlag:  r.readInt(size) != 0,
		ContentList:    readVector(r, size, readContentData),
		DateList:       readVector(r, size, readSearchDateInfo),
		ServiceList:    readVector(r, size, func(r *wireReader, size int) int64 { return r.readLong(size) }),
		VideoList:      readVector(r, size, func(r *wireReader, size int) int { return r.readUshort(size) }),
		AudioList:      readVector(r, size, func(r *wireReader, size int) int { return r.readUshort(size) }),
		AimaiFlag:      r.readByte(size) != 0,
		NotContetFlag:  r.readByte(size) != 0,
		NotDateFlag:    r.readByte(size) != 0,
		FreeCaFlag:     r.readByte(size),
		ChkRecEnd:      r.readByte(size) != 0,
		ChkDurationMin: chkDurationMin,
		ChkDurationMax: chkDurationMax,
	}
	chkRecDay := r.readUshort(size)
	if chkRecDay >= 40000 {
		value.ChkRecDay = chkRecDay % 10000
	} else {
		value.ChkRecDay = chkRecDay
	}
	value.ChkRecNoService = chkRecDay >= 40000
	r.pos = size
	return value
}

func readAutoAddData(r *wireReader, size int) AutoAddData {
	size = r.readStructIntro(size)
	value := AutoAddData{
		DataID:     r.readInt(size),
		SearchInfo: readSearchKeyInfo(r, size),
		RecSetting: readRecSettingData(r, size),
		AddCount:   r.readInt(size),
	}
	r.pos = size
	return value
}

func readNWPlayTimeShiftInfo(r *wireReader, size int) NWPlayTimeShiftInfo {
	size = r.readStructIntro(size)
	value := NWPlayTimeShiftInfo{
		CtrlID:   r.readInt(size),
		FilePath: r.readString(size),
	}
	r.pos = size
	return value
}

// ***** ライター *****

// wireWriter は EDCB へのリクエストバッファを組み立てるライター。
type wireWriter struct {
	buf []byte
}

func (w *wireWriter) writeByte(value int) {
	if value < 0 || value > 0xFF {
		panic(writeError{})
	}
	w.buf = append(w.buf, byte(value))
}

func (w *wireWriter) writeUshort(value int) {
	if value < 0 || value > 0xFFFF {
		panic(writeError{})
	}
	w.buf = append(w.buf, byte(value), byte(value>>8))
}

func (w *wireWriter) writeInt(value int) {
	if value < -0x80000000 || value > 0x7FFFFFFF {
		panic(writeError{})
	}
	w.buf = append(w.buf, byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
}

func (w *wireWriter) writeUint(value int) {
	if value < 0 || value > 0xFFFFFFFF {
		panic(writeError{})
	}
	w.buf = append(w.buf, byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
}

func (w *wireWriter) writeLong(value int64) {
	for index := 0; index < 8; index++ {
		w.buf = append(w.buf, byte(value>>(8*index)))
	}
}

func (w *wireWriter) writeIntInplace(position int, value int) {
	if value < -0x80000000 || value > 0x7FFFFFFF {
		panic(writeError{})
	}
	w.buf[position] = byte(value)
	w.buf[position+1] = byte(value >> 8)
	w.buf[position+2] = byte(value >> 16)
	w.buf[position+3] = byte(value >> 24)
}

// writeSystemTime は datetime を EDCB の SystemTime 構造体として書き出す。
func (w *wireWriter) writeSystemTime(value time.Time) {
	w.writeUshort(value.Year())
	w.writeUshort(int(value.Month()))
	w.writeUshort(pythonISODayOfWeek(value) % 7)
	w.writeUshort(value.Day())
	w.writeUshort(value.Hour())
	w.writeUshort(value.Minute())
	w.writeUshort(value.Second())
	w.writeUshort(0)
}

// pythonISODayOfWeek は Python の datetime.isoweekday() (月曜=1〜日曜=7) を返す。
func pythonISODayOfWeek(value time.Time) int {
	weekday := int(value.Weekday()) // Go: 日曜=0
	if weekday == 0 {
		return 7
	}
	return weekday
}

// writeString は長さプレフィックス付きの UTF-16LE 文字列を書き出す。
func (w *wireWriter) writeString(value string) {
	units := utf16.Encode([]rune(value))
	w.writeInt(6 + len(units)*2)
	for _, unit := range units {
		w.buf = append(w.buf, byte(unit), byte(unit>>8))
	}
	w.writeUshort(0)
}

// writeVector は長さプレフィックス付きの配列を書き出す。
func (w *wireWriter) writeVector(count int, write func()) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeInt(count)
	write()
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeRecFileSetInfo(w *wireWriter, value RecFileSetInfo) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeString(value.RecFolder)
	w.writeString(value.WritePlugIn)
	w.writeString(value.RecNamePlugIn)
	w.writeString("")
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeRecSettingData(w *wireWriter, value RecSettingData) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeByte(value.RecMode)
	w.writeByte(value.Priority)
	w.writeBoolByte(value.TuijyuuFlag)
	w.writeUint(value.ServiceMode)
	w.writeBoolByte(value.PittariFlag)
	w.writeString(value.BatFilePath)
	w.writeVector(len(value.RecFolderList), func() {
		for _, folder := range value.RecFolderList {
			writeRecFileSetInfo(w, folder)
		}
	})
	w.writeByte(value.SuspendMode)
	w.writeBoolByte(value.RebootFlag)
	// Python 版と同じく、開始・終了マージンの両方が存在するときだけフラグを立てる
	w.writeBoolByte(value.StartMargin != nil && value.EndMargin != nil)
	startMargin := 0
	if value.StartMargin != nil {
		startMargin = *value.StartMargin
	}
	endMargin := 0
	if value.EndMargin != nil {
		endMargin = *value.EndMargin
	}
	w.writeInt(startMargin)
	w.writeInt(endMargin)
	w.writeBoolByte(value.ContinueRec)
	w.writeByte(value.PartialRecFlag)
	w.writeUint(value.TunerID)
	w.writeVector(len(value.PartialRecFolder), func() {
		for _, folder := range value.PartialRecFolder {
			writeRecFileSetInfo(w, folder)
		}
	})
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeReserveData(w *wireWriter, value ReserveData) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeString(value.Title)
	w.writeSystemTime(value.StartTime)
	w.writeUint(value.DurationSecond)
	w.writeString(value.StationName)
	w.writeUshort(value.Onid)
	w.writeUshort(value.Tsid)
	w.writeUshort(value.Sid)
	w.writeUshort(value.Eid)
	w.writeString(value.Comment)
	w.writeInt(value.ReserveID)
	w.writeByte(0)
	w.writeByte(value.OverlapMode)
	w.writeString("")
	w.writeSystemTime(value.StartTimeEPG)
	writeRecSettingData(w, value.RecSetting)
	w.writeInt(0)
	w.writeVector(len(value.RecFileNameList), func() {
		for _, name := range value.RecFileNameList {
			w.writeString(name)
		}
	})
	w.writeInt(0)
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeContentData(w *wireWriter, value ContentData) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeUshort((value.ContentNibble>>8 | value.ContentNibble<<8) & 0xFFFF)
	w.writeUshort((value.UserNibble>>8 | value.UserNibble<<8) & 0xFFFF)
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeSearchDateInfo(w *wireWriter, value SearchDateInfo) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeByte(value.StartDayOfWeek)
	w.writeUshort(value.StartHour)
	w.writeUshort(value.StartMin)
	w.writeByte(value.EndDayOfWeek)
	w.writeUshort(value.EndHour)
	w.writeUshort(value.EndMin)
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeSearchKeyInfo(w *wireWriter, value SearchKeyInfo, hasChkRecEnd bool) {
	position := len(w.buf)
	w.writeInt(0)
	// Python int の剰余は非負。乗算前に bounded 化して MaxInt64 も落とさない。
	minimum := value.ChkDurationMin % 10000
	maximum := value.ChkDurationMax % 100000000
	chkDuration := (minimum*10000 + maximum) % 100000000
	if chkDuration < 0 {
		chkDuration += 100000000
	}
	andKey := ""
	if value.KeyDisabled {
		andKey += "^!{999}"
	}
	if value.CaseSensitive {
		andKey += "C!{999}"
	}
	if chkDuration > 0 {
		andKey += "D!{1" + zeroPad(chkDuration, 8) + "}"
	}
	andKey += value.AndKey
	w.writeString(andKey)
	w.writeString(value.NotKey)
	w.writeBoolInt(value.RegExpFlag)
	w.writeBoolInt(value.TitleOnlyFlag)
	w.writeVector(len(value.ContentList), func() {
		for _, content := range value.ContentList {
			writeContentData(w, content)
		}
	})
	w.writeVector(len(value.DateList), func() {
		for _, date := range value.DateList {
			writeSearchDateInfo(w, date)
		}
	})
	w.writeVector(len(value.ServiceList), func() {
		for _, service := range value.ServiceList {
			w.writeLong(service)
		}
	})
	w.writeVector(len(value.VideoList), func() {
		for _, video := range value.VideoList {
			w.writeUshort(video)
		}
	})
	w.writeVector(len(value.AudioList), func() {
		for _, audio := range value.AudioList {
			w.writeUshort(audio)
		}
	})
	w.writeBoolByte(value.AimaiFlag)
	w.writeBoolByte(value.NotContetFlag)
	w.writeBoolByte(value.NotDateFlag)
	w.writeByte(value.FreeCaFlag)
	if hasChkRecEnd {
		w.writeBoolByte(value.ChkRecEnd)
		chkRecDay := value.ChkRecDay
		if value.ChkRecNoService {
			chkRecDay = chkRecDay%10000 + 40000
		}
		w.writeUshort(chkRecDay)
	}
	w.writeIntInplace(position, len(w.buf)-position)
}

func writeAutoAddData(w *wireWriter, value AutoAddData) {
	position := len(w.buf)
	w.writeInt(0)
	w.writeInt(value.DataID)
	writeSearchKeyInfo(w, value.SearchInfo, true)
	writeRecSettingData(w, value.RecSetting)
	w.writeInt(value.AddCount)
	w.writeIntInplace(position, len(w.buf)-position)
}

func (w *wireWriter) writeBoolByte(value bool) {
	if value {
		w.writeByte(1)
		return
	}
	w.writeByte(0)
}

func (w *wireWriter) writeBoolInt(value bool) {
	if value {
		w.writeInt(1)
		return
	}
	w.writeInt(0)
}

// zeroPad は Python の f'{value:0{width}d}' 相当。
func zeroPad(value int, width int) string {
	text := itoa(value)
	for len(text) < width {
		text = "0" + text
	}
	return text
}

// itoa は負数にも対応した簡易整数→文字列変換。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	digits := []byte{}
	if negative {
		value = -value
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// atoiSafe は数字だけからなる文字列を整数に変換する (前提条件は呼び出し側で検証済み) 。
func atoiSafe(text string) int {
	value := 0
	for index := 0; index < len(text); index++ {
		value = value*10 + int(text[index]-'0')
	}
	return value
}

// allDigits は文字列が数字だけからなるかどうかを返す。
func allDigits(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return false
		}
	}
	return true
}

// trimPrefix は Python の str.removeprefix() 相当。
func trimPrefix(value string, prefix string) string {
	if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return value
}
