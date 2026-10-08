package api

import (
	"context"
	"encoding/json"
	"testing"
)

// TestPG3BigIntegerWireBoundary は任意精度入力を wire まで持ち運ぶことを確認する。
func TestPG3BigIntegerWireBoundary(t *testing.T) {
	s, _ := newTestServer(t, "")
	for _, c := range []struct {
		body     string
		min, max int
		fails    bool
	}{
		{`{"service_ranges":[],"duration_range_min":9223372036854775808}`, 5808, 0, false},
		{`{"service_ranges":[],"duration_range_max":9223372036854775808}`, 0, 54775808, false},
		{`{"service_ranges":[{"network_id":9223372036854775808,"transport_stream_id":1,"service_id":1}]}`, 0, 0, true},
	} {
		t.Run(c.body, func(t *testing.T) {
			var q programSearchConditionRequest
			if err := json.Unmarshal([]byte(c.body), &q); err != nil {
				t.Fatal(err)
			}
			condition, err := q.toProgramSearchCondition()
			if err != nil {
				t.Fatal(err)
			}
			key, err := s.encodeEDCBSearchKeyInfo(context.Background(), condition, nil, nil)
			if (err != nil) != c.fails {
				t.Fatalf("err=%v want failure=%v", err, c.fails)
			}
			if !c.fails && (key.ChkDurationMin != c.min || key.ChkDurationMax != c.max) {
				t.Fatalf("min=%d max=%d", key.ChkDurationMin, key.ChkDurationMax)
			}
		})
	}
}
