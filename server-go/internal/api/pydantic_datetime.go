package api

import (
	"math"
	"strconv"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// このファイルは Pydantic v2 (pydantic-core + speedate) の datetime | None クエリ
// パラメータの受理規則とエラー分類を再現する。
//
// pydantic は文字列を次の順で解釈する (pydantic-core の DateTimeValidator) 。
//  1. speedate の DateTime パース (RFC3339 の日時、または Unix タイムスタンプ)
//  2. 失敗したら speedate の Date パース (RFC3339 の日付、または Unix タイムスタンプ) を行い、
//     成功すれば 0 時 0 分 0 秒の datetime として受理する
//  3. どちらも失敗したら、Date パス側のエラー文言を ctx.error に載せ、
//     エラータイプ "datetime_from_date_parsing" として報告する
//
// そのため 422 の ctx.error は常に「Date パス」のエラー文言になる。実測は録画機の
// Python (pydantic 2.13.4) で行い、その値と文言をそのまま期待値として固定している。
//
// 注意: speedate の受理規則は ISO8601 ではなく独自仕様である。数字だけの入力
// (例 "2026" / "20261009100000") は年や日付ではなく Unix タイムスタンプとして
// 解釈される (実測: "2026" -> 1970-01-01T00:33:46Z, "20261009100000" -> 2612-01-18T10:05:00Z) 。

// speedate の ParseError::get_documentation() が返す文言。Python はこれを
// "Input should be a valid datetime or date, <文言>" の形でそのまま返す。
const (
	speedateTooShort        = "input is too short"
	speedateExtraCharacters = "unexpected extra characters at the end of the input"
	speedateInvalidDateSep  = "invalid date separator, expected `-`"
	speedateInvalidYear     = "invalid character in year"
	speedateInvalidMonth    = "invalid character in month"
	speedateInvalidDay      = "invalid character in day"
	speedateOutOfRangeMonth = "month value is outside expected range of 1-12"
	speedateOutOfRangeDay   = "day value is outside expected range"
	speedateDateTooSmall    = "dates before 0000 are not supported as unix timestamps"
	speedateDateTooLarge    = "dates after 9999 are not supported as unix timestamps"

	// Python の datetime が表現できる年の下限は 1 のため、0000 年は別種のエラーになる。
	pydanticYearOutOfRange = "year 0 is out of range"
)

const (
	// Unix タイムスタンプとして許可される範囲 (0000-01-01T00:00:00 .. 9999-12-31T23:59:59) 。
	unixTimestampMinSeconds = -62167219200
	unixTimestampMaxSeconds = 253402300799

	// この絶対値を超えるタイムスタンプはミリ秒として解釈する (speedate の MS_WATERSHED) 。
	// 実測: "1600000000" は秒 (2020 年) 、"20261009100000" はミリ秒 (2612 年) 。
	speedateMillisecondWatershed = 20000000000
)

// pydanticDatetimeError は datetime の検証エラー情報。
type pydanticDatetimeError struct {
	// Type は Pydantic のエラータイプ。通常は "datetime_from_date_parsing"、
	// 年 0 のときだけ "datetime_parsing" になる。
	Type string
	// Msg は ctx.error に載る文言。
	Msg string
}

// datetimeQueryDetail は datetime の検証エラーを validationDetail へ変換する。
// 年 0 のエラーだけはメッセージの前置きが異なる ("...or date," を付けない) 。
func datetimeQueryDetail(name string, raw string, datetimeErr *pydanticDatetimeError) validationDetail {
	prefix := "Input should be a valid datetime or date, "
	if datetimeErr.Type == "datetime_parsing" {
		prefix = "Input should be a valid datetime, "
	}
	return validationDetail{
		Type:  datetimeErr.Type,
		Loc:   []any{"query", name},
		Msg:   prefix + datetimeErr.Msg,
		Input: raw,
		Ctx:   &validationContext{Error: datetimeErr.Msg},
	}
}

// queryDatetime は datetime | None のクエリパラメータを検証する。
//
// 戻り値の 2 番目は「パラメータが指定され、かつ有効だった」かどうか。指定が無い場合は
// Python 側の既定値 (None) を使うため false を返す。不正な場合は v にエラーを積んで false を返す。
// Starlette と同じく同名パラメータが複数ある場合は最後の値を採用する。
func (v *fastapiValidation) queryDatetime(name string) (time.Time, bool) {
	values, ok := v.query[name]
	if !ok {
		return time.Time{}, false
	}
	raw := lastQueryValue(values)
	parsed, datetimeErr := parsePydanticDatetime(raw)
	if datetimeErr != nil {
		v.details = append(v.details, datetimeQueryDetail(name, raw, datetimeErr))
		return time.Time{}, false
	}
	return parsed, true
}

// parsePydanticDatetime は Pydantic v2 の datetime 文字列パースを再現する。
// 成功時は JST の time.Time を返す (タイムゾーン無しは JST として扱う) 。
func parsePydanticDatetime(raw string) (time.Time, *pydanticDatetimeError) {
	bytes := []byte(raw)

	// 1. DateTime パス (RFC3339 の日時)
	if parsed, ok := parseSpeedateDateTime(bytes); ok {
		return finalizeSpeedateDatetime(parsed)
	}

	// 1'. DateTime パスの Unix タイムスタンプ フォールバック
	number := parseSpeedateNumber(bytes)
	switch number.kind {
	case speedateNumberInt:
		if parsed, ok := speedateIntToTime(number.intValue); ok {
			return finalizeSpeedateDatetime(parsed)
		}
		// 範囲外の整数タイムスタンプ。Date パスも int_parse に成功するため
		// 同じ範囲エラーが報告される。
		return time.Time{}, &pydanticDatetimeError{
			Type: "datetime_from_date_parsing",
			Msg:  speedateTimestampRangeError(number.intValue),
		}
	case speedateNumberFloat:
		if parsed, ok := speedateFloatToTime(number.floatValue); ok {
			return finalizeSpeedateDatetime(parsed)
		}
		// 範囲外の小数タイムスタンプは Date パス (int_parse は '.' で失敗する) に
		// フォールスルーし、Date パスの RFC3339 エラーが報告される。
	}

	// 2. Date パス (RFC3339 の日付)
	if year, month, day, ok := parseSpeedateDateRFC3339(bytes); ok {
		return finalizeSpeedateDate(year, month, day)
	}

	// 3. Date パスのエラー文言を報告する
	return time.Time{}, &pydanticDatetimeError{
		Type: "datetime_from_date_parsing",
		Msg:  dateRFC3339Error(bytes),
	}
}

// finalizeSpeedateDatetime は DateTime パース結果を JST に正規化する。
// 年 0 は Python の datetime で表現できないため専用のエラーを返す。
func finalizeSpeedateDatetime(value time.Time) (time.Time, *pydanticDatetimeError) {
	if value.Year() == 0 {
		return time.Time{}, &pydanticDatetimeError{Type: "datetime_parsing", Msg: pydanticYearOutOfRange}
	}
	return value.In(constants.JST), nil
}

// finalizeSpeedateDate は Date パース結果を 0 時 0 分の JST datetime に変換する。
func finalizeSpeedateDate(year int, month int, day int) (time.Time, *pydanticDatetimeError) {
	if year == 0 {
		return time.Time{}, &pydanticDatetimeError{Type: "datetime_parsing", Msg: pydanticYearOutOfRange}
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, constants.JST), nil
}

// speedateNumberKind は speedate の数値パース結果の種類。
type speedateNumberKind int

const (
	speedateNumberErr speedateNumberKind = iota
	speedateNumberInt
	speedateNumberFloat
)

// speedateNumber は speedate の数値パース結果。
type speedateNumber struct {
	kind       speedateNumberKind
	intValue   int64
	floatValue float64
}

// parseSpeedateNumber は speedate の int/float パースを再現する。
// まず整数として解釈し、'.' を含む場合に限り float として解釈し直す。
// そのため "1e3" のような指数表記や前後の空白は受け付けない。
func parseSpeedateNumber(bytes []byte) speedateNumber {
	hasDot := false
	for _, char := range bytes {
		if char == '.' {
			hasDot = true
			break
		}
	}
	if !hasDot {
		value, err := strconv.ParseInt(string(bytes), 10, 64)
		if err != nil {
			return speedateNumber{kind: speedateNumberErr}
		}
		return speedateNumber{kind: speedateNumberInt, intValue: value}
	}
	// '.' を含む場合は speedate が float として再解釈する形式かを確認する
	if !isSpeedateFloatLiteral(bytes) {
		return speedateNumber{kind: speedateNumberErr}
	}
	value, err := strconv.ParseFloat(string(bytes), 64)
	if err != nil {
		return speedateNumber{kind: speedateNumberErr}
	}
	return speedateNumber{kind: speedateNumberFloat, floatValue: value}
}

// isSpeedateFloatLiteral は記号 + 数字 + 小数点 + 数字 の形 (指数表記なし) かどうかを返す。
func isSpeedateFloatLiteral(bytes []byte) bool {
	index := 0
	if index < len(bytes) && (bytes[index] == '+' || bytes[index] == '-') {
		index++
	}
	digits := 0
	dots := 0
	for ; index < len(bytes); index++ {
		switch {
		case bytes[index] >= '0' && bytes[index] <= '9':
			digits++
		case bytes[index] == '.':
			dots++
		default:
			return false
		}
	}
	return digits > 0 && dots == 1
}

// speedateTimestampWatershed は秒 / ミリ秒を判定しつつ秒とマイクロ秒へ分解する
// (speedate の timestamp_watershed と同じ) 。
func speedateTimestampWatershed(value int64) (int64, int64) {
	if value >= -speedateMillisecondWatershed && value <= speedateMillisecondWatershed {
		return value, 0
	}
	seconds := value / 1000
	microseconds := (value % 1000) * 1000
	if microseconds < 0 {
		seconds--
		microseconds += 1000000
	}
	return seconds, microseconds
}

// speedateIntToTime は整数タイムスタンプを UTC の time.Time に変換する。範囲外は ok=false。
func speedateIntToTime(value int64) (time.Time, bool) {
	seconds, microseconds := speedateTimestampWatershed(value)
	if seconds < unixTimestampMinSeconds || seconds > unixTimestampMaxSeconds {
		return time.Time{}, false
	}
	return time.Unix(seconds, microseconds*1000).UTC(), true
}

// speedateFloatToTime は小数タイムスタンプを UTC の time.Time に変換する。範囲外は ok=false。
func speedateFloatToTime(value float64) (time.Time, bool) {
	seconds := value
	if math.Abs(value) > speedateMillisecondWatershed {
		seconds = value / 1000
	}
	whole := int64(math.Floor(seconds))
	microseconds := int64(math.Round((seconds - float64(whole)) * 1e6))
	if microseconds >= 1000000 {
		whole++
		microseconds -= 1000000
	}
	if whole < unixTimestampMinSeconds || whole > unixTimestampMaxSeconds {
		return time.Time{}, false
	}
	return time.Unix(whole, microseconds*1000).UTC(), true
}

// speedateTimestampRangeError は範囲外タイムスタンプのエラー文言を返す。
func speedateTimestampRangeError(value int64) string {
	seconds, _ := speedateTimestampWatershed(value)
	if seconds < unixTimestampMinSeconds {
		return speedateDateTooSmall
	}
	return speedateDateTooLarge
}

// parseSpeedateDateTime は speedate の DateTime::parse_bytes_rfc3339 相当。
// "YYYY-MM-DD" + 区切り文字 (T/t/空白/_ のいずれか) + 時刻 (+ 任意のタイムゾーン) を受け付ける。
func parseSpeedateDateTime(bytes []byte) (time.Time, bool) {
	year, month, day, errMsg := parseDateDigits(bytes)
	if errMsg != "" {
		return time.Time{}, false
	}
	if len(bytes) < 11 {
		return time.Time{}, false
	}
	switch bytes[10] {
	case 'T', 't', ' ', '_':
	default:
		return time.Time{}, false
	}
	hour, minute, second, microsecond, position, ok := parseSpeedateTime(bytes, 11)
	if !ok {
		return time.Time{}, false
	}
	offset, end, ok := parseSpeedateTimezone(bytes, position)
	if !ok || end != len(bytes) {
		return time.Time{}, false
	}
	nanosecond := microsecond * 1000
	if offset == nil {
		return time.Date(year, time.Month(month), day, hour, minute, second, nanosecond, constants.JST), true
	}
	location := time.FixedZone("", *offset)
	return time.Date(year, time.Month(month), day, hour, minute, second, nanosecond, location).In(constants.JST), true
}

// parseSpeedateTime は speedate の PureTime パース相当 (オフセット位置から "HH:MM[:SS[.fff]]") 。
func parseSpeedateTime(bytes []byte, offset int) (hour int, minute int, second int, microsecond int, position int, ok bool) {
	if len(bytes)-offset < 5 {
		return 0, 0, 0, 0, 0, false
	}
	if !isASCIIDigit(bytes[offset]) || !isASCIIDigit(bytes[offset+1]) {
		return 0, 0, 0, 0, 0, false
	}
	hour = int(bytes[offset]-'0')*10 + int(bytes[offset+1]-'0')
	if bytes[offset+2] != ':' {
		return 0, 0, 0, 0, 0, false
	}
	if !isASCIIDigit(bytes[offset+3]) || !isASCIIDigit(bytes[offset+4]) {
		return 0, 0, 0, 0, 0, false
	}
	minute = int(bytes[offset+3]-'0')*10 + int(bytes[offset+4]-'0')
	if hour > 23 || minute > 59 {
		return 0, 0, 0, 0, 0, false
	}
	position = offset + 5
	if position < len(bytes) && bytes[position] == ':' {
		if position+2 >= len(bytes) || !isASCIIDigit(bytes[position+1]) || !isASCIIDigit(bytes[position+2]) {
			return 0, 0, 0, 0, 0, false
		}
		second = int(bytes[position+1]-'0')*10 + int(bytes[position+2]-'0')
		if second > 59 {
			return 0, 0, 0, 0, 0, false
		}
		position += 3
		// 小数秒。pydantic の既定 (microseconds_precision='truncate') では
		// 7 桁目以降を切り捨てるため、実測でも ".1234567" は受理される。
		if position < len(bytes) && (bytes[position] == '.' || bytes[position] == ',') {
			position++
			digits := 0
			for position < len(bytes) && isASCIIDigit(bytes[position]) {
				if digits < 6 {
					microsecond = microsecond*10 + int(bytes[position]-'0')
				}
				digits++
				position++
			}
			if digits == 0 {
				return 0, 0, 0, 0, 0, false
			}
			for ; digits < 6; digits++ {
				microsecond *= 10
			}
		}
	}
	return hour, minute, second, microsecond, position, true
}

// parseSpeedateTimezone は speedate のタイムゾーンパース相当。
// 戻り値の offset は nil のときタイムゾーン省略 (naive) を表す。
func parseSpeedateTimezone(bytes []byte, position int) (offset *int, end int, ok bool) {
	if position >= len(bytes) {
		return nil, position, true
	}
	next := bytes[position]
	position++
	if next == 'Z' || next == 'z' {
		zero := 0
		return &zero, position, true
	}
	sign := 0
	switch next {
	case '+':
		sign = 1
	case '-':
		sign = -1
	default:
		return nil, 0, false
	}
	if position+1 >= len(bytes) || !isASCIIDigit(bytes[position]) || !isASCIIDigit(bytes[position+1]) {
		return nil, 0, false
	}
	hour := int(bytes[position]-'0')*10 + int(bytes[position+1]-'0')
	position += 2
	var firstMinute int
	if position < len(bytes) && bytes[position] == ':' {
		position++
		if position >= len(bytes) || !isASCIIDigit(bytes[position]) {
			return nil, 0, false
		}
		firstMinute = int(bytes[position] - '0')
	} else if position < len(bytes) && isASCIIDigit(bytes[position]) {
		firstMinute = int(bytes[position] - '0')
	} else {
		return nil, 0, false
	}
	if position+1 >= len(bytes) || !isASCIIDigit(bytes[position+1]) {
		return nil, 0, false
	}
	minutes := firstMinute*10 + int(bytes[position+1]-'0')
	position += 2
	if minutes > 59 {
		return nil, 0, false
	}
	value := sign * (hour*3600 + minutes*60)
	if value >= 24*3600 || value <= -24*3600 {
		return nil, 0, false
	}
	return &value, position, true
}

// parseDateDigits は speedate の Date::parse_bytes_partial 相当。
// 先頭 10 バイトが "YYYY-MM-DD" として妥当なら errMsg は空になる。
func parseDateDigits(bytes []byte) (year int, month int, day int, errMsg string) {
	if len(bytes) < 10 {
		return 0, 0, 0, speedateTooShort
	}
	for index := 0; index < 4; index++ {
		if !isASCIIDigit(bytes[index]) {
			return 0, 0, 0, speedateInvalidYear
		}
	}
	if bytes[4] != '-' {
		return 0, 0, 0, speedateInvalidDateSep
	}
	if !isASCIIDigit(bytes[5]) || !isASCIIDigit(bytes[6]) {
		return 0, 0, 0, speedateInvalidMonth
	}
	if bytes[7] != '-' {
		return 0, 0, 0, speedateInvalidDateSep
	}
	if !isASCIIDigit(bytes[8]) || !isASCIIDigit(bytes[9]) {
		return 0, 0, 0, speedateInvalidDay
	}
	year = int(bytes[0]-'0')*1000 + int(bytes[1]-'0')*100 + int(bytes[2]-'0')*10 + int(bytes[3]-'0')
	month = int(bytes[5]-'0')*10 + int(bytes[6]-'0')
	day = int(bytes[8]-'0')*10 + int(bytes[9]-'0')
	maxDays := 0
	switch month {
	case 1, 3, 5, 7, 8, 10, 12:
		maxDays = 31
	case 4, 6, 9, 11:
		maxDays = 30
	case 2:
		if isLeapYear(year) {
			maxDays = 29
		} else {
			maxDays = 28
		}
	default:
		return 0, 0, 0, speedateOutOfRangeMonth
	}
	if day < 1 || day > maxDays {
		return 0, 0, 0, speedateOutOfRangeDay
	}
	return year, month, day, ""
}

// parseSpeedateDateRFC3339 は speedate の Date::parse_bytes_rfc3339 相当。
// parseDateDigits が成功し、かつ入力長がちょうど 10 バイトのときだけ成功する。
func parseSpeedateDateRFC3339(bytes []byte) (year int, month int, day int, ok bool) {
	year, month, day, errMsg := parseDateDigits(bytes)
	if errMsg != "" {
		return 0, 0, 0, false
	}
	if len(bytes) > 10 {
		return 0, 0, 0, false
	}
	return year, month, day, true
}

// dateRFC3339Error は Date パスで報告されるエラー文言を返す。
func dateRFC3339Error(bytes []byte) string {
	_, _, _, errMsg := parseDateDigits(bytes)
	if errMsg == "" {
		// 日付として妥当でも、長さが 10 バイトを超える場合は
		// Date::parse_bytes_rfc3339 の後段チェックで extra characters になる。
		return speedateExtraCharacters
	}
	return errMsg
}

// isASCIIDigit は ASCII の数字かどうかを返す。
func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

// isLeapYear はグレゴリオ暦のうるう年判定を返す。
func isLeapYear(year int) bool {
	if year%100 == 0 {
		return year%400 == 0
	}
	return year%4 == 0
}
