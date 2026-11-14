package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

type settingsRoundTripper func(*http.Request) (*http.Response, error)

func (f settingsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServerSettingsValidationMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/server_settings_validation_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Input, Body string
			Status            int
			Bypass            bool
			Detail            []SettingsValidationError
			Calls             []string
			Options           struct {
				Arch            string
				EDCBStatus      string  `json:"edcb_status"`
				EncoderStdout   *string `json:"encoder_stdout"`
				EncoderError    bool    `json:"encoder_error"`
				MirakurunError  bool    `json:"mirakurun_error"`
				MirakurunStatus int     `json:"mirakurun_status"`
				MirakurunServer *string `json:"mirakurun_server"`
				MirakurunBody   *string `json:"mirakurun_body"`
				Listeners       []struct {
					PID  *int
					Port int
				}
				Parents map[string]int
			}
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "settings-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings-file.ts"), []byte("ts"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			calls := []string{}
			options := ServerSettingsValidationOptions{BypassEnvironment: tc.Bypass, Architecture: tc.Options.Arch, CurrentPID: 100, ParentPID: 50,
				Stat: func(name string) (fs.FileInfo, error) {
					if name == "denied" {
						return nil, fs.ErrPermission
					}
					return os.Stat(filepath.Join(root, name))
				},
				LocalSystem: func() (bool, error) { return false, nil },
				EDCBStatus: func(_ context.Context, url string) string {
					calls = append(calls, "edcb:"+url)
					if tc.Options.EDCBStatus != "" {
						return tc.Options.EDCBStatus
					}
					return "Normal"
				},
				RunEncoder: func(_ context.Context, path, arg string, _ bool) ([]byte, error) {
					calls = append(calls, "encoder:"+path+":"+arg)
					if tc.Options.EncoderError {
						return nil, fs.ErrNotExist
					}
					if tc.Options.EncoderStdout != nil {
						return []byte(*tc.Options.EncoderStdout), nil
					}
					return []byte("H.264/AVC H.265/HEVC"), nil
				},
				Listeners: func(context.Context) ([]SettingsListener, error) {
					result := []SettingsListener{}
					for _, l := range tc.Options.Listeners {
						pid := 0
						if l.PID != nil {
							pid = *l.PID
						}
						parent, known := tc.Options.Parents[fmt.Sprint(pid)]
						result = append(result, SettingsListener{Port: l.Port, PID: pid, ParentPID: parent, ParentKnown: known})
					}
					return result, nil
				},
			}
			// 期待値のアーキテクチャーを固定して、Linux 側でも同じ検証分岐を通す。
			if options.Architecture == "" {
				options.Architecture = "AMD64"
			}
			options.HTTPClient = &http.Client{Transport: settingsRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, "http:"+r.URL.String())
				if tc.Options.MirakurunError {
					return nil, fmt.Errorf("connection refused")
				}
				status := tc.Options.MirakurunStatus
				if status == 0 {
					status = 200
				}
				server := "mirakc/3.4.4"
				if tc.Options.MirakurunServer != nil {
					server = *tc.Options.MirakurunServer
				}
				body := "[]"
				if tc.Options.MirakurunBody != nil {
					body = *tc.Options.MirakurunBody
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Server": []string{server}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			body, details := ParseSettingsBody([]byte(tc.Input), "application/json")
			if len(details) > 0 {
				t.Fatal(details)
			}
			actual, details, fatal := body.ServerSettings(context.Background(), options)
			if !reflect.DeepEqual(calls, tc.Calls) {
				t.Errorf("environment checks differ: Go=%v upstream=%v", calls, tc.Calls)
			}
			if tc.Status == 200 {
				if fatal != nil || len(details) > 0 {
					t.Fatalf("Go rejected accepted input: %v %v", details, fatal)
				}
				serialized, err := MarshalServerSettingsResponse(actual)
				want := tc.Body
				// pathlib の区切り文字だけは実行 OS に従う。
				if runtime.GOOS != "windows" && strings.Contains(tc.Name, "settings-dir/../settings-dir") {
					want = strings.ReplaceAll(want, `settings-dir\\..\\settings-dir`, `settings-dir/../settings-dir`)
				}
				if err != nil || serialized != want {
					t.Fatalf("normalized response differs:\nGo=%s\nupstream=%s\nerror=%v", serialized, want, err)
				}
			} else if tc.Status == 500 {
				if fatal == nil {
					if _, err := SettingsValidationJSON(details); err == nil {
						t.Fatalf("expected an internal error: %v", details)
					}
				}
			} else {
				if fatal != nil {
					t.Fatalf("unexpected fatal error: %v", fatal)
				}
				got, _ := json.Marshal(details)
				want, _ := json.Marshal(tc.Detail)
				if !reflect.DeepEqual(canonicalSettingsJSON(got), canonicalSettingsJSON(want)) {
					t.Fatalf("errors differ: Go=%s upstream=%s", got, want)
				}
			}
		})
	}
}

