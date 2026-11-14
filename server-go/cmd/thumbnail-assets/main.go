// thumbnail-assets は本家 PyAV と同じ配布物から FFmpeg の共有ライブラリだけを導入する。
// Python 本体、PyAV モジュール、pip/uv は実行時に必要ない。
package main

import (
	"archive/zip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed libraries.json
var libraries []byte

type release struct{ Filename, URL, SHA256 string }
type manifest struct {
	Release release
	Files   map[string]string
}

func main() {
	destination := flag.String("output", "../server/go-native/FFmpeg-Thumbnail", "library installation directory")
	flag.Parse()
	if err := install(*destination); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func install(destination string) error {
	platform := map[string]string{"linux/amd64": "manylinux_2_28_x86_64", "linux/arm64": "manylinux_2_28_aarch64", "windows/amd64": "win_amd64", "windows/arm64": "win_arm64"}[runtime.GOOS+"/"+runtime.GOARCH]
	if platform == "" {
		return fmt.Errorf("unsupported thumbnail library platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	var releases []release
	if err := json.Unmarshal(libraries, &releases); err != nil {
		return err
	}
	var selected release
	for _, r := range releases {
		if strings.HasSuffix(r.Filename, "-"+platform+".whl") {
			selected = r
		}
	}
	if selected.URL == "" {
		return fmt.Errorf("thumbnail library release not found")
	}
	if data, err := os.ReadFile(filepath.Join(destination, "assets-manifest.json")); err == nil {
		var existing manifest
		if err := json.Unmarshal(data, &existing); err != nil {
			return err
		}
		if existing.Release.SHA256 != selected.SHA256 || len(existing.Files) == 0 {
			return fmt.Errorf("installed thumbnail libraries have a different release")
		}
		for name, expected := range existing.Files {
			if !filepath.IsLocal(name) {
				return fmt.Errorf("invalid thumbnail library manifest path")
			}
			actual, err := hashFile(filepath.Join(destination, name))
			if err != nil {
				return err
			}
			if actual != expected {
				return fmt.Errorf("installed thumbnail library hash mismatch: %s", name)
			}
		}
		fmt.Println("Thumbnail libraries verified:", selected.Filename)
		return nil
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("thumbnail library directory already exists without a valid manifest: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".thumbnail-assets-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	wheel, err := os.CreateTemp("", "konomitv-av-*.whl")
	if err != nil {
		return err
	}
	defer os.Remove(wheel.Name())
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Get(selected.URL)
	if err != nil {
		wheel.Close()
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		wheel.Close()
		return fmt.Errorf("thumbnail library download: HTTP %d", response.StatusCode)
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(wheel, hash), io.LimitReader(response.Body, 256<<20))
	closeErr := wheel.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n >= 256<<20 || hex.EncodeToString(hash.Sum(nil)) != selected.SHA256 {
		return fmt.Errorf("thumbnail library archive SHA-256 mismatch")
	}
	archive, err := zip.OpenReader(wheel.Name())
	if err != nil {
		return err
	}
	defer archive.Close()
	installed := manifest{Release: selected, Files: map[string]string{}}
	for _, file := range archive.File {
		name := path.Clean(file.Name)
		if file.FileInfo().IsDir() || !(strings.HasPrefix(name, "av.libs/") || strings.HasPrefix(name, "av-19.0.2.dist-info/")) {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) || file.UncompressedSize64 > 256<<20 {
			return fmt.Errorf("invalid thumbnail archive member: %s", name)
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("thumbnail archive contains a symlink: %s", name)
		}
		target := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, 256<<20))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		installed.Files[name], err = hashFile(target)
		if err != nil {
			return err
		}
	}
	if len(installed.Files) < 5 {
		return fmt.Errorf("thumbnail archive contains no usable library bundle")
	}
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "assets-manifest.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	fmt.Println("Thumbnail libraries installed:", selected.Filename)
	return nil
}

func hashFile(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
