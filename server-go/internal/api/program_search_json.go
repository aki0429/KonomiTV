package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// searchJSONObject は検索モデルの省略と null、必須子フィールドを区別する。
// 型変換は各モデルで実施し、この段階ではモデルの存在契約だけを検証する。
func searchJSONObject(data []byte, required, nonNullable []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("Input should be a valid dictionary")
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return nil, fmt.Errorf("Field required: %s", name)
		}
	}
	for _, name := range nonNullable {
		if value, exists := fields[name]; exists && string(value) == "null" {
			return nil, fmt.Errorf("Input cannot be null: %s", name)
		}
	}
	return fields, nil
}

// UnmarshalJSON は省略時の既定値と明示的 null の拒否を区別する。
func (request *programSearchConditionRequest) UnmarshalJSON(data []byte) error {
	fields, err := searchJSONObject(data, nil, []string{"keyword", "exclude_keyword", "note", "broadcast_type", "duplicate_title_check_scope"})
	if err != nil {
		return err
	}
	// bool / int の null は coercion の型エラーとして判定する。
	for _, name := range []string{"is_enabled", "is_title_only", "is_case_sensitive", "is_fuzzy_search_enabled", "is_regex_search_enabled", "is_exclude_genre_ranges", "is_exclude_date_ranges"} {
		if raw, exists := fields[name]; exists {
			value, err := searchJSONBool(raw)
			if err != nil {
				return err
			}
			fields[name], _ = json.Marshal(value)
		}
	}
	large, err := normalizeSearchIntegers(fields, []string{"duration_range_min", "duration_range_max"}, true)
	if err != nil {
		return err
	}
	days, err := normalizeSearchIntegers(fields, []string{"duplicate_title_check_period_days"}, false)
	if err != nil {
		return err
	}
	for name, value := range days {
		large[name] = value
	}
	normalized, _ := json.Marshal(fields)
	type plain programSearchConditionRequest
	var decoded plain
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*request = programSearchConditionRequest(decoded)
	request.largeIntegers = large
	return nil
}

// UnmarshalJSON はサービス三つ組の全フィールドを必須として検証する。
func (service *programSearchConditionServiceSchema) UnmarshalJSON(data []byte) error {
	names := []string{"network_id", "transport_stream_id", "service_id"}
	fields, err := searchJSONObject(data, names, nil)
	if err != nil {
		return err
	}
	large, err := normalizeSearchIntegers(fields, names, false)
	if err != nil {
		return err
	}
	normalized, _ := json.Marshal(fields)
	type plain programSearchConditionServiceSchema
	var decoded plain
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*service = programSearchConditionServiceSchema(decoded)
	service.largeNetworkID = large["network_id"]
	service.largeTransportStreamID = large["transport_stream_id"]
	service.largeServiceID = large["service_id"]
	return nil
}

// UnmarshalJSON は日付範囲の全フィールドを必須として検証する。
func (date *programSearchConditionDateSchema) UnmarshalJSON(data []byte) error {
	names := []string{"start_day_of_week", "start_hour", "start_minute", "end_day_of_week", "end_hour", "end_minute"}
	fields, err := searchJSONObject(data, names, nil)
	if err != nil {
		return err
	}
	large, err := normalizeSearchIntegers(fields, names, false)
	if err != nil {
		return err
	}
	// 日付には上限があるため machine int overflow ではなく schema 範囲エラーにする。
	for _, name := range names {
		if text, exists := large[name]; exists {
			value, _ := new(big.Int).SetString(text, 10)
			if value.Sign() < 0 {
				return fmt.Errorf("Input should be greater than or equal to 0")
			}
			limit := 59
			if strings.Contains(name, "day_of_week") {
				limit = 6
			} else if strings.Contains(name, "hour") {
				limit = 23
			}
			return fmt.Errorf("Input should be less than or equal to %d", limit)
		}
	}
	normalized, _ := json.Marshal(fields)
	type plain programSearchConditionDateSchema
	return json.Unmarshal(normalized, (*plain)(date))
}

// UnmarshalJSON は Genre TypedDict の必須文字列を検証する。
func (genre *genreSchema) UnmarshalJSON(data []byte) error {
	names := []string{"major", "middle"}
	if _, err := searchJSONObject(data, names, names); err != nil {
		return err
	}
	type plain genreSchema
	return json.Unmarshal(data, (*plain)(genre))
}
