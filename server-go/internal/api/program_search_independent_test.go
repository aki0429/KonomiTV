package api

import (
	"encoding/json"
	"github.com/aki0429/KonomiTV/server-go/internal/reservations"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type pg3IndependentOracle struct {
	Schema []struct {
		Text     string
		Accepted bool
	}
	Detail []struct {
		Text        string
		Detail      map[string]string
		Description string
	}
}

func pg3IndependentLoad(t *testing.T) pg3IndependentOracle {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", "pg3_review_oracle.json"))
	if e != nil {
		t.Fatal(e)
	}
	var o pg3IndependentOracle
	if e = json.Unmarshal(b, &o); e != nil {
		t.Fatal(e)
	}
	return o
}
func TestPG3IndependentSchemaOracle(t *testing.T) {
	for i, c := range pg3IndependentLoad(t).Schema {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			var q programSearchConditionRequest
			r := httptest.NewRequest("POST", "/api/programs/search", strings.NewReader(c.Text))
			ok := decodeJSONBody(r, &q)
			if ok {
				_, e := q.toProgramSearchCondition()
				ok = e == nil
			}
			if ok != c.Accepted {
				t.Errorf("request=%s Go accepted=%v Python accepted=%v", c.Text, ok, c.Accepted)
			}
		})
	}
}
func TestPG3IndependentDetailOracle(t *testing.T) {
	for i, c := range pg3IndependentLoad(t).Detail {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			p := decodeSearchEvent(reservations.EventInfo{ExtInfo: &reservations.ExtendedEventInfo{TextChar: c.Text}})
			var d map[string]string
			json.Unmarshal(p.Detail, &d)
			got, _ := json.Marshal(d)
			want, _ := json.Marshal(c.Detail)
			if string(got) != string(want) || p.Description != c.Description {
				t.Errorf("text=%q got=%s description=%q want=%s description=%q", c.Text, got, p.Description, want, c.Description)
			}
		})
	}
}
func TestPG3IndependentDetailOrder(t *testing.T) {
	p := decodeSearchEvent(reservations.EventInfo{ExtInfo: &reservations.ExtendedEventInfo{TextChar: "- B\nx\n- A\ny"}})
	if string(p.Detail) != `{"B":"x","A":"y"}` {
		t.Errorf("Python挿入順 B,A; Go=%s", p.Detail)
	}
}
func TestPG3IndependentHTTPInvalidBody(t *testing.T) {
	for _, text := range []string{`{"keyword":null,"service_ranges":[]}`, `{"date_ranges":[{}],"service_ranges":[]}`, `{"service_ranges":[]} trailing`, `null`} {
		t.Run(text, func(t *testing.T) {
			s, _ := newTestServer(t, "")
			s.config.General.Backend = "EDCB"
			s.config.General.EDCBURL = "invalid://no-port"
			r := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/programs/search", text, "", "application/json")
			if r.Code != 422 {
				t.Errorf("expected validation 422 before URL creation; got=%d body=%s", r.Code, r.Body)
			}
		})
	}
}
func TestPG3IndependentProxyRawMalformedBody(t *testing.T) {
	const text = `not-json`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != text {
			t.Errorf("body=%q", b)
		}
		w.Header().Set("X-Fresh", "yes")
		w.WriteHeader(418)
		w.Write([]byte("synthetic"))
	}))
	defer backend.Close()
	s, _ := newTestServer(t, "")
	s.config.General.Backend = "EDCB"
	u, _ := url.Parse(backend.URL)
	s.proxy = httputil.NewSingleHostReverseProxy(u)
	r := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/programs/search", text, "", "application/json")
	if r.Code != 418 || r.Body.String() != "synthetic" || r.Header().Get("X-Fresh") != "yes" {
		t.Fatal(r.Code, r.Body.String(), r.Header())
	}
}
