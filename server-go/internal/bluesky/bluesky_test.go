package bluesky

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixture は testdata/generate_bluesky_fixture.py が Python 版 BlueskyAPI から生成した期待値。
type fixture struct {
	HandleCases []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"handle_cases"`
	DatetimeCases []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"datetime_cases"`
	RecordKeyCases []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"record_key_cases"`
	TagCases []struct {
		Text      string `json:"text"`
		BuiltText string `json:"built_text"`
		Facets    []any  `json:"facets"`
	} `json:"tag_cases"`
	FacetExpandCases []struct {
		Text   string `json:"text"`
		Facets []any  `json:"facets"`
		Output string `json:"output"`
	} `json:"facet_expand_cases"`
	PostViews []struct {
		Name   string         `json:"name"`
		Reason map[string]any `json:"reason"`
		Post   map[string]any `json:"post"`
	} `json:"post_views"`
	PostViewResults []struct {
		Name  string `json:"name"`
		Tweet any    `json:"tweet"`
	} `json:"post_view_results"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "bluesky_fixture.json"))
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	var value fixture
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("failed to parse fixture: %v", err)
	}
	return value
}

// toJSONValue は任意の値を JSON 経由で map[string]any / []any に変換する。
// フィクスチャ (Python の JSON) と構造を比較するために使う。
func toJSONValue(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to marshal value: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("failed to unmarshal value: %v", err)
	}
	return decoded
}

// parseFacets はフィクスチャの facets (Python のモデルダンプ) を Facet へ変換する。
func parseFacets(t *testing.T, raw []any) []Facet {
	t.Helper()
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("failed to marshal facets: %v", err)
	}
	var facets []Facet
	if err := json.Unmarshal(encoded, &facets); err != nil {
		t.Fatalf("failed to unmarshal facets: %v", err)
	}
	return facets
}

// TestNormalizeHandleMatchesPython は handle の正規化が Python 版と一致することを検証する。
func TestNormalizeHandleMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.HandleCases) == 0 {
		t.Fatal("fixture has no handle_cases")
	}
	for _, testCase := range value.HandleCases {
		t.Run(testCase.Input, func(t *testing.T) {
			if got := NormalizeHandle(testCase.Input); got != testCase.Output {
				t.Errorf("NormalizeHandle(%q) = %q, want %q", testCase.Input, got, testCase.Output)
			}
		})
	}
}

// TestParseDateTimeMatchesPython は日時パースと JST への変換が Python 版と一致することを検証する。
func TestParseDateTimeMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.DatetimeCases) == 0 {
		t.Fatal("fixture has no datetime_cases")
	}
	for _, testCase := range value.DatetimeCases {
		t.Run(testCase.Input, func(t *testing.T) {
			parsed, err := ParseDateTime(testCase.Input)
			if err != nil {
				t.Fatalf("ParseDateTime(%q) returned error: %v", testCase.Input, err)
			}
			// フィクスチャは Python 版 _parseDateTime(...).isoformat(' ') の出力
			if got := FormatJSTDateTimeISO(parsed, " "); got != testCase.Output {
				t.Errorf("isoformat = %q, want %q", got, testCase.Output)
			}
		})
	}
}

// TestExtractRecordKeyMatchesPython は AT URI からの record key 抽出が Python 版と一致することを検証する。
func TestExtractRecordKeyMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.RecordKeyCases) == 0 {
		t.Fatal("fixture has no record_key_cases")
	}
	for _, testCase := range value.RecordKeyCases {
		t.Run(testCase.Input, func(t *testing.T) {
			if got := ExtractRecordKey(testCase.Input); got != testCase.Output {
				t.Errorf("ExtractRecordKey(%q) = %q, want %q", testCase.Input, got, testCase.Output)
			}
		})
	}
}

// TestBuildFacetsMatchesPython は URL / ハッシュタグの facet 生成が Python 版と一致することを検証する。
func TestBuildFacetsMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.TagCases) == 0 {
		t.Fatal("fixture has no tag_cases")
	}
	for index, testCase := range value.TagCases {
		t.Run(testCase.Text, func(t *testing.T) {
			builtText, facets := BuildFacets(testCase.Text)
			if builtText != testCase.BuiltText {
				t.Errorf("case %d: builtText = %q, want %q", index, builtText, testCase.BuiltText)
			}
			got := toJSONValue(t, facets)
			if facets == nil {
				got = nil
			}
			want := testCase.Facets
			if want == nil {
				want = nil
			}
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Errorf("case %d: facets mismatch\n got: %s\nwant: %s", index, gotJSON, wantJSON)
			}
		})
	}
}

// TestExpandFacetLinksInTextMatchesPython は link facet の展開が Python 版と一致することを検証する。
func TestExpandFacetLinksInTextMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.FacetExpandCases) == 0 {
		t.Fatal("fixture has no facet_expand_cases")
	}
	for index, testCase := range value.FacetExpandCases {
		t.Run(testCase.Text, func(t *testing.T) {
			got := ExpandFacetLinksInText(testCase.Text, parseFacets(t, testCase.Facets))
			if got != testCase.Output {
				t.Errorf("case %d: text = %q, want %q", index, got, testCase.Output)
			}
		})
	}
}

// TestFormatPostViewMatchesPython は PostView の Tweet 変換が Python 版と一致することを検証する。
func TestFormatPostViewMatchesPython(t *testing.T) {
	value := loadFixture(t)
	if len(value.PostViewResults) == 0 {
		t.Fatal("fixture has no post_view_results")
	}
	posts := map[string]map[string]any{}
	reasons := map[string]map[string]any{}
	for _, postView := range value.PostViews {
		posts[postView.Name] = postView.Post
		if postView.Reason != nil {
			reasons[postView.Name] = postView.Reason
		}
	}

	for _, testCase := range value.PostViewResults {
		t.Run(testCase.Name, func(t *testing.T) {
			rawPost, ok := posts[testCase.Name]
			if !ok {
				t.Fatalf("post_views has no entry named %q", testCase.Name)
			}
			post, err := ParsePostView(rawPost)
			if err != nil {
				t.Fatalf("ParsePostView returned error: %v", err)
			}

			// Python 版は ReasonRepost のみをリポストとして扱う (それ以外の reason は無視される)
			var reason *ReasonRepost
			if rawReason := reasons[testCase.Name]; rawReason != nil {
				if rawReason["$type"] == "app.bsky.feed.defs#reasonRepost" {
					encoded, err := json.Marshal(rawReason)
					if err != nil {
						t.Fatalf("failed to marshal reason: %v", err)
					}
					var parsed ReasonRepost
					if err := json.Unmarshal(encoded, &parsed); err != nil {
						t.Fatalf("failed to unmarshal reason: %v", err)
					}
					reason = &parsed
				}
			}

			tweet, err := FormatPostView(post, reason)
			if err != nil {
				t.Fatalf("FormatPostView returned error: %v", err)
			}
			got := toJSONValue(t, tweet)
			if !reflect.DeepEqual(got, testCase.Tweet) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(testCase.Tweet)
				t.Errorf("tweet mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}
