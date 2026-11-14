package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLoadValidatedMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/server_startup_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Input, Body     string
			Valid, Bypass, Docker bool
			Calls                 []string
			ErrorTypes            []string `json:"error_types"`
			ErrorLocs             [][]any  `json:"error_locs"`
			Options               struct {
				EDCBStatus     string `json:"edcb_status"`
				EncoderError   bool   `json:"encoder_error"`
				MirakurunError bool   `json:"mirakurun_error"`
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, "settings-dir")
	file := filepath.Join(root, "settings-file.ts")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("ts"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			calls := []string{}
			options := ServerSettingsValidationOptions{BypassEnvironment: tc.Bypass, Architecture: "AMD64", CurrentPID: 100, ParentPID: 50,
				EDCBStatus: func(_ context.Context, url string) string {
					calls = append(calls, "edcb:"+url)
					if tc.Options.EDCBStatus != "" {
						return tc.Options.EDCBStatus
					}
					return "Normal"
				},
				RunEncoder: func(_ context.Context, path, argument string, _ bool) ([]byte, error) {
					calls = append(calls, "encoder:"+path+":"+argument)
					if tc.Options.EncoderError {
						return nil, fs.ErrNotExist
					}
					return []byte("H.264/AVC H.265/HEVC"), nil
				},
				Listeners:   func(context.Context) ([]SettingsListener, error) { return nil, nil },
				LocalSystem: func() (bool, error) { return false, nil },
				Stat: func(path string) (fs.FileInfo, error) {
					if path == "denied" {
						return nil, fs.ErrPermission
					}
					if strings.HasPrefix(filepath.ToSlash(path), "/host-rootfs") {
						if strings.HasSuffix(path, ".ts") {
							return os.Stat(file)
						}
						return os.Stat(directory)
					}
					if filepath.IsAbs(path) {
						return nil, fs.ErrNotExist
					}
					return os.Stat(filepath.Join(root, path))
				},
				HTTPClient: &http.Client{Transport: settingsRoundTripper(func(request *http.Request) (*http.Response, error) {
					calls = append(calls, "http:"+request.URL.String())
					if tc.Options.MirakurunError {
						return nil, fmt.Errorf("connection refused")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Server": []string{"mirakc/3.4.4"}}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
				})},
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.Input), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadValidated(t.Context(), path, ServerSettingsLoadOptions{Validation: options, Docker: tc.Docker})
			if !reflect.DeepEqual(calls, tc.Calls) {
				t.Errorf("startup hooks differ: Go=%v upstream=%v", calls, tc.Calls)
			}
			if tc.Valid {
				if err != nil {
					t.Fatalf("Go rejected accepted YAML: %v", err)
				}
				serialized, err := MarshalServerSettingsResponse(config.ServerSettingsSnapshot())
				if err != nil {
					t.Fatal(err)
				}
				want := tc.Body
				if runtime.GOOS != "windows" && tc.Docker {
					want = strings.ReplaceAll(want, `\\host-rootfs\\`, `/host-rootfs/`)
				}
				if serialized != want {
					t.Fatalf("startup model differs:\nGo=%s\nupstream=%s", serialized, want)
				}
				// GET 用のコピーを編集しても、起動時設定や内部の実行用設定を変えない。
				snapshot := config.ServerSettingsSnapshot()
				snapshot["general"].(map[string]any)["encoder"] = "changed"
				if config.ServerSettingsSnapshot()["general"].(map[string]any)["encoder"] == "changed" || config.General.Encoder == "changed" {
					t.Fatal("startup snapshot was shared with its caller")
				}
			} else {
				if err == nil {
					t.Fatalf("Go accepted rejected YAML: %s", tc.Input)
				}
				if len(tc.ErrorTypes) > 0 {
					var loadError *ServerSettingsLoadError
					if !errors.As(err, &loadError) {
						t.Fatal(err)
					}
					types := []string{}
					locs := [][]any{}
					for _, detail := range loadError.Details {
						types = append(types, detail.Type)
						locs = append(locs, detail.Loc)
					}
					gotLocations, _ := json.Marshal(locs)
					wantLocations, _ := json.Marshal(tc.ErrorLocs)
					if !reflect.DeepEqual(types, tc.ErrorTypes) || !reflect.DeepEqual(canonicalSettingsJSON(gotLocations), canonicalSettingsJSON(wantLocations)) {
						t.Errorf("startup errors differ: Go=%v %v upstream=%v %v (error %v)", types, locs, tc.ErrorTypes, tc.ErrorLocs, err)
					}
				}
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != tc.Input {
				t.Fatalf("startup modified the settings file: %v", readErr)
			}
		})
	}
}

func TestLoadValidatedLegacyIPTVIsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("general: {backend: IPTV}\niptv: {enabled: true, sources: [playlist.m3u], cache_ttl: 60}")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	options := ServerSettingsLoadOptions{Validation: ServerSettingsValidationOptions{BypassEnvironment: true}}
	if _, err := LoadValidated(t.Context(), path, options); err == nil {
		t.Fatal("upstream mode accepted a Go-specific backend")
	}
	options.AllowLegacyIPTV = true
	config, err := LoadValidated(t.Context(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	if config.General.Backend != "IPTV" || config.IPTV.CacheTTL != 60 || len(config.IPTV.Sources) != 1 || config.IPTV.Sources[0] != "playlist.m3u" {
		t.Fatal(config)
	}
	body, _ := ParseSettingsBody([]byte(`{"general":{"backend":"IPTV"}}`), "application/json")
	if _, details, _ := body.ServerSettings(t.Context(), ServerSettingsValidationOptions{BypassEnvironment: true}); len(details) != 1 || details[0].Type != "literal_error" {
		t.Fatal("legacy loader support leaked into the settings API")
	}
}
