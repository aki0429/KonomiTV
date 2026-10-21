package api

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
)

type mockEDCBLogo struct {
	index     string
	requested []string
	fail      bool
}

func (m *mockEDCBLogo) FileCopy2(names []string) ([]reservations.FileData, bool) {
	m.requested = append(m.requested, names...)
	if m.fail {
		return nil, false
	}
	if len(names) == 2 {
		return []reservations.FileData{{Name: names[0], Data: []byte("[LogoData]\r\n00040065=7\r\n")}, {Name: names[1], Data: []byte(m.index)}}, true
	}
	if len(names) == 1 {
		return []reservations.FileData{{Name: names[0], Data: []byte("synthetic-logo")}}, true
	}
	return nil, false
}
func disableEDCBLogoNetwork(t *testing.T) {
	t.Helper()
	old := edcbLogoClientFactory
	t.Cleanup(func() { edcbLogoClientFactory = old })
	edcbLogoClientFactory = func(context.Context, string) (edcbLogoClient, error) { return &mockEDCBLogo{fail: true}, nil }
}
func TestEDCBLogoTypeOrderAndSafeName(t *testing.T) {
	s, _ := newTestServer(t, "")
	old := edcbLogoClientFactory
	t.Cleanup(func() { edcbLogoClientFactory = old })
	source := &mockEDCBLogo{index: "2026/01/01 00:00:00 10 0004_007_000_02.bmp\r\n2026/01/01 00:00:00 10 ../0004_007_000_05.png\r\n2026/01/01 00:00:00 10 0004_007_001_05.PNG\r\n"}
	edcbLogoClientFactory = func(context.Context, string) (edcbLogoClient, error) { return source, nil }
	data, mime, ok := s.fetchEDCBChannelLogo(context.Background(), &database.Channel{NetworkID: 4, ServiceID: 101})
	if !ok || string(data) != "synthetic-logo" || mime != "image/png" || len(source.requested) != 3 || source.requested[2] != `LogoData\0004_007_001_05.PNG` {
		t.Fatalf("result=%q %s %v requests=%v", data, mime, ok, source.requested)
	}
	source.index = "2026/01/01 00:00:00 10 0004_007_000_02.bmp\r\n"
	if _, mime, ok := s.fetchEDCBChannelLogo(context.Background(), &database.Channel{NetworkID: 4, ServiceID: 101}); !ok || mime != "image/bmp" {
		t.Fatal("BMP fallback failed")
	}
}

func logoFileFixture(name string, data []byte) pg3Bytes {
	var b pg3Bytes
	b.text(name)
	b.u32(len(data))
	b.u32(0)
	b = append(b, data...)
	return pg3Struct(b)
}
func logoCopyReply(files ...pg3Bytes) pg3Bytes {
	var b pg3Bytes
	b.u16(5)
	return append(b, pg3Vector(files...)...)
}
func TestEDCBLogoProtocolHTTPAndETag(t *testing.T) {
	s, paths := newTestServer(t, "")
	writeFile(t, filepath.Join(paths.StaticDir, "logos", "default.png"), "default")
	insertTestChannel(t, s.db, testChannel{ID: "NID4-SID101", DisplayChannelID: "bs101", NetworkID: 4, ServiceID: 101, Type: "BS", Name: "Synthetic", ChannelNumber: "101", IsWatchable: true})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s.config.General.EDCBURL = "tcp://" + listener.Addr().String() + "/"
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			command, body, err := pg3ReadRequest(conn)
			var request pg3Bytes
			request.u16(5)
			var response pg3Bytes
			if i%2 == 0 {
				var a, b pg3Bytes
				a.text("LogoData.ini")
				b.text(`LogoData\*.*`)
				request = append(request, pg3Vector(a, b)...)
				response = logoCopyReply(logoFileFixture("LogoData.ini", []byte("00040065=7\r\n")), logoFileFixture(`LogoData\*.*`, []byte("2026/01/01 00:00:00 10 0004_007_000_05.png\r\n")))
			} else {
				var name pg3Bytes
				name.text(`LogoData\0004_007_000_05.png`)
				request = append(request, pg3Vector(name)...)
				response = logoCopyReply(logoFileFixture(`LogoData\0004_007_000_05.png`, []byte("png-wire-logo")))
			}
			if err == nil && (command != 2060 || !bytes.Equal(body, request)) {
				err = fmt.Errorf("FileCopy2 mismatch command=%d got=%x want=%x", command, body, request)
			}
			if err == nil {
				err = pg3SendResponse(conn, 1, response)
			}
			conn.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	handler := s.Handler()
	first := doJSONRequest(t, handler, http.MethodGet, "/api/channels/bs101/logo", "", "", "")
	if first.Code != 200 || first.Body.String() != "png-wire-logo" || first.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("logo=%d %s", first.Code, first.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/api/channels/bs101/logo", nil)
	request.Header.Set("If-None-Match", first.Header().Get("ETag"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatal("logo ETag failed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
