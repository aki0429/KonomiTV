package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
)

// TestPG3NativeMedia はジャンル nibble の byte swap と映像/二音声を実 TCP 応答から固定する。
func TestPG3NativeMedia(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	tsid := 10
	insertTestChannel(t, server.db, testChannel{ID: "DB-channel", DisplayChannelID: "bs101", NetworkID: 4, ServiceID: 101, TransportStreamID: &tsid, ChannelNumber: "101", Type: "BS", Name: "fixture", IsWatchable: true})
	event := pg3Event(1, 10, 101, 2099, false)
	// short/ext の直後へ、独立した ContentInfo/ComponentInfo/AudioInfo を挿入する。
	pos := 34
	pos += int(uint32(event[pos]) | uint32(event[pos+1])<<8 | uint32(event[pos+2])<<16 | uint32(event[pos+3])<<24)
	pos += int(uint32(event[pos]) | uint32(event[pos+1])<<8 | uint32(event[pos+2])<<16 | uint32(event[pos+3])<<24)
	var nibble pg3Bytes
	nibble.u16(0x0100)
	nibble.u16(0)
	content := pg3Struct(pg3Vector(pg3Struct(nibble)))
	video := pg3Bytes{5, 0xb3, 1}
	video.text("")
	video = pg3Struct(video)
	var audioItems []pg3Bytes
	for _, typ := range []byte{2, 3} {
		a := pg3Bytes{2, typ, 1, 0, 0, 1, 1, 0, 7}
		a.text("")
		audioItems = append(audioItems, pg3Struct(a))
	}
	audio := pg3Struct(pg3Vector(audioItems...))
	body := append(pg3Bytes{}, event[4:pos]...)
	body = append(body, content...)
	body = append(body, video...)
	body = append(body, audio...)
	body = append(body, event[pos+12:]...)
	event = pg3Struct(body)
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		command, _, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		if command != 1025 {
			return fmt.Errorf("command=%d", command)
		}
		return pg3SendResponse(conn, 1, pg3Vector(event))
	})
	r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{"service_ranges":[]}`, "", "application/json")
	var got struct {
		Total    int              `json:"total"`
		Programs []map[string]any `json:"programs"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if r.Code != 200 || got.Total != 1 || len(got.Programs) != 1 {
		t.Fatalf("search=%d %s", r.Code, r.Body.String())
	}
	want := map[string]any{"genres": []any{map[string]any{"major": "ニュース・報道", "middle": "天気"}}, "video_type": "H.264|MPEG-4 AVC、映像1080i(1125i)、アスペクト比16:9 パンベクトルなし", "video_codec": "H.264", "video_resolution": "1080i", "primary_audio_type": "1/0+1/0モード(デュアルモノ)", "primary_audio_language": "日本語+英語", "primary_audio_sampling_rate": "48kHz", "secondary_audio_type": "2/0モード(ステレオ)", "secondary_audio_language": "副音声", "secondary_audio_sampling_rate": "48kHz"}
	for key, value := range want {
		if !reflect.DeepEqual(got.Programs[0][key], value) {
			t.Errorf("%s=%v want=%v", key, got.Programs[0][key], value)
		}
	}
}
