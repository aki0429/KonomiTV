package api

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
	"unicode/utf16"
)

// pg3Bytes は production writer を使わない独立した CtrlCmd fixture ビルダー。
type pg3Bytes []byte

func (b *pg3Bytes) u16(v int) { *b = binary.LittleEndian.AppendUint16(*b, uint16(v)) }
func (b *pg3Bytes) u32(v int) { *b = binary.LittleEndian.AppendUint32(*b, uint32(v)) }
func (b *pg3Bytes) text(v string) {
	units := utf16.Encode([]rune(v))
	b.u32(6 + 2*len(units))
	for _, u := range units {
		b.u16(int(u))
	}
	b.u16(0)
}
func pg3Struct(body pg3Bytes) pg3Bytes {
	var b pg3Bytes
	b.u32(4 + len(body))
	return append(b, body...)
}
func pg3Vector(items ...pg3Bytes) pg3Bytes {
	var b pg3Bytes
	b.u32(0)
	b.u32(len(items))
	for _, item := range items {
		b = append(b, item...)
	}
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	return b
}

func pg3Event(eid, tsid, sid, year int, shared bool) pg3Bytes {
	var b pg3Bytes
	for _, v := range []int{4, tsid, sid, eid} {
		b.u16(v)
	}
	b = append(b, 1)
	for _, v := range []int{year, 1, 0, 2, 3, 4, 5, 0} {
		b.u16(v)
	}
	b = append(b, 1)
	b.u32(1800)
	var short pg3Bytes
	short.text("　ＡＢＣ🈑　")
	short.text("　概要　")
	b = append(b, pg3Struct(short)...)
	var ext pg3Bytes
	ext.text("- ◇見出し\r\n　本文　\r\n- ◇見出し\r\n第二本文\r\n")
	b = append(b, pg3Struct(ext)...)
	for i := 0; i < 3; i++ {
		b.u32(4)
	}
	if shared {
		var event pg3Bytes
		for _, v := range []int{4, tsid, sid + 1, eid} {
			event.u16(v)
		}
		group := append(pg3Bytes{1}, pg3Vector(pg3Struct(event))...)
		b = append(b, pg3Struct(group)...)
	} else {
		b.u32(4)
	}
	b.u32(4)
	b = append(b, 1)
	return pg3Struct(b)
}

// TestPG3NativeProgramFields は TCP デコードからサービス照合・終了済み除外と全フィールドまで固定する。
func TestPG3NativeProgramFields(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	tsid := 10
	insertTestChannel(t, server.db, testChannel{ID: "DB-channel", DisplayChannelID: "bs101", NetworkID: 4, ServiceID: 101, TransportStreamID: &tsid, ChannelNumber: "101", Type: "BS", Name: "fixture", IsWatchable: true})
	insertTestChannel(t, server.db, testChannel{ID: "hidden", DisplayChannelID: "bs102", NetworkID: 4, ServiceID: 102, TransportStreamID: &tsid, ChannelNumber: "102", Type: "BS", Name: "hidden", IsWatchable: false})
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		command, _, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		if command != 1025 {
			return fmt.Errorf("command=%d", command)
		}
		return pg3SendResponse(conn, 1, pg3Vector(pg3Event(1, 10, 101, 2099, false), pg3Event(2, 11, 101, 2099, false), pg3Event(3, 10, 102, 2099, false), pg3Event(4, 10, 101, 2000, false), pg3Event(5, 10, 101, 2099, true)))
	})
	r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{"service_ranges":[]}`, "", "application/json")
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"total": float64(1), "programs": []any{map[string]any{
		"id": "NID4-SID101-EID1", "channel_id": "DB-channel", "network_id": float64(4), "service_id": float64(101), "event_id": float64(1),
		"title": "ABC[字]", "description": "概要", "detail": map[string]any{"見出し": "本文", "見出し\t": "第二本文"},
		"start_time": "2099-01-02T03:04:05+09:00", "end_time": "2099-01-02T03:34:05+09:00", "duration": float64(1800), "is_free": false, "genres": []any{},
		"video_type": nil, "video_codec": nil, "video_resolution": nil, "primary_audio_type": "", "primary_audio_language": "", "primary_audio_sampling_rate": "",
		"secondary_audio_type": nil, "secondary_audio_language": nil, "secondary_audio_sampling_rate": nil,
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("program fields/filter mismatch\ngot=%s\nwant=%v", r.Body.String(), want)
	}
}
