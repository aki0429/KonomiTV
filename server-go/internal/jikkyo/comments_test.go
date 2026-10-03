package jikkyo

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// commentsFixture は tools/generate_jikkyo_comments_fixture.py が生成するフィクスチャ。
type commentsFixture struct {
	StartTime       int64               `json:"start_time"`
	Colors          map[string]*string  `json:"colors"`
	Commands        map[string][]string `json:"commands"`
	SpecialCommands map[string]bool     `json:"special_commands"`
	Packets         []json.RawMessage   `json:"packets"`
	Comments        []Comment           `json:"comments"`
}

// loadCommentsFixture はフィクスチャを読み込む。
func loadCommentsFixture(t *testing.T) *commentsFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/jikkyo_comments.json")
	if err != nil {
		t.Fatalf("failed to read the fixture: %v", err)
	}
	fixture := &commentsFixture{}
	if err := json.Unmarshal(data, fixture); err != nil {
		t.Fatalf("failed to parse the fixture: %v", err)
	}
	return fixture
}

// TestGetCommentColorMatchesPython は色指定の変換が Python 版と一致することを検証する。
func TestGetCommentColorMatchesPython(t *testing.T) {
	fixture := loadCommentsFixture(t)
	for color, expected := range fixture.Colors {
		actual, ok := GetCommentColor(color)
		if expected == nil {
			if ok {
				t.Errorf("GetCommentColor(%q) = %q, want not found", color, actual)
			}
			continue
		}
		if !ok || actual != *expected {
			t.Errorf("GetCommentColor(%q) = %q (%v), want %q", color, actual, ok, *expected)
		}
	}
}

// TestParseCommentCommandMatchesPython はコメントコマンドの解析が Python 版と一致することを検証する。
func TestParseCommentCommandMatchesPython(t *testing.T) {
	fixture := loadCommentsFixture(t)
	for key, expected := range fixture.Commands {
		var mail *string
		if key != "<null>" {
			value := key
			mail = &value
		}
		color, position, size := ParseCommentCommand(mail)
		actual := []string{color, position, size}
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("ParseCommentCommand(%q) = %v, want %v", key, actual, expected)
		}
	}
}

// TestIsSpecialCommandCommentMatchesPython は運営コマンド付きコメントの判定が Python 版と一致することを検証する。
func TestIsSpecialCommandCommentMatchesPython(t *testing.T) {
	fixture := loadCommentsFixture(t)
	for key, expected := range fixture.SpecialCommands {
		// キーは "<コメント>|<premium>" の形式
		separator := -1
		for index := len(key) - 1; index >= 0; index-- {
			if key[index] == '|' {
				separator = index
				break
			}
		}
		if separator < 0 {
			t.Fatalf("invalid fixture key: %q", key)
		}
		comment := key[:separator]
		premiumText := key[separator+1:]
		var premium *string
		if premiumText != "<null>" {
			premium = &premiumText
		}
		if actual := IsSpecialCommandComment(comment, premium); actual != expected {
			t.Errorf("IsSpecialCommandComment(%q, %q) = %v, want %v", comment, premiumText, actual, expected)
		}
	}
}

// TestParseKakologResponseMatchesPython は過去ログ API のレスポンスの整形が Python 版と一致することを検証する。
func TestParseKakologResponseMatchesPython(t *testing.T) {
	fixture := loadCommentsFixture(t)

	// フィクスチャの packet を過去ログ API のレスポンスの形に組み立てる
	response := map[string]any{"packet": json.RawMessage("[]")}
	packets := []json.RawMessage{}
	packets = append(packets, fixture.Packets...)
	encodedPackets, err := json.Marshal(packets)
	if err != nil {
		t.Fatal(err)
	}
	response["packet"] = json.RawMessage(encodedPackets)
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}

	comments, err := parseKakologResponse(encodedResponse, fixture.StartTime)
	if err != nil {
		t.Fatalf("parseKakologResponse() failed: %v", err)
	}
	if !comments.IsSuccess {
		t.Errorf("is_success = false, detail = %q", comments.Detail)
	}
	if !reflect.DeepEqual(comments.Comments, fixture.Comments) {
		t.Errorf("comments = %+v, want %+v", comments.Comments, fixture.Comments)
	}
}

// TestParseKakologResponseErrors は過去ログ API のエラー応答を検証する。
func TestParseKakologResponseErrors(t *testing.T) {
	// エラーが入っている場合
	comments, err := parseKakologResponse([]byte(`{"error": "エラーメッセージ"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if comments.IsSuccess || comments.Detail != "エラーメッセージ" {
		t.Errorf("comments = %+v", comments)
	}

	// コメントが 1 つもない場合
	comments, err = parseKakologResponse([]byte(`{"packet": []}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if comments.IsSuccess || comments.Detail != "この録画番組の過去ログコメントは存在しないか、現在取得中です。" {
		t.Errorf("comments = %+v", comments)
	}
	if comments.Comments == nil || len(comments.Comments) != 0 {
		t.Errorf("comments = %+v", comments.Comments)
	}
}
