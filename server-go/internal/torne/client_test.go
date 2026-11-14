package torne

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestToruUpstreamFixture は本家クラスを合成 DB/HTTP で実行した結果と完全一致させる。
func TestToruUpstreamFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name      string            `json:"name"`
			Channels  []Channel         `json:"channels"`
			Responses map[string]string `json:"responses"`
			Expected  map[string]Count  `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var cache Cache
			err := cache.Update(context.Background(), test.Channels, time.Now(), func(_ context.Context, region int, _ time.Time) ([]byte, error) {
				raw, exists := test.Responses[strconv.Itoa(region)]
				if !exists {
					return nil, errors.New("unexpected region")
				}
				return hex.DecodeString(raw)
			})
			if err != nil || !reflect.DeepEqual(cache.Snapshot(), test.Expected) {
				t.Fatalf("counts differ from upstream: got=%v want=%v err=%v", cache.Snapshot(), test.Expected, err)
			}
		})
	}
}

func payload(groups ...[]record) []byte {
	data := make([]byte, 8*(len(groups)+1))
	data[1] = byte(len(groups))
	for index, records := range groups {
		binary.BigEndian.PutUint64(data[8*(index+1):], uint64(records[0].key.group)<<60|uint64(len(records)))
		for _, record := range records {
			var raw [8]byte
			binary.BigEndian.PutUint16(raw[:], uint16(record.key.sid))
			binary.BigEndian.PutUint16(raw[2:], uint16(record.key.eid))
			binary.BigEndian.PutUint32(raw[4:], uint32(record.count))
			data = append(data, raw[:]...)
		}
	}
	return data
}

// TestParseToruCounts は重複の先頭優先・グループ別基準・境界検証を固定する。
func TestParseToruCounts(t *testing.T) {
	data := payload([]record{
		{eventKey{1, 1024, 1}, 0}, {eventKey{1, 1024, 2}, 1},
		{eventKey{1, 1024, 3}, 2}, {eventKey{1, 1024, 4}, 100},
		{eventKey{1, 1024, 4}, 999},
	}, []record{{eventKey{2, 101, 1}, 1}})
	records, bases, err := parse(data)
	if err != nil || len(records) != 5 || records[3].count != 100 || bases[1] == nil || *bases[1] != 100 || bases[2] != nil {
		t.Fatalf("records=%v bases=%v err=%v", records, bases, err)
	}
	for length := 0; length < len(data); length++ {
		if _, _, err := parse(data[:length]); err == nil {
			t.Fatalf("accepted truncated length %d", length)
		}
	}
	if _, _, err := parse(append(data, 0)); err == nil {
		t.Fatal("accepted trailing data")
	}
}

// TestToruHighlightPopulationDeviation は本家 statistics.pstdev の母分散と整数化を確認する。
func TestToruHighlightPopulationDeviation(t *testing.T) {
	records := []record{}
	for index, value := range []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 100} {
		records = append(records, record{eventKey{1, 1024, index + 1}, value})
	}
	_, bases, err := parse(payload(records))
	// Python: min(int(statistics.fmean(values) + 2 * statistics.pstdev(values)), 99) + 1 == 72
	if err != nil || bases[1] == nil || *bases[1] != 72 {
		t.Fatalf("base=%v err=%v", bases[1], err)
	}
}

// TestToruCacheAtomicRegions は全地域成功時の最大値選択と失敗時の前回保持を確認する。
func TestToruCacheAtomicRegions(t *testing.T) {
	var cache Cache
	channels := []Channel{{"GR", 10, 31 << 10}, {"GR", 11, 32 << 10}, {"BS", 4, 101}, {"CATV", 12, 500}}
	called := []int{}
	fetch := func(_ context.Context, region int, start time.Time) ([]byte, error) {
		called = append(called, region)
		if start.Hour() != 0 || start.Location().String() != "Asia/Tokyo" {
			t.Errorf("start=%v", start)
		}
		return payload([]record{{eventKey{1, 31 << 10, 1}, int64(region)}, {eventKey{1, 31 << 10, 0}, 200}}, []record{{eventKey{2, 101, 1}, 1}}), nil
	}
	if err := cache.Update(context.Background(), channels, time.Now(), fetch); err != nil {
		t.Fatal(err)
	}
	got := cache.Snapshot()
	if !reflect.DeepEqual(called, []int{31, 32}) || len(got) != 2 || got["NID10-SID31744-EID1"].Count != 32 {
		t.Fatalf("regions=%v counts=%v", called, got)
	}
	err := cache.Update(context.Background(), channels, time.Now(), func(context.Context, int, time.Time) ([]byte, error) {
		return nil, errors.New("offline")
	})
	if err == nil || !reflect.DeepEqual(got, cache.Snapshot()) {
		t.Fatal("failed update changed the cached counts")
	}
	base := got["NID10-SID31744-EID1"]
	*base.HighlightBase = -1
	if *cache.Snapshot()["NID10-SID31744-EID1"].HighlightBase < 0 {
		t.Fatal("snapshot allowed mutation of cache")
	}
}

// TestToruHTTPContract は認証と8日分の地域指定を合成HTTPサーバーで照合する。
func TestToruHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "synthetic-user" || password != "synthetic-password" || r.Header.Get("mobileIdAuth") != "synthetic-mobile" || r.UserAgent() != "torne 150 (Android)" {
			t.Error("request headers differ from upstream")
		}
		_ = r.ParseForm()
		if r.Form.Get("regionid") != "23" || r.Form.Get("duration") != "8" || r.Form.Get("startdate") != "2026-11-14T00:00:00+0900" || r.Form.Get("op") != "opGetToruCount" {
			t.Errorf("form=%v", r.Form)
		}
		_, _ = io.WriteString(w, "response")
	}))
	defer server.Close()
	fetch := HTTPFetcher(server.Client(), server.URL, Credentials{Username: "synthetic-user", Password: "synthetic-password", MobileIDAuth: "synthetic-mobile"})
	start := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	data, err := fetch(context.Background(), 23, start)
	if err != nil || strings.TrimSpace(string(data)) != "response" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
