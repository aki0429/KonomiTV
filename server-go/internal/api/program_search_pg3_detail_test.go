package api

import (
	"encoding/json"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"reflect"
	"testing"
)

// TestPG3ExtendedHeadingCollision は raw 見出しの重複タブを正規化の前に保持する Python の二段階処理を固定する。
func TestPG3ExtendedHeadingCollision(t *testing.T) {
	event := reservations.EventInfo{Onid: 4, Sid: 1, Eid: 1, ExtInfo: &reservations.ExtendedEventInfo{TextChar: "- Ａ\nfirst\n- A\nsecond\n- Ａ\nthird\n"}}
	program := decodeSearchEvent(event)
	var got map[string]string
	if err := json.Unmarshal(program.Detail, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "first", "A\t": "second", "A\t\t": "third"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("head collision=%v want=%v", got, want)
	}
	if program.Description != "first" {
		t.Fatalf("fallback=%q", program.Description)
	}
	if program.StartTime != "1970-01-01T09:00:00+09:00" || program.EndTime != "1970-01-01T09:05:00+09:00" || program.Duration != 300 {
		t.Fatalf("missing timestamp/duration defaults=%+v", program)
	}
}

// TestPG3ExtendedNoSpuriousPrefix は最初に見出しがある場合に空の番組内容を挿入しない。
func TestPG3ExtendedNoSpuriousPrefix(t *testing.T) {
	program := decodeSearchEvent(reservations.EventInfo{ExtInfo: &reservations.ExtendedEventInfo{TextChar: ""}})
	var got map[string]string
	json.Unmarshal(program.Detail, &got)
	if !reflect.DeepEqual(got, map[string]string{}) {
		t.Fatalf("detail=%v", got)
	}
}
