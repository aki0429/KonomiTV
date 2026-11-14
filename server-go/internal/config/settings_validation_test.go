package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestClientSettingsValidationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/client_settings_validation_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Defaults map[string]any
		Cases    []struct {
			Name, Input string
			FullJSON    string `json:"normalized_full_json"`
			MIME        string `json:"content_type"`
			Base64      string `json:"input_base64"`
			Status      int
			Normalized  map[string]any
			Errors      []SettingsValidationError
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			input := []byte(tc.Input)
			if tc.Base64 != "" {
				input, _ = base64.StdEncoding.DecodeString(tc.Base64)
			}
			actual, details := ValidateClientSettingsBody(input, tc.MIME)
			if tc.Status == 200 {
				if len(details) > 0 {
					t.Fatalf("Go rejected upstream accepted input: %+v", details)
				}
				expected := deepCopyMap(fixture.Defaults)
				for key, value := range tc.Normalized {
					expected[key] = value
				}
				if !reflect.DeepEqual(canonicalSettingsValue(actual), canonicalSettingsValue(expected)) {
					t.Fatalf("normalized settings differ: Go=%v upstream=%v", actual, expected)
				}
				serialized, err := MarshalClientSettings(actual)
				if err != nil || serialized != tc.FullJSON {
					t.Fatalf("serialized settings differ: Go=%s upstream=%s err=%v", serialized, tc.FullJSON, err)
				}
			} else {
				if len(details) == 0 {
					t.Fatalf("Go accepted upstream rejected input: %+v", actual)
				}
				if tc.Status == 400 {
					if details[0].Type != "body_error" {
						t.Fatalf("body decoding failure differs: %+v", details)
					}
					return
				}
				if tc.Status == 500 {
					if _, err := SettingsValidationJSON(details); err == nil {
						t.Fatal("upstream error with non-finite input was serializable")
					}
					return
				}
				got, _ := json.Marshal(details)
				want, _ := json.Marshal(tc.Errors)
				if !reflect.DeepEqual(canonicalSettingsJSON(got), canonicalSettingsJSON(want)) {
					t.Fatalf("errors differ: Go=%s upstream=%s", got, want)
				}
			}
		})
	}
}

func TestClientSettingsValidationConcurrentIsolation(t *testing.T) {
	before, err := encodePythonJSON(clientSettingsDefaults, clientSettingsKeyOrder, "")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 32)
	for worker := range 32 {
		workers.Go(func() {
			for iteration := range 20 {
				font := fmt.Sprintf("worker-%d-%d", worker, iteration)
				input := fmt.Sprintf(`{"caption_font":%q,"show_player_background_image":"false","comment_font_size":"34.0","mylist":[{"z":%d,"a":%d}],"timetable_genre_colors":{"g-%d":"Pink"}}`, font, worker, iteration, worker)
				settings, details := ValidateClientSettingsJSON([]byte(input))
				if len(details) > 0 || settings["caption_font"] != font || settings["show_player_background_image"] != false {
					failures <- fmt.Errorf("request state leaked for %s: %v %v", font, settings, details)
					return
				}
				encoded, err := MarshalClientSettings(settings)
				if err != nil || !strings.Contains(encoded, fmt.Sprintf(`"mylist":[{"z":%d,"a":%d}]`, worker, iteration)) {
					failures <- fmt.Errorf("request dictionary order leaked for %s: %s %v", font, encoded, err)
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	after, err := encodePythonJSON(clientSettingsDefaults, clientSettingsKeyOrder, "")
	if err != nil || after != before {
		t.Fatalf("shared defaults changed: %v", err)
	}
}

func canonicalSettingsJSON(data []byte) any {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	return canonicalSettingsValue(value)
}

func canonicalSettingsValue(value any) any {
	switch item := value.(type) {
	case settingsOrderedObject:
		return canonicalSettingsValue(item.values)
	case map[string]any:
		result := map[string]any{}
		for key, value := range item {
			result[key] = canonicalSettingsValue(value)
		}
		return result
	case []any:
		result := make([]any, len(item))
		for i, value := range item {
			result[i] = canonicalSettingsValue(value)
		}
		return result
	case float64:
		// model_dump_json は非有限浮動小数点を null として表現する。
		if math.IsNaN(item) || math.IsInf(item, 0) {
			return nil
		}
		return canonicalSettingsValue(json.Number(strconv.FormatFloat(item, 'g', -1, 64)))
	case json.Number:
		number, _ := new(big.Rat).SetString(item.String())
		return "number:" + number.RatString()
	default:
		return value
	}
}
