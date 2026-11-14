package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// SettingsValidationError は FastAPI が返す設定項目一件の検証エラー。
type SettingsValidationError struct {
	Type  string         `json:"type"`
	Loc   []any          `json:"loc"`
	Msg   string         `json:"msg"`
	Input any            `json:"input"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

//go:embed settings_validation_schema.json
var settingsValidationSchemaJSON []byte

type settingsFieldSchema struct {
	Type                 string                          `json:"type"`
	Format               string                          `json:"format"`
	Title                string                          `json:"title"`
	Ref                  string                          `json:"$ref"`
	Properties           map[string]*settingsFieldSchema `json:"properties"`
	Definitions          map[string]*settingsFieldSchema `json:"$defs"`
	Default              json.RawMessage                 `json:"default"`
	Enum                 []string                        `json:"enum"`
	Items                *settingsFieldSchema            `json:"items"`
	AdditionalProperties json.RawMessage                 `json:"additionalProperties"`
	AnyOf                []*settingsFieldSchema          `json:"anyOf"`
	Strict               bool                            `json:"x-strict"`
	AllowInfNan          *bool                           `json:"x-allow-inf-nan"`
	Minimum              *float64                        `json:"minimum"`
	Maximum              *float64                        `json:"maximum"`
	ExclusiveMinimum     *float64                        `json:"exclusiveMinimum"`
}

type settingsModelSchema struct {
	Schema     *settingsFieldSchema `json:"schema"`
	FieldOrder map[string][]string  `json:"field_order"`
}

var settingsValidationModels = func() map[string]settingsModelSchema {
	var document struct {
		Models map[string]settingsModelSchema `json:"models"`
	}
	if err := json.Unmarshal(settingsValidationSchemaJSON, &document); err != nil {
		panic("failed to load settings validation schema: " + err.Error())
	}
	return document.Models
}()

type settingsValidator struct {
	model  settingsModelSchema
	order  map[string][]string
	errors []SettingsValidationError
	server *serverSettingsValidation
	fatal  error
}

// settingsOrderedObject はユーザーが指定した辞書の挿入順を保存・再取得後も保持する。
// モデル自体のフィールド順とは異なり、ジャンル色や履歴内の辞書を既定値の順へ並べ替えない。
type settingsOrderedObject struct {
	values map[string]any
	keys   []string
}

// settingsResponseFloat は Pydantic の応答表現を DB 保存用の Python JSON 表現と区別する。
type settingsResponseFloat float64

// MarshalClientSettingsResponse は応答モデルと同じ非有限数の null 化・浮動小数点表現を適用する。
func MarshalClientSettingsResponse(settings map[string]any) (string, error) {
	return encodePythonJSON(settingsResponseValue(settings), clientSettingsKeyOrder, "")
}

func settingsResponseValue(input any) any {
	switch value := input.(type) {
	case *string:
		if value == nil {
			return nil
		}
		return *value
	case []string:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = item
		}
		return result
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		return settingsResponseFloat(value)
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			number, _ := value.Float64()
			return settingsResponseValue(number)
		}
		return value
	case settingsOrderedObject:
		result := settingsOrderedObject{keys: value.keys, values: map[string]any{}}
		for key, item := range value.values {
			result.values[key] = settingsResponseValue(item)
		}
		return result
	case map[string]any:
		result := map[string]any{}
		for key, item := range value {
			result[key] = settingsResponseValue(item)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = settingsResponseValue(item)
		}
		return result
	default:
		return input
	}
}

func formatSettingsResponseFloat(value float64) string {
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.Split(scientific, "e")
	exponent, _ := strconv.Atoi(parts[1])
	if exponent >= -5 && exponent < 16 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	if exponent >= 0 {
		return parts[0] + "e+" + strconv.Itoa(exponent)
	}
	return parts[0] + "e" + strconv.Itoa(exponent)
}

func (object settingsOrderedObject) MarshalJSON() ([]byte, error) {
	data, err := encodePythonJSON(object, nil, "")
	return []byte(data), err
}

func (v *settingsValidator) orderedValue(input any, path []any) any {
	switch value := input.(type) {
	case map[string]any:
		result := settingsOrderedObject{values: map[string]any{}, keys: append([]string{}, v.order[settingsPathKey(path)]...)}
		for key, item := range value {
			result.values[key] = v.orderedValue(item, settingsChild(path, key))
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = v.orderedValue(item, settingsChild(path, i))
		}
		return result
	default:
		return input
	}
}

// ValidateClientSettingsJSON は本家 ClientSettings の全フィールドを宣言順に検証する。
// 未指定項目には既定値を入れ、未知の項目は破棄する。誤った値は保存せず全エラーを返す。
func ValidateClientSettingsJSON(raw []byte) (map[string]any, []SettingsValidationError) {
	body, details := parseSettingsJSON(raw)
	if len(details) > 0 {
		return nil, details
	}
	validator := settingsValidator{model: settingsValidationModels["ClientSettings"], order: body.order}
	return body.validate(&validator)
}

// SettingsBody は認証前の本文デコードと、認証後の設定検証を分離する。
// 辞書順序を含むデコード結果を保持し、外部機器の検査を未認証で実行しない。
type SettingsBody struct {
	input   any
	order   map[string][]string
	missing bool
}

func parseSettingsJSON(raw []byte) (*SettingsBody, []SettingsValidationError) {
	parser := settingsJSONParser{data: string(raw), order: map[string][]string{}}
	if len(raw) == 0 {
		return &SettingsBody{missing: true}, nil
	}
	input := parser.value([]any{"body"})
	parser.space()
	if parser.err == nil && parser.pos < len(parser.data) {
		parser.fail("Extra data", parser.pos)
	}
	if parser.err != nil {
		return nil, []SettingsValidationError{*parser.err}
	}
	return &SettingsBody{input: input, order: parser.order}, nil
}

func (body *SettingsBody) validate(validator *settingsValidator) (map[string]any, []SettingsValidationError) {
	if body.missing {
		return nil, []SettingsValidationError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	result := validator.value(body.input, validator.model.Schema, []any{"body"})
	if len(validator.errors) > 0 || validator.fatal != nil {
		return nil, validator.errors
	}
	return result.(map[string]any), nil
}

func (v *settingsValidator) add(kind, message string, path []any, input any, context map[string]any) {
	v.errors = append(v.errors, SettingsValidationError{Type: kind, Loc: path, Msg: message, Input: v.orderedValue(input, path), Ctx: context})
}

func settingLiteralExpected(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "'" + value + "'"
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

func (v *settingsValidator) value(input any, schema *settingsFieldSchema, path []any) any {
	if v.fatal != nil {
		return nil
	}
	if schema.Ref != "" {
		return v.value(input, v.model.Schema.Definitions[strings.TrimPrefix(schema.Ref, "#/$defs/")], path)
	}
	if len(schema.AnyOf) > 0 {
		for _, option := range schema.AnyOf {
			if option.Type == "null" && input == nil {
				return nil
			}
		}
		for _, option := range schema.AnyOf {
			if option.Type != "null" {
				return v.value(input, option, path)
			}
		}
	}
	if len(schema.Enum) > 0 {
		text, ok := input.(string)
		if ok && text == "IPTV" && v.server != nil && v.server.options.AllowLegacyIPTV && len(path) == 3 && path[1] == "general" && path[2] == "backend" {
			return text
		}
		if ok && containsString(schema.Enum, text) {
			return text
		}
		expected := settingLiteralExpected(schema.Enum)
		v.add("literal_error", "Input should be "+expected, path, input, map[string]any{"expected": expected})
		return nil
	}
	if schema.Format != "" && v.server != nil {
		return v.server.formattedValue(v, input, schema, path)
	}
	switch schema.Type {
	case "object":
		object, ok := input.(map[string]any)
		if !ok {
			if schema.Properties != nil {
				v.add("model_attributes_type", "Input should be a valid dictionary or object to extract fields from", path, input, nil)
			} else {
				v.add("dict_type", "Input should be a valid dictionary", path, input, nil)
			}
			return nil
		}
		result := map[string]any{}
		if schema.Properties != nil {
			for _, name := range v.model.FieldOrder[schema.Title] {
				field := schema.Properties[name]
				if value, exists := object[name]; exists {
					fieldPath := settingsChild(path, name)
					before := len(v.errors)
					if v.server != nil && !v.server.beforeField(v, value, fieldPath) {
						continue
					}
					normalized := v.value(value, field, fieldPath)
					if len(v.errors) != before || v.fatal != nil {
						continue
					}
					if v.server != nil {
						normalized = v.server.afterField(v, value, normalized, fieldPath, result)
					}
					if len(v.errors) == before && v.fatal == nil {
						result[name] = normalized
					}
				} else if field.Default != nil {
					decoder := json.NewDecoder(strings.NewReader(string(field.Default)))
					decoder.UseNumber()
					var value any
					_ = decoder.Decode(&value)
					result[name] = value
				} else {
					v.add("missing", "Field required", settingsChild(path, name), object, nil)
				}
			}
		} else {
			var element settingsFieldSchema
			if len(schema.AdditionalProperties) > 0 && schema.AdditionalProperties[0] == '{' {
				_ = json.Unmarshal(schema.AdditionalProperties, &element)
			}
			for _, key := range v.order[settingsPathKey(path)] {
				result[key] = v.value(object[key], &element, settingsChild(path, key))
			}
			return settingsOrderedObject{values: result, keys: append([]string{}, v.order[settingsPathKey(path)]...)}
		}
		return result
	case "array":
		array, ok := input.([]any)
		if !ok {
			v.add("list_type", "Input should be a valid list", path, input, nil)
			return nil
		}
		result := make([]any, len(array))
		for i, item := range array {
			result[i] = v.value(item, schema.Items, settingsChild(path, i))
		}
		return result
	case "string":
		if text, ok := settingsText(input); ok {
			return text
		}
		v.add("string_type", "Input should be a valid string", path, input, nil)
		return nil
	case "boolean":
		if boolean, ok := input.(bool); ok {
			return boolean
		}
		kind, message := "bool_type", "Input should be a valid boolean"
		if text, ok := settingsText(input); ok {
			switch strings.ToLower(text) {
			case "1", "true", "t", "yes", "y", "on":
				return true
			case "0", "false", "f", "no", "n", "off":
				return false
			}
			kind, message = "bool_parsing", "Input should be a valid boolean, unable to interpret input"
		} else if number, valid := settingsFloat(input, false); valid && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number && number >= -9223372036854775808.0 && number < 9223372036854775808.0 {
			if number == 0 || number == 1 {
				return number == 1
			}
			kind, message = "bool_parsing", "Input should be a valid boolean, unable to interpret input"
		}
		v.add(kind, message, path, input, nil)
		return nil
	case "number":
		number, ok := settingsFloat(input, !schema.Strict)
		if !ok {
			kind, message := "float_type", "Input should be a valid number"
			if _, text := settingsText(input); text && !schema.Strict {
				kind, message = "float_parsing", "Input should be a valid number, unable to parse string as a number"
			}
			v.add(kind, message, path, input, nil)
			return nil
		}
		if schema.AllowInfNan != nil && !*schema.AllowInfNan && (math.IsInf(number, 0) || math.IsNaN(number)) {
			v.add("finite_number", "Input should be a finite number", path, input, nil)
			return nil
		}
		v.numberBounds(number, schema, path, input)
		return number
	case "integer":
		number, kind := settingsInteger(input)
		if kind != "" {
			message := map[string]string{"int_type": "Input should be a valid integer", "int_parsing": "Input should be a valid integer, unable to parse string as an integer", "int_from_float": "Input should be a valid integer, got a number with a fractional part", "int_parsing_size": "Unable to parse input string as an integer, exceeded maximum size", "finite_number": "Input should be a finite number"}[kind]
			v.add(kind, message, path, input, nil)
			return nil
		}
		asFloat, _ := new(big.Float).SetInt(number).Float64()
		v.numberBounds(asFloat, schema, path, input)
		return json.Number(number.String())
	default:
		// list[dict[str, Any]] の値は、配列・辞書・数値も変換せずに保持する。
		return v.orderedValue(input, path)
	}
}

func (v *settingsValidator) numberBounds(number float64, schema *settingsFieldSchema, path []any, input any) {
	for _, bound := range []struct {
		value              *float64
		key, kind, message string
		invalid            func(float64) bool
	}{
		{schema.ExclusiveMinimum, "gt", "greater_than", "Input should be greater than ", func(limit float64) bool { return !(number > limit) }},
		{schema.Minimum, "ge", "greater_than_equal", "Input should be greater than or equal to ", func(limit float64) bool { return !(number >= limit) }},
		{schema.Maximum, "le", "less_than_equal", "Input should be less than or equal to ", func(limit float64) bool { return !(number <= limit) }},
	} {
		if bound.value != nil && bound.invalid(*bound.value) {
			v.add(bound.kind, bound.message+strconv.FormatFloat(*bound.value, 'g', -1, 64), path, input, map[string]any{bound.key: *bound.value})
			return
		}
	}
}

var settingsFloatText = regexp.MustCompile(`(?i)^[+-]?(?:(?:[0-9](?:_?[0-9])*)(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:e[+-]?[0-9](?:_?[0-9])*)?$`)
var settingsIntegerText = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*(?:\.0+)?$`)

func settingsFloat(input any, coerce bool) (float64, bool) {
	switch value := input.(type) {
	case settingsYAMLBinary:
		if coerce {
			return settingsFloat(string(value), true)
		}
	case float64:
		return value, true
	case json.Number:
		number, err := strconv.ParseFloat(value.String(), 64)
		return number, err == nil || math.IsInf(number, 0)
	case bool:
		if coerce {
			if value {
				return 1, true
			}
			return 0, true
		}
	case string:
		if !coerce {
			return 0, false
		}
		text := strings.TrimSpace(value)
		switch strings.ToLower(text) {
		case "nan", "+nan", "-nan":
			return math.NaN(), true
		case "inf", "+inf", "infinity", "+infinity":
			return math.Inf(1), true
		case "-inf", "-infinity":
			return math.Inf(-1), true
		}
		if settingsFloatText.MatchString(text) {
			number, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
			return number, err == nil || math.IsInf(number, 0)
		}
	}
	return 0, false
}

func settingsInteger(input any) (*big.Int, string) {
	switch value := input.(type) {
	case settingsYAMLBinary:
		return settingsInteger(string(value))
	case bool:
		if value {
			return big.NewInt(1), ""
		}
		return big.NewInt(0), ""
	case json.Number:
		if number, ok := new(big.Int).SetString(value.String(), 10); ok {
			return number, ""
		}
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, "finite_number"
		}
		if math.Trunc(value) != value {
			return nil, "int_from_float"
		}
		number, _ := new(big.Float).SetFloat64(value).Int(nil)
		return number, ""
	case string:
		text := strings.TrimSpace(value)
		if len(text) > 4300 {
			return nil, "int_parsing_size"
		}
		if !settingsIntegerText.MatchString(text) {
			return nil, "int_parsing"
		}
		text = strings.ReplaceAll(strings.Split(text, ".")[0], "_", "")
		if number, ok := new(big.Int).SetString(text, 10); ok {
			return number, ""
		}
	}
	return nil, "int_type"
}

// SettingsValidationJSON は API 側で本家と同じ detail 配列を出力するための JSON を生成する。
func SettingsValidationJSON(details []SettingsValidationError) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(struct {
		Detail []SettingsValidationError `json:"detail"`
	}{details})
	if err != nil {
		return nil, fmt.Errorf("failed to encode settings validation errors: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
