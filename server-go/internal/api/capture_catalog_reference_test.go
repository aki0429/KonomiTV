package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestCatalogUpstreamReference は本家の関数を実行して生成した画像の期待値と比較する。
func TestCatalogUpstreamReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/capture_catalog_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Filename, Data string
			Modified             string `json:"modified_at"`
			Hash                 string `json:"file_hash"`
			Width                int    `json:"image_width"`
			Height               int    `json:"image_height"`
			Captured             string `json:"captured_at"`
			Status               string `json:"metadata_status"`
			Invalid              bool
			Metadata             map[string]any
		}
		Signatures []struct {
			Values    any
			Signature string
		}
		DateCases []struct {
			Raw   string
			Value *string
			Error json.RawMessage
		} `json:"date_cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(test.Data)
			if err != nil {
				t.Fatal(err)
			}
			modified, err := time.Parse(time.RFC3339Nano, test.Modified)
			if err != nil {
				t.Fatal(err)
			}
			got, err := readCatalogImage(data, test.Filename, modified)
			if test.Invalid {
				if err == nil {
					t.Fatal("corrupt image accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			captured, err := time.Parse(time.RFC3339Nano, test.Captured)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", got.Hash) != test.Hash || got.Width != test.Width || got.Height != test.Height || !got.CapturedAt.Equal(captured) || got.MetadataStatus != test.Status {
				t.Fatalf("got status=%s captured=%s dimensions=%dx%d; want %s %s %dx%d", got.MetadataStatus, got.CapturedAt, got.Width, got.Height, test.Status, test.Captured, test.Width, test.Height)
			}
			if test.Metadata == nil {
				if got.Metadata != nil {
					t.Fatal("metadata should be nil")
				}
				return
			}
			m := got.Metadata
			if m == nil {
				t.Fatal("metadata missing")
			}
			for key, actual := range map[string]time.Time{"captured_at": m.CapturedAt, "start_time": m.StartTime} {
				expected, err := time.Parse(time.RFC3339Nano, test.Metadata[key].(string))
				if err != nil || !actual.Equal(expected) {
					t.Fatalf("%s got=%s expected=%v", key, actual, test.Metadata[key])
				}
			}
			var end any
			if m.EndTime != nil {
				end = m.EndTime.Format(time.RFC3339Nano)
			}
			if end != test.Metadata["end_time"] {
				t.Fatalf("end got=%v expected=%v", end, test.Metadata["end_time"])
			}
			var duration, caption any
			if m.Duration != nil {
				duration = *m.Duration
			}
			if m.CaptionText != nil {
				caption = *m.CaptionText
			}
			actual := map[string]any{"network_id": float64(m.NetworkID), "service_id": float64(m.ServiceID), "event_id": float64(m.EventID), "captured_playback_position": m.PlaybackPosition, "duration": duration, "title": m.Title, "description": m.Description, "caption_text": caption, "is_caption_composited": m.CaptionComposited, "is_comment_composited": m.CommentComposited}
			for key, value := range actual {
				if !reflect.DeepEqual(value, test.Metadata[key]) {
					t.Fatalf("%s got=%v expected=%v", key, value, test.Metadata[key])
				}
			}
		})
	}
	for _, test := range fixture.Signatures {
		if got := catalogSignature(test.Values); got != test.Signature {
			t.Fatalf("cursor signature got=%s expected=%s", got, test.Signature)
		}
	}
	for _, test := range fixture.DateCases {
		t.Run("date_"+test.Raw, func(t *testing.T) {
			v := newFastAPIValidation(httptest.NewRequest("GET", "/api/captures?date="+url.QueryEscape(test.Raw), nil))
			got := catalogQueryDate(v, "date")
			if test.Value != nil {
				if got == nil || *got != *test.Value || len(v.details) != 0 {
					t.Fatalf("got=%v errors=%v expected=%s", got, v.details, *test.Value)
				}
			} else {
				if got != nil || len(v.details) != 1 {
					t.Fatalf("unexpected date result %v errors=%v", got, v.details)
				}
				actual, _ := json.Marshal(v.details[0])
				var want, have any
				_ = json.Unmarshal(test.Error, &want)
				_ = json.Unmarshal(actual, &have)
				if !reflect.DeepEqual(want, have) {
					t.Fatalf("got %s expected %s", actual, test.Error)
				}
			}
		})
	}
}
