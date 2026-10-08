package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPG3CoercionOracle は実 Python schema の許容、変換後値、エラー型・本文を固定する。
func TestPG3CoercionOracle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pg3_coercion_oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text     string
		Accepted bool
		Result   json.RawMessage
		Errors   []struct{ Type, Msg string }
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Text, func(t *testing.T) {
			var request programSearchConditionRequest
			err := json.Unmarshal([]byte(c.Text), &request)
			var got programSearchCondition
			if err == nil {
				got, err = request.toProgramSearchCondition()
			}
			if (err == nil) != c.Accepted {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, c.Accepted, err)
			}
			if !c.Accepted {
				// 型の検証は JSON coercion 段階に限定し、既存範囲検証のメッセージも不変にする。
				if err.Error() != c.Errors[0].Msg {
					t.Errorf("message=%q want=%q", err.Error(), c.Errors[0].Msg)
				}
				if typed, ok := err.(interface{ ValidationType() string }); ok && typed.ValidationType() != c.Errors[0].Type {
					t.Errorf("type=%s want=%s", typed.ValidationType(), c.Errors[0].Type)
				}
				return
			}
			b, _ := json.Marshal(got)
			var actual, want map[string]json.RawMessage
			json.Unmarshal(b, &actual)
			json.Unmarshal(c.Result, &want)
			for key, value := range want {
				if string(actual[key]) != string(value) {
					t.Errorf("%s=%s want=%s", key, actual[key], value)
				}
			}
		})
	}
}
