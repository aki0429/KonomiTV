package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/torne"
)

// toruPayload は仕様どおりのグループヘッダーと番組レコードを独立に組み立てる。
func toruPayload(group, sid int, counts []int) []byte {
	data := make([]byte, 16+8*len(counts))
	data[1] = 1
	binary.BigEndian.PutUint64(data[8:], uint64(group)<<60|uint64(len(counts)))
	for index, count := range counts {
		offset := 16 + 8*index
		binary.BigEndian.PutUint16(data[offset:], uint16(sid))
		binary.BigEndian.PutUint16(data[offset+2:], uint16(index+1))
		binary.BigEndian.PutUint32(data[offset+4:], uint32(count))
	}
	return data
}

// TestToruRankingEndpoints は本家の上位100件・20件ページ・未取得時・フィルタをHTTPで確認する。
func TestToruRankingEndpoints(t *testing.T) {
	s, _ := newTestServer(t, "")
	handler := s.Handler()
	response := doJSONRequest(t, handler, http.MethodGet, "/api/programs/toru-counts", "", "", "")
	if response.Code != http.StatusOK || response.Body.String() != "{}\n" {
		t.Fatalf("initial counts status=%d body=%q", response.Code, response.Body.String())
	}
	now := timeTableTestSetup(t, s)
	counts := make([]int, 105)
	for index := range counts {
		counts[index] = index + 1
		insertTestProgram(t, s.db, fmt.Sprintf("NID32736-SID1024-EID%d", index+1), "NID32736-SID1024", fmt.Sprintf("rank %d", index+1), now.Add(time.Hour), now.Add(2*time.Hour), 3600)
	}
	err := s.toruCounts.Update(context.Background(), []torne.Channel{{Type: "GR", NetworkID: 32736, ServiceID: 1024}}, now,
		func(context.Context, int, time.Time) ([]byte, error) { return toruPayload(1, 1024, counts), nil })
	if err != nil {
		t.Fatal(err)
	}
	type rankingResponse struct {
		Total    int               `json:"total"`
		Programs []programResponse `json:"programs"`
	}
	for _, test := range []struct {
		query string
		total int
		first string
		count int
	}{
		{"", 100, "NID32736-SID1024-EID105", 20},
		{"?page=2", 100, "NID32736-SID1024-EID85", 20},
		{"?page=6", 100, "", 0},
		{"?page=9223372036854775807", 100, "", 0},
		{"?channel_type=BS", 0, "", 0},
		{"?genre=" + url.QueryEscape("存在しないジャンル"), 0, "", 0},
	} {
		response := doJSONRequest(t, handler, http.MethodGet, "/api/programs/toru-ranking"+test.query, "", "", "")
		var got rankingResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatalf("query=%s status=%d body=%s", test.query, response.Code, response.Body.String())
		}
		if got.Total != test.total || len(got.Programs) != test.count || (test.first != "" && got.Programs[0].ID != test.first) {
			t.Fatalf("query=%s response=%+v", test.query, got)
		}
	}
	for _, query := range []string{"?page=0", "?page=x", "?channel_type=CATV"} {
		response := doJSONRequest(t, handler, http.MethodGet, "/api/programs/toru-ranking"+query, "", "", "")
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("query=%s status=%d", query, response.Code)
		}
	}
}
