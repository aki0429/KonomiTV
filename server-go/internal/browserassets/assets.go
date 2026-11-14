// Package browserassets は専用Chromeの配布物を検証し、アプリのディレクトリだけに導入する。
package browserassets

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version は配布物のSHA-256とともに固定したChrome for Testingのバージョン。
const Version = "157.0.8103.78"

const maxArchiveSize = 512 << 20

type release struct {
	Version, Platform, URL, SHA256, Executable string
}

// Options は保存先、再利用する配布ZIP、Ubuntu用の依存ファイルを指定する。
// UbuntuLibrariesはapt-get download / dpkg-deb -xだけを使い、OSにはインストールしない。
// AppArmorProfileは既存のプロファイル名。プロファイル自体は作成・変更しない。
type Options struct {
	Output          string
	Archive         string
	UbuntuLibraries bool
	AppArmorProfile string
}

// Manifest は取得元と全導入ファイルの内容ハッシュを記録する。
type Manifest struct {
	Release         release           `json:"release"`
	UbuntuLibraries bool              `json:"ubuntu_libraries"`
	AppArmorProfile string            `json:"apparmor_profile"`
	Files           map[string]string `json:"files"`
	Links           map[string]string `json:"links,omitempty"`
}

// Install は固定した公式配布物を導入する。既存の有効な導入先では再取得せず全ファイルを検証する。
// 既存の不明なディレクトリや異なる版を上書きしない。
func Install(ctx context.Context, options Options) (*Manifest, error) {
	selected, err := platformRelease(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	return install(ctx, options, selected)
}

func platformRelease(goos, goarch string) (release, error) {
	platform := ""
	executable, digest := "", ""
	switch goos + "/" + goarch {
	case "linux/amd64":
		platform, executable = "linux64", "chrome-linux64/chrome"
		digest = "9223832977a13db3646201b1341c1f9cf08afa3ceae92540116d7b0c062364fb"
	case "linux/arm64":
		platform, executable = "linux-arm64", "chrome-linux-arm64/chrome"
		digest = "f354d666822a11d8fe99e923c44be77dd226fab2750c5b5947e6318818bf10f1"
	case "windows/amd64":
		platform, executable = "win64", "chrome-win64/chrome.exe"
		digest = "5f6ec40206407112702e163bf694d07f9fdc8ccbd346c70ce61fa62f716493f9"
	default:
		return release{}, fmt.Errorf("no pinned Chrome archive for %s/%s; use a system Chrome or Brave browser", goos, goarch)
	}
	selected := release{Version: Version, Platform: platform, Executable: executable, SHA256: digest,
		URL: "https://storage.googleapis.com/chrome-for-testing-public/" + Version + "/" + platform + "/chrome-" + platform + ".zip"}
	return selected, nil
}

func install(ctx context.Context, options Options, selected release) (*Manifest, error) {
	output, err := filepath.Abs(options.Output)
	if err != nil || options.Output == "" {
		return nil, fmt.Errorf("invalid browser output directory")
	}
	if options.UbuntuLibraries && !strings.HasPrefix(selected.Platform, "linux") {
		return nil, fmt.Errorf("Ubuntu libraries require Linux")
	}
	if options.AppArmorProfile != "" {
		if !strings.HasPrefix(selected.Platform, "linux") || strings.ContainsAny(options.AppArmorProfile, "\x00\r\n") {
			return nil, fmt.Errorf("invalid AppArmor profile")
		}
		if _, err := exec.LookPath("aa-exec"); err != nil {
			return nil, fmt.Errorf("aa-exec is required for the selected AppArmor profile: %w", err)
		}
	}
	if _, err := os.Lstat(output); err == nil {
		data, err := os.ReadFile(filepath.Join(output, "assets-manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("existing browser directory has no readable manifest: %w", err)
		}
		var existing Manifest
		if err := json.Unmarshal(data, &existing); err != nil {
			return nil, fmt.Errorf("invalid installed browser manifest: %w", err)
		}
		if existing.Release != selected || existing.UbuntuLibraries != options.UbuntuLibraries || existing.AppArmorProfile != options.AppArmorProfile || len(existing.Files) == 0 || existing.Files[selected.Executable] == "" {
			return nil, fmt.Errorf("installed browser release or configuration differs")
		}
		for name, expected := range existing.Files {
			if !safeRelative(name) {
				return nil, fmt.Errorf("invalid installed browser path: %s", name)
			}
			actual, err := hashFile(filepath.Join(output, filepath.FromSlash(name)))
			if err != nil || actual != expected {
				return nil, fmt.Errorf("installed browser file verification failed: %s", name)
			}
		}
		for name, expected := range existing.Links {
			if !safeRelative(name) {
				return nil, fmt.Errorf("invalid installed browser link path: %s", name)
			}
			actual, err := os.Readlink(filepath.Join(output, filepath.FromSlash(name)))
			if err != nil || actual != expected {
				return nil, fmt.Errorf("installed browser link verification failed: %s", name)
			}
		}
		return &existing, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(output), ".browser-assets-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	archive := options.Archive
	if archive == "" {
		archive = filepath.Join(staging, "download.zip")
		if err := download(ctx, selected.URL, archive); err != nil {
			return nil, err
		}
	}
	digest, err := hashFile(archive)
	if err != nil || digest != selected.SHA256 {
		return nil, fmt.Errorf("browser archive SHA-256 mismatch")
	}
	payload := filepath.Join(staging, "payload")
	if err := extract(archive, payload); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(payload, filepath.FromSlash(selected.Executable))); err != nil {
		return nil, fmt.Errorf("browser executable is missing from archive: %w", err)
	}
	if options.UbuntuLibraries {
		if err := installUbuntuLibraries(ctx, staging, payload); err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(selected.Platform, "linux") {
		if err := writeLauncher(output, payload, options, selected); err != nil {
			return nil, err
		}
	}
	installed := &Manifest{Release: selected, UbuntuLibraries: options.UbuntuLibraries, AppArmorProfile: options.AppArmorProfile, Files: map[string]string{}, Links: map[string]string{}}
	if err := filepath.WalkDir(payload, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(payload, file)
		if err != nil {
			return err
		}
		// Ubuntuパッケージの共有ライブラリでは、リンク先の文字列と実ファイルの両方を記録する。
		if entry.Type()&os.ModeSymlink != 0 {
			installed.Links[filepath.ToSlash(relative)], err = os.Readlink(file)
			return err
		}
		installed.Files[filepath.ToSlash(relative)], err = hashFile(file)
		return err
	}); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(payload, "assets-manifest.json"), append(data, '\n'), 0644); err != nil {
		return nil, err
	}
	// 検査が終わるまでは公開先を作らず、最後に同じファイルシステム内で移動する。
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return nil, fmt.Errorf("browser output appeared during installation")
	}
	if err := os.Rename(payload, output); err != nil {
		return nil, err
	}
	return installed, nil
}

