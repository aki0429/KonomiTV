package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestClientSettingsAPIValidationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../config/testdata/client_settings_validation_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Input string
			Status      int             `json:"api_status"`
			Detail      json.RawMessage `json:"api_detail"`
			FullJSON    string          `json:"normalized_full_json"`
			GetStatus   int             `json:"get_status"`
			GetBody     string          `json:"get_body"`
			Body        string          `json:"body"`
			MIME        string          `json:"content_type"`
			Base64      string          `json:"input_base64"`
		}
		Unauthorized []struct {
			Input, Body string
			Status      int
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	server, db, _ := setupSettingsTestServer(t)
	userID := createTestUser(t, db, "settings_validation", "password", false)
	token := issueTestToken(t, userID)
	handler := server.Handler()
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if _, err := db.Exec(`UPDATE users SET client_settings='{}' WHERE id=?`, userID); err != nil {
				t.Fatal(err)
			}
			input := tc.Input
			if tc.Base64 != "" {
				decoded, _ := base64.StdEncoding.DecodeString(tc.Base64)
				input = string(decoded)
			}
			response := doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", input, token, tc.MIME)
			if response.Code != tc.Status {
				t.Fatalf("Go=%d %s upstream=%d", response.Code, response.Body.String(), tc.Status)
			}
			var stored string
			if err := db.QueryRow(`SELECT client_settings FROM users WHERE id=?`, userID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if tc.Status == 204 {
				if stored != tc.FullJSON {
					t.Fatalf("saved settings differ: Go=%s upstream=%s", stored, tc.FullJSON)
				}
				get := doJSONRequest(t, handler, http.MethodGet, "/api/settings/client", "", token, "")
				if get.Code != tc.GetStatus || strings.TrimSpace(get.Body.String()) != tc.GetBody {
					t.Fatalf("retrieved settings differ: Go=%d %s upstream=%d %s", get.Code, get.Body.String(), tc.GetStatus, tc.GetBody)
				}
			} else {
				if stored != "{}" {
					t.Fatal("invalid request changed saved settings")
				}
				if tc.Status == 422 {
					var actual struct{ Detail any }
					var expected any
					_ = json.Unmarshal(response.Body.Bytes(), &actual)
					_ = json.Unmarshal(tc.Detail, &expected)
					if !reflect.DeepEqual(actual.Detail, expected) {
						t.Fatalf("validation response differs: Go=%s upstream=%s", response.Body.String(), tc.Detail)
					}
				} else if tc.Status == 500 && response.Body.String() != tc.Body {
					t.Fatalf("internal error differs: Go=%s upstream=%s", response.Body.String(), tc.Body)
				} else if tc.Status == 400 && strings.TrimSpace(response.Body.String()) != tc.Body {
					t.Fatalf("body decoding failure differs: Go=%s upstream=%s", response.Body.String(), tc.Body)
				}
			}
		})
	}
	for _, tc := range fixture.Unauthorized {
		t.Run("unauthorized/"+tc.Input, func(t *testing.T) {
			response := doJSONRequest(t, handler, http.MethodPut, "/api/settings/client", tc.Input, "", "application/json")
			if response.Code != tc.Status || strings.TrimSpace(response.Body.String()) != tc.Body {
				t.Fatalf("dependency ordering differs: Go=%d %s upstream=%d %s", response.Code, response.Body.String(), tc.Status, tc.Body)
			}
		})
	}
	for _, invalid := range []string{`{"caption_font":false}`, `{"timetable_channel_width":"invalid"}`, `null`, `not JSON`} {
		t.Run("invalid stored settings/"+invalid, func(t *testing.T) {
			if _, err := db.Exec(`UPDATE users SET client_settings=? WHERE id=?`, invalid, userID); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				response := doJSONRequest(t, handler, method, "/api/settings/client", `{}`, token, "application/json")
				if response.Code != 500 || response.Body.String() != "Internal Server Error" {
					t.Fatalf("invalid stored settings were hidden: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
}
