package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 起動経路の試験では実機のエンコーダーを使わず、同じテスト実行ファイルに
// --version への応答だけを担当させる。配布用バイナリには含まれない。
func TestMain(m *testing.M) {
	if os.Getenv("KONOMITV_TEST_ENCODER_FIXTURE") == "1" && len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("ffmpeg synthetic startup fixture")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func prepareStartupEncoder(t *testing.T, serverDir string) {
	t.Helper()
	extension := ".elf"
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	directory := filepath.Join(serverDir, "thirdparty", "FFmpeg")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "ffmpeg"+extension)
	// Windows は実行中イメージへのハードリンクも削除できないため、独立したコピーにする。
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_TEST_ENCODER_FIXTURE", "1")
}

// 親の二つの待ち受けを維持し、子 main が本家と同じ関連プロセス除外を通るようにする。
func startupFixturePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		front, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := front.Addr().(*net.TCPAddr).Port
		if port > 65525 {
			front.Close()
			continue
		}
		internal, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port+10))
		if err != nil {
			front.Close()
			continue
		}
		t.Cleanup(func() { front.Close(); internal.Close() })
		return port
	}
	t.Fatal("could not reserve startup fixture ports")
	return 0
}

func TestMainInvalidSettingsStopsBeforeDatabase(t *testing.T) {
	if os.Getenv("KONOMITV_TEST_INVALID_SETTINGS_MAIN") == "1" {
		flag.CommandLine = flag.NewFlagSet("konomitv-go", flag.ExitOnError)
		os.Args = []string{"konomitv-go", "-server-dir", os.Getenv("KONOMITV_TEST_SERVER_DIR"), "-no-proxy"}
		main()
		return
	}
	for _, tc := range []struct {
		name, extra string
		encoder     bool
		want        string
	}{
		{"invalid polling type", "video: {recorded_folders_polling_interval: '5'}\n", true, "Input should be a valid number"},
		{"missing encoder", "", false, "Failed to load server settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			serverDir := filepath.Join(root, "server")
			if err := os.Mkdir(serverDir, 0700); err != nil {
				t.Fatal(err)
			}
			if tc.encoder {
				prepareStartupEncoder(t, serverDir)
			}
			content := fmt.Sprintf("general: {backend: IPTV}\nserver: {port: %d}\n%s", startupFixturePort(t), tc.extra)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainInvalidSettingsStopsBeforeDatabase$")
			command.Env = append(os.Environ(), "KONOMITV_TEST_INVALID_SETTINGS_MAIN=1", "KONOMITV_TEST_SERVER_DIR="+serverDir)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil || err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("startup gate did not reject invalid configuration: %v %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(serverDir, "data", "database.sqlite")); !os.IsNotExist(err) {
				t.Fatal("invalid configuration created a database")
			}
			log, err := os.ReadFile(filepath.Join(serverDir, "logs", "KonomiTV-Server.log"))
			if err != nil || !strings.Contains(string(log), tc.want) {
				t.Fatalf("startup validation failure was not written to the server log: %v %s", err, log)
			}
		})
	}
}