func download(ctx context.Context, url, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("browser archive download: HTTP %d", response.StatusCode)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(output, io.LimitReader(response.Body, maxArchiveSize+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxArchiveSize {
		return fmt.Errorf("browser archive exceeds size limit")
	}
	return nil
}

func safeRelative(name string) bool {
	return name != "." && !strings.Contains(name, "\\") && filepath.IsLocal(filepath.FromSlash(name))
}

func extract(archivePath, directory string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	var total uint64
	for _, file := range archive.File {
		if !safeRelative(file.Name) || file.Mode()&os.ModeSymlink != 0 || file.UncompressedSize64 > 1<<30 {
			return fmt.Errorf("invalid browser archive member: %s", file.Name)
		}
		total += file.UncompressedSize64
		if total > 2<<30 {
			return fmt.Errorf("browser archive uncompressed size exceeds limit")
		}
		target := filepath.Join(directory, filepath.FromSlash(file.Name))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if file.Mode()&0111 != 0 {
			mode = 0755
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			input.Close()
			return err
		}
		n, copyErr := io.Copy(output, io.LimitReader(input, int64(file.UncompressedSize64)+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if uint64(n) != file.UncompressedSize64 {
			return fmt.Errorf("browser archive member size mismatch: %s", file.Name)
		}
	}
	return nil
}

func hashFile(file string) (string, error) {
	input, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func installUbuntuLibraries(ctx context.Context, staging, payload string) error {
	packages := filepath.Join(staging, "packages")
	if err := os.Mkdir(packages, 0755); err != nil {
		return err
	}
	download := exec.CommandContext(ctx, "apt-get", "download", "libasound2t64", "libgbm1", "fonts-noto-cjk")
	download.Dir = packages
	if output, err := download.CombinedOutput(); err != nil {
		return fmt.Errorf("Ubuntu dependency download: %w: %s", err, output)
	}
	entries, err := os.ReadDir(packages)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".deb") {
			continue
		}
		command := exec.CommandContext(ctx, "dpkg-deb", "-x", filepath.Join(packages, entry.Name()), filepath.Join(payload, "libraries"))
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("Ubuntu dependency extraction: %w: %s", err, output)
		}
	}
	return nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func writeLauncher(output, payload string, options Options, selected release) error {
	launcher := "#!/bin/sh\n"
	if options.UbuntuLibraries {
		triplet := "x86_64-linux-gnu"
		if selected.Platform == "linux-arm64" {
			triplet = "aarch64-linux-gnu"
		}
		libraries := filepath.Join(output, "libraries", "usr", "lib", triplet)
		launcher += "export LD_LIBRARY_PATH=" + shellQuote(libraries) + ":\"${LD_LIBRARY_PATH:-}\"\n"
		// フォント検索先はアプリ専用の設定へ追加し、OSのfontconfigは変更しない。
		fontDirectory := filepath.Join(output, "libraries", "usr", "share", "fonts", "opentype", "noto")
		config := "<?xml version=\"1.0\"?><!DOCTYPE fontconfig SYSTEM \"fonts.dtd\"><fontconfig><include ignore_missing=\"yes\">/etc/fonts/fonts.conf</include><dir>" + xmlEscape(fontDirectory) + "</dir><cachedir prefix=\"xdg\">fontconfig</cachedir></fontconfig>"
		if err := os.WriteFile(filepath.Join(payload, "fonts.conf"), []byte(config), 0644); err != nil {
			return err
		}
		launcher += "export FONTCONFIG_FILE=" + shellQuote(filepath.Join(output, "fonts.conf")) + "\n"
	}
	launcher += "exec "
	if options.AppArmorProfile != "" {
		launcher += "aa-exec -p " + shellQuote(options.AppArmorProfile) + " -- "
	}
	launcher += shellQuote(filepath.Join(output, filepath.FromSlash(selected.Executable))) + " \"$@\"\n"
	return os.WriteFile(filepath.Join(payload, "chrome"), []byte(launcher), 0755)
}

func xmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(value)
}
