package api

import (
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// searchValidationError は Pydantic のエラー型と本文を保持する。
type searchValidationError struct{ kind, message string }

func (e searchValidationError) Error() string          { return e.message }
func (e searchValidationError) ValidationType() string { return e.kind }

// searchJSONBool は Pydantic の非 strict bool 変換を実装する。
func searchJSONBool(raw json.RawMessage) (bool, error) {
	invalid := searchValidationError{"bool_type", "Input should be a valid boolean"}
	parsing := searchValidationError{"bool_parsing", "Input should be a valid boolean, unable to interpret input"}
	if string(raw) == "true" {
		return true, nil
	}
	if string(raw) == "false" {
		return false, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && string(raw) != "null" {
		switch strings.ToLower(text) {
		case "1", "true", "t", "yes", "y", "on":
			return true, nil
		case "0", "false", "f", "no", "n", "off":
			return false, nil
		}
		return false, parsing
	}
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value >= 9223372036854775808.0 || value < -9223372036854775808.0 {
		return false, invalid
	}
	if value == 0 {
		return false, nil
	}
	if value == 1 {
		return true, nil
	}
	return false, parsing
}

// searchIntegerText は許容される十進整数文字列 (ゼロ小数部・桁区切り) の文法。
var searchIntegerText = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*(?:\.0+)?$`)

// searchJSONInteger は精度を落とさず Python int を読み取る。
// JSON float の場合だけ Python decoder と同じ binary64 に丸める。
func searchJSONInteger(raw json.RawMessage) (*big.Int, error) {
	invalid := searchValidationError{"int_type", "Input should be a valid integer"}
	parsing := searchValidationError{"int_parsing", "Input should be a valid integer, unable to parse string as an integer"}
	if string(raw) == "true" {
		return big.NewInt(1), nil
	}
	if string(raw) == "false" {
		return big.NewInt(0), nil
	}
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return nil, parsing
		}
		text = strings.TrimSpace(text)
		if !searchIntegerText.MatchString(text) {
			return nil, parsing
		}
		text = strings.ReplaceAll(strings.Split(text, ".")[0], "_", "")
	} else {
		text = string(raw)
		if strings.ContainsAny(text, ".eE") {
			value, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, invalid
			}
			if math.Trunc(value) != value {
				return nil, searchValidationError{"int_from_float", "Input should be a valid integer, got a number with a fractional part"}
			}
			result, _ := new(big.Float).SetFloat64(value).Int(nil)
			return result, nil
		}
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, invalid
	}
	return value, nil
}

// normalizeSearchIntegers は任意精度値を別に保持し、既存の int consumer と共存する。
// overflow 値を reject / wrap せず、wire 境界に至るまで正確な整数を保持する。
func normalizeSearchIntegers(fields map[string]json.RawMessage, names []string, nullable bool) (map[string]string, error) {
	large := map[string]string{}
	for _, name := range names {
		raw, exists := fields[name]
		if !exists {
			continue
		}
		if nullable && string(raw) == "null" {
			continue
		}
		value, err := searchJSONInteger(raw)
		if err != nil {
			return nil, err
		}
		if value.IsInt64() {
			fields[name] = json.RawMessage(value.String())
		} else {
			large[name] = value.String()
			fields[name] = json.RawMessage("0")
		}
	}
	return large, nil
}

// MarshalJSON は任意精度整数を含む条件でも Python と同じ JSON 数値を返す。
func (condition programSearchCondition) MarshalJSON() ([]byte, error) {
	type plain programSearchCondition
	data, err := json.Marshal(plain(condition))
	if err != nil || len(condition.largeIntegers) == 0 {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for name, text := range condition.largeIntegers {
		fields[name] = json.RawMessage(text)
	}
	return json.Marshal(fields)
}

// MarshalJSON はサービスの任意精度整数をそのまま出力する。
func (service programSearchConditionServiceSchema) MarshalJSON() ([]byte, error) {
	type plain programSearchConditionServiceSchema
	data, err := json.Marshal(plain(service))
	if err != nil {
		return nil, err
	}
	// 通常値は親子 schema の既存フィールド順をそのまま維持する。
	if service.largeNetworkID == "" && service.largeTransportStreamID == "" && service.largeServiceID == "" {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for name, text := range map[string]string{"network_id": service.largeNetworkID, "transport_stream_id": service.largeTransportStreamID, "service_id": service.largeServiceID} {
		if text != "" {
			fields[name] = json.RawMessage(text)
		}
	}
	return json.Marshal(fields)
}
