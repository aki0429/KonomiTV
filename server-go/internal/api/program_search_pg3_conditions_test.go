package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
)

// pg3ExpectedKey は Python CtrlCmdUtil の規約を独立ビルダーで構築する。
func pg3ExpectedKey(and, not string, services []uint64, complex bool) pg3Bytes {
	var key pg3Bytes
	key.text(and)
	key.text(not)
	if complex {
		key.u32(1)
		key.u32(1)
	} else {
		key.u32(0)
		key.u32(0)
	}
	if complex {
		var n pg3Bytes
		n.u16(0xff01)
		n.u16(0)
		key = append(key, pg3Vector(pg3Struct(n))...)
		var d pg3Bytes
		d = append(d, 1)
		d.u16(22)
		d.u16(30)
		d = append(d, 2)
		d.u16(1)
		d.u16(15)
		key = append(key, pg3Vector(pg3Struct(d))...)
	} else {
		key = append(key, pg3Vector()...)
		key = append(key, pg3Vector()...)
	}
	var list []pg3Bytes
	for _, s := range services {
		var b pg3Bytes
		b = binary.LittleEndian.AppendUint64(b, s)
		list = append(list, b)
	}
	key = append(key, pg3Vector(list...)...)
	key = append(key, pg3Vector()...)
	key = append(key, pg3Vector()...)
	if complex {
		key = append(key, 1, 1, 1, 2)
	} else {
		key = append(key, 0, 0, 0, 0)
	}
	return pg3Vector(pg3Struct(key))
}

// TestPG3NativeSearchConditions は UTF-16 の surrogate 長、全条件、通常コマンドの末尾を byte 単位で固定する。
func TestPG3NativeSearchConditions(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	want := pg3ExpectedKey("^!{999}C!{999}D!{100300090}天気🈑", ":note:a\\sb\\mc\\\\d 除外", []uint64{uint64(4)<<32 | uint64(10)<<16 | 101}, true)
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		command, body, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		if command != 1025 || !bytes.Equal(body, want) {
			return fmt.Errorf("SearchPg wire mismatch command=%d\ngot=%x\nwant=%x", command, body, want)
		}
		return pg3SendResponse(conn, 1, pg3Vector())
	})
	request := `{"is_enabled":false,"keyword":"天気🈑","exclude_keyword":"除外","note":"a b　c\\d","is_title_only":true,"is_case_sensitive":true,"is_fuzzy_search_enabled":true,"is_regex_search_enabled":true,"service_ranges":[{"network_id":4,"transport_stream_id":10,"service_id":101}],"genre_ranges":[{"major":"スポーツ","middle":"すべて"}],"is_exclude_genre_ranges":true,"date_ranges":[{"start_day_of_week":1,"start_hour":22,"start_minute":30,"end_day_of_week":2,"end_hour":1,"end_minute":15}],"is_exclude_date_ranges":true,"duration_range_min":30,"duration_range_max":90,"broadcast_type":"PaidOnly","duplicate_title_check_scope":"AllChannels","duplicate_title_check_period_days":10}`
	r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", request, "", "application/json")
	if r.Code != 200 || r.Body.String() != "{\"total\":0,\"programs\":[]}\n" {
		t.Fatalf("search=%d %s", r.Code, r.Body.String())
	}
}

// TestPG3NativeDefaultServices は null/省略時の ChSet5 転送と重複排除を実 TCP で固定する。
func TestPG3NativeDefaultServices(t *testing.T) {
	for _, body := range []string{`{}`, `{"service_ranges":null}`} {
		t.Run(body, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			server.config.General.Backend = "EDCB"
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				for i := 0; i < 2; i++ {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					command, data, err := pg3ReadRequest(conn)
					if err == nil && i == 0 {
						var name pg3Bytes
						name.text("ChSet5.txt")
						if command != 1060 || !bytes.Equal(data, name) {
							err = fmt.Errorf("FileCopy frame=%d %x", command, data)
						} else {
							err = pg3SendResponse(conn, 1, []byte("one\tnet\t4\t10\t101\t1\t0\t1\t0\r\nduplicate\tnet\t4\t10\t101\t1\t0\t1\t1\r\ntwo\tnet\t4\t11\t102\t1\t0\t1\t1\r\n"))
						}
					} else if err == nil {
						want := pg3ExpectedKey("", "", []uint64{uint64(4)<<32 | uint64(10)<<16 | 101, uint64(4)<<32 | uint64(11)<<16 | 102}, false)
						if command != 1025 || !bytes.Equal(data, want) {
							err = fmt.Errorf("default service frame=%d %x want=%x", command, data, want)
						} else {
							err = pg3SendResponse(conn, 1, pg3Vector())
						}
					}
					conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			server.config.General.EDCBURL = "tcp://" + listener.Addr().String() + "/"
			r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", body, "", "application/json")
			listener.Close()
			if err := <-done; err != nil {
				t.Error(err)
			}
			if r.Code != 200 || r.Body.String() != "{\"total\":0,\"programs\":[]}\n" {
				t.Fatalf("search=%d %s", r.Code, r.Body.String())
			}
		})
	}
}