func TestSettingsListeningPortsOwnSocket(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "tcp6" {
				address = "[::1]:0"
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				if network == "tcp6" {
					t.Skip("IPv6 is unavailable")
				}
				t.Fatal(err)
			}
			defer listener.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			listeners, err := settingsListeningPorts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range listeners {
				if entry.Port == port && entry.PID == os.Getpid() && entry.ParentKnown && entry.ParentPID == os.Getppid() {
					return
				}
			}
			t.Fatalf("own %s listening socket and parent were not detected (port %d)", network, port)
		})
	}
}

func TestServerSettingsFolderAccountsAndIOErrors(t *testing.T) {
	for _, localSystem := range []bool{false, true} {
		body, _ := ParseSettingsBody([]byte(`{"video":{"recorded_folders":["denied"]}}`), "application/json")
		_, details, fatal := body.ServerSettings(t.Context(), ServerSettingsValidationOptions{
			Stat: func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }, LocalSystem: func() (bool, error) { return localSystem, nil },
		})
		if fatal != nil || len(details) != 1 || details[0].Type != "value_error" || len(details[0].Loc) != 3 {
			t.Fatalf("access failure was not attributed to the folder list: %v %v", details, fatal)
		}
		if strings.Contains(details[0].Msg, "LocalSystem") != localSystem {
			t.Fatalf("wrong account guidance: %v", details)
		}
	}
	body, _ := ParseSettingsBody([]byte(`{"server":{"custom_https_certificate":"io-error"}}`), "application/json")
	_, details, fatal := body.ServerSettings(t.Context(), ServerSettingsValidationOptions{Stat: func(string) (fs.FileInfo, error) { return nil, syscall.EIO }})
	if fatal == nil || len(details) != 0 {
		t.Fatalf("unexpected I/O error was hidden as a missing file: %v %v", details, fatal)
	}
}

func TestSaveValidatedServerSettingsDockerAndNonFinite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("general: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body, details := ParseSettingsBody([]byte(`{"general":{"program_update_interval":NaN},"video":{"recorded_folders":["/host-rootfs/a"],"exclude_scan_paths":["/host-rootfs/a/host-rootfs/b"]},"capture":{"upload_folders":["/host-rootfs/c"]},"tv":{"debug_mode_ts_path":"/host-rootfs/d"}}`), "application/json")
	if len(details) > 0 {
		t.Fatal(details)
	}
	file, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	settings, details, fatal := body.ServerSettings(t.Context(), ServerSettingsValidationOptions{BypassEnvironment: true, Stat: func(name string) (fs.FileInfo, error) {
		if strings.HasSuffix(name, "d") {
			return file, nil
		}
		return directory, nil
	}})
	if fatal != nil || len(details) > 0 {
		t.Fatalf("fixture rejected: %v %v", details, fatal)
	}
	// Docker で検証済みの POSIX パスを Windows の開発機でも再現する。
	for _, entry := range [][2]string{{"video", "recorded_folders"}, {"capture", "upload_folders"}} {
		section := settings[entry[0]].(map[string]any)
		for i, item := range section[entry[1]].([]any) {
			section[entry[1]].([]any)[i] = filepath.ToSlash(item.(string))
		}
	}
	settings["tv"].(map[string]any)["debug_mode_ts_path"] = "/host-rootfs/d"
	if err := SaveValidatedServerSettings(path, settings, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	video := saved["video"].(map[string]any)
	if video["exclude_scan_paths"].([]any)[0] != "/a/b" || saved["capture"].(map[string]any)["upload_folders"].([]any)[0] != "/c" || saved["tv"].(map[string]any)["debug_mode_ts_path"] != "/d" || !strings.Contains(string(data), ".nan") {
		t.Fatalf("Docker paths or NaN were not saved correctly: %s", data)
	}
	if settings["video"].(map[string]any)["exclude_scan_paths"].([]any)[0] != "/host-rootfs/a/host-rootfs/b" {
		t.Fatal("save mutated the validated settings")
	}
}
