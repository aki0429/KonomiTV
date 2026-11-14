package browserassets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func testArchive(t *testing.T, name string, mode os.FileMode) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	header := &zip.FileHeader{Name: name}
	header.SetMode(mode)
	file, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("browser fixture")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func fixtureRelease(data []byte) release {
	hash := sha256.Sum256(data)
	return release{Version: "fixture", Platform: "fixture", SHA256: hex.EncodeToString(hash[:]), Executable: "chrome/browser"}
}

func TestVerifiedInstallCachesAndDetectsChanges(t *testing.T) {
	data := testArchive(t, "chrome/browser", 0755)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	selected := fixtureRelease(data)
	selected.URL = server.URL
	options := Options{Output: filepath.Join(t.TempDir(), "installed")}
	manifest, err := install(context.Background(), options, selected)
	if err != nil || manifest == nil || len(manifest.Files) != 1 {
		t.Fatalf("install: manifest=%+v error=%v", manifest, err)
	}
	if _, err := install(context.Background(), options, selected); err != nil || requests.Load() != 1 {
		t.Fatalf("cached verification: requests=%d error=%v", requests.Load(), err)
	}
	if err := os.WriteFile(filepath.Join(options.Output, "chrome", "browser"), []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := install(context.Background(), options, selected); err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("changed file was accepted: %v", err)
	}
}

func TestInvalidArchivesDoNotPublishOrEscape(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{"../escaped", 0644}, {"/escaped", 0644}, {"chrome/../../escaped", 0644},
		{"chrome\\..\\escaped", 0644}, {"chrome/browser", os.ModeSymlink | 0777},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "input.zip")
			data := testArchive(t, test.name, test.mode)
			if err := os.WriteFile(archive, data, 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "output")
			if _, err := install(context.Background(), Options{Output: output, Archive: archive}, fixtureRelease(data)); err == nil {
				t.Fatal("invalid archive was accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("failed installation published output: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
				t.Fatalf("archive escaped the staging directory: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary files were left behind: %v %v", entries, err)
			}
		})
	}
}

func TestCorruptArchiveAndExistingDirectoryArePreserved(t *testing.T) {
	root := t.TempDir()
	data := testArchive(t, "chrome/browser", 0755)
	archive := filepath.Join(root, "input.zip")
	if err := os.WriteFile(archive, append(data, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output")
	options := Options{Output: output, Archive: archive}
	if _, err := install(context.Background(), options, fixtureRelease(data)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("archive hash was not verified: %v", err)
	}
	if err := os.Mkdir(output, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(output, "user-file")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := install(context.Background(), options, fixtureRelease(data)); err == nil {
		t.Fatal("unmanaged output was accepted")
	}
	if preserved, err := os.ReadFile(sentinel); err != nil || string(preserved) != "keep" {
		t.Fatalf("existing content changed: %q %v", preserved, err)
	}
}

func TestDownloadFailureAndCancellationDoNotPublish(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	selected := fixtureRelease(testArchive(t, "chrome/browser", 0755))
	selected.URL = server.URL
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		output := filepath.Join(t.TempDir(), "output")
		_, err := install(ctx, Options{Output: output}, selected)
		cancel()
		if err == nil {
			t.Fatal("failed download was accepted")
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("failed download published output: %v", err)
		}
	}
}

func TestLinuxLauncherQuotesPathsAndKeepsSandbox(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "chrome with ' and & spaces")
	selected, err := platformRelease("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLauncher(output, root, Options{UbuntuLibraries: true, AppArmorProfile: "chrome"}, selected); err != nil {
		t.Fatal(err)
	}
	launcher, err := os.ReadFile(filepath.Join(root, "chrome"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launcher), "aa-exec -p 'chrome' --") || !strings.Contains(string(launcher), "'\"'\"'") || strings.Contains(string(launcher), "--no-sandbox") {
		t.Fatalf("unexpected launcher: %s", launcher)
	}
	if !strings.Contains(string(launcher), "aarch64-linux-gnu") || !strings.Contains(string(launcher), filepath.Join("chrome-linux-arm64", "chrome")) {
		t.Fatalf("ARM launcher uses the wrong architecture: %s", launcher)
	}
	fonts, err := os.ReadFile(filepath.Join(root, "fonts.conf"))
	if err != nil || !strings.Contains(string(fonts), "&amp;") || !strings.Contains(string(fonts), "&apos;") {
		t.Fatalf("font paths were not XML escaped: %q %v", fonts, err)
	}
}

// TestOfficialBrowserArchivesは取得済みの実配布ZIPを各OS向けのまま展開・再検証する。
// 外部ネットワークへは接続せず、他OSのバイナリも実行しない。
func TestOfficialBrowserArchives(t *testing.T) {
	directory := os.Getenv("KONOMITV_TEST_BROWSER_ARCHIVES")
	if directory == "" {
		t.Skip("KONOMITV_TEST_BROWSER_ARCHIVES is not set")
	}
	count := 0
	for _, target := range [][2]string{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		selected, err := platformRelease(target[0], target[1])
		if err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(directory, "chrome-"+selected.Platform+"-"+Version+".zip")
		if _, err := os.Stat(archive); os.IsNotExist(err) {
			continue
		}
		count++
		t.Run(selected.Platform, func(t *testing.T) {
			options := Options{Output: filepath.Join(t.TempDir(), "installed"), Archive: archive}
			manifest, err := install(context.Background(), options, selected)
			if err != nil || manifest == nil || len(manifest.Files) < 100 {
				t.Fatalf("official archive: manifest=%v error=%v", manifest != nil, err)
			}
			if _, err := install(context.Background(), options, selected); err != nil {
				t.Fatalf("official archive cached verification: %v", err)
			}
			input, err := os.Open(filepath.Join(options.Output, filepath.FromSlash(selected.Executable)))
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 20)
			_, err = io.ReadFull(input, data)
			input.Close()
			if err != nil {
				t.Fatal(err)
			}
			if target[0] == "windows" && !bytes.HasPrefix(data, []byte("MZ")) {
				t.Fatal("Windows archive does not contain a PE executable")
			}
			if target[0] == "linux" && (len(data) < 20 || !bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'})) {
				t.Fatal("Linux archive does not contain an ELF executable")
			}
			if target[1] == "arm64" && (data[18] != 183 || data[19] != 0) {
				t.Fatal("ARM archive does not contain an AArch64 executable")
			}
		})
	}
	if count == 0 {
		t.Fatal("no official browser archives found")
	}
}