// TestPG3NativeBadFrames は切れた frame/不正 vector/構造体/失敗コードを空配列へ変換する。
func TestPG3NativeBadFrames(t *testing.T) {
	cases := []struct {
		name  string
		frame []byte
	}{
		{"short-header", []byte{1, 0}},
		{"negative-size", []byte{1, 0, 0, 0, 255, 255, 255, 255}},
		{"truncated-body", []byte{1, 0, 0, 0, 8, 0, 0, 0, 8, 0}},
		{"bad-vector", append([]byte{1, 0, 0, 0, 8, 0, 0, 0}, []byte{7, 0, 0, 0, 0, 0, 0, 0}...)},
		{"negative-count", append([]byte{1, 0, 0, 0, 8, 0, 0, 0}, []byte{8, 0, 0, 0, 255, 255, 255, 255}...)},
		{"missing-event", append([]byte{1, 0, 0, 0, 8, 0, 0, 0}, []byte{8, 0, 0, 0, 1, 0, 0, 0}...)},
		{"failed-code", []byte{0, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			server.config.General.Backend = "EDCB"
			server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
				command, _, err := pg3ReadRequest(conn)
				if err != nil {
					return err
				}
				if command != 1025 {
					return fmt.Errorf("command=%d", command)
				}
				_, err = conn.Write(tc.frame)
				return err
			})
			r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{"service_ranges":[]}`, "", "application/json")
			if r.Code != 200 || r.Body.String() != "{\"total\":0,\"programs\":[]}\n" {
				t.Fatalf("bad frame=%d %s", r.Code, r.Body.String())
			}
		})
	}
}

func TestPG3NativeValidation(t *testing.T) {
	for _, body := range []string{`{`, `{"broadcast_type":"Invalid"}`, `{"date_ranges":[{"start_day_of_week":7}]}`, `{"duration_range_min":-1}`} {
		t.Run(body, func(t *testing.T) {
			server, _ := newTestServer(t, "")
			server.config.General.Backend = "EDCB"
			server.config.General.EDCBURL = "invalid://no-port"
			r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", body, "", "application/json")
			if r.Code != 422 {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			var detail map[string]any
			if err := json.Unmarshal(r.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if _, ok := detail["detail"].(string); !ok {
				t.Fatal("missing detail")
			}
		})
	}
}

// TestPG3NativeFragmentedResponse は ReadFull が一回の Read に依存していないことを固定する。
func TestPG3NativeFragmentedResponse(t *testing.T) {
	server, _ := newTestServer(t, "")
	server.config.General.Backend = "EDCB"
	server.config.General.EDCBURL = pg3TCP(t, func(conn net.Conn) error {
		_, _, err := pg3ReadRequest(conn)
		if err != nil {
			return err
		}
		for _, b := range []byte{1, 0, 0, 0, 8, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0} {
			if _, err := conn.Write([]byte{b}); err != nil {
				return err
			}
		}
		return nil
	})
	r := doJSONRequest(t, server.Handler(), http.MethodPost, "/api/programs/search", `{"service_ranges":[]}`, "", "application/json")
	if r.Code != 200 || r.Body.String() != "{\"total\":0,\"programs\":[]}\n" {
		t.Fatalf("fragmented=%d %s", r.Code, r.Body.String())
	}
}

var _ = io.EOF
