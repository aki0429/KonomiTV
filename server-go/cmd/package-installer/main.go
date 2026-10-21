// package-installer は監査済みソース・クライアントから自己完結ペイロードを作る内部ビルドツール。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	target := flag.String("os", "windows", "windows or linux")
	out := flag.String("out", "../../packaging-output", "output directory")
	flag.Parse()
	if *target != "windows" && *target != "linux" {
		return fmt.Errorf("invalid target")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	output, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(output, 0755); err != nil {
		return err
	}
	ext := ""
	if *target == "windows" {
		ext = ".exe"
	}
	core := filepath.Join(output, "konomitv-go-"+*target+ext)
	base := filepath.Join(output, "installer-base-"+*target+ext)
	cacheTool := filepath.Join(output, "keyframe-cache-"+*target+ext)
	// テスト用のloaded設定スナップショット（録画パス等）は製品バイナリへ埋め込まない。
	// ソースや本体側の並行作業を変えず、GoのoverlayでdefaultsだけのJSONを使う。
	fixturePath := filepath.Join(root, "server-go", "internal", "config", "server_settings_defaults.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(fixture, &fields); err != nil {
		return err
	}
	if len(fields["defaults"]) == 0 {
		return fmt.Errorf("settings defaults missing")
	}
	sanitized, err := json.Marshal(map[string]json.RawMessage{"defaults": fields["defaults"]})
	if err != nil {
		return err
	}
	sanitizedPath := filepath.Join(output, "production-defaults.json")
	if err = os.WriteFile(sanitizedPath, sanitized, 0600); err != nil {
		return err
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{fixturePath: sanitizedPath}})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(output, "build-overlay.json")
	if err = os.WriteFile(overlayPath, overlay, 0600); err != nil {
		return err
	}
	for pkg, p := range map[string]string{"./cmd/konomitv-go": core, "./cmd/installer": base, "./cmd/keyframe-cache": cacheTool} {
		cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-overlay", overlayPath, "-o", p, pkg)
		cmd.Env = append(os.Environ(), "GOOS="+*target, "GOARCH=amd64", "CGO_ENABLED=0")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Run(); err != nil {
			return err
		}
	}
	asset := filepath.Join(output, "KonomiTV-Go-Installer-akki-0.14.1-beta0.1-"+*target+"-amd64"+ext)
	dest, err := os.OpenFile(asset, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer dest.Close()
	r, err := os.Open(base)
	if err != nil {
		return err
	}
	_, err = io.Copy(dest, r)
	r.Close()
	if err != nil {
		return err
	}
	z := zip.NewWriter(dest)
	add := func(source, name string, mode fs.FileMode) error {
		f, e := os.Open(source)
		if e != nil {
			return e
		}
		defer f.Close()
		h := &zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Deflate}
		h.SetMode(mode)
		w, e := z.CreateHeader(h)
		if e != nil {
			return e
		}
		_, e = io.Copy(w, f)
		return e
	}
	tree := func(source, prefix string) error {
		return filepath.WalkDir(source, func(p string, e fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if e.IsDir() {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("payload source must be regular: %s", p)
			}
			rel, _ := filepath.Rel(source, p)
			return add(p, filepath.Join(prefix, rel), 0644)
		})
	}
	if err = add(core, "bin/konomitv-go"+ext, 0755); err != nil {
		return err
	}
	if err = add(cacheTool, "bin/keyframe-cache"+ext, 0755); err != nil {
		return err
	}
	for _, variant := range []string{"iptv", "noiptv"} {
		if err = tree(filepath.Join(output, "client-"+variant), "client-"+variant); err != nil {
			return err
		}
	}
	if err = tree(filepath.Join(root, "server", "static"), "server/static"); err != nil {
		return err
	}
	if err = add(filepath.Join(root, "License.txt"), "License.txt", 0644); err != nil {
		return err
	}
	if err = add(filepath.Join(root, "packaging", "README.md"), "INSTALLATION.md", 0644); err != nil {
		return err
	}
	if err = tree(filepath.Join(output, "licenses"), "licenses"); err != nil {
		return err
	}
	if err = z.Close(); err != nil {
		return err
	}
	if err = dest.Close(); err != nil {
		return err
	}
	if *target == "linux" {
		data, err := os.ReadFile(asset)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		header := "#!/usr/bin/env bash\nset -euo pipefail\n[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || { echo 'Linux x86_64 required' >&2; exit 1; }\ncommand -v sha256sum >/dev/null || { echo 'sha256sum is required' >&2; exit 1; }\ntmp=$(mktemp -d)\ntrap 'rm -f -- \"$tmp/installer\"; rmdir -- \"$tmp\"' EXIT\ntail -n +PAYLOAD_LINE -- \"$0\" > \"$tmp/installer\"\nprintf '%s  %s\\n' 'HASH' \"$tmp/installer\" | sha256sum -c - >/dev/null\nchmod 700 \"$tmp/installer\"\nstatus=0\n\"$tmp/installer\" \"$@\" || status=$?\nexit \"$status\"\n__PAYLOAD__\n"
		header = strings.ReplaceAll(header, "HASH", hex.EncodeToString(hash[:]))
		header = strings.ReplaceAll(header, "PAYLOAD_LINE", fmt.Sprint(strings.Count(header, "\n")+1))
		sh := strings.TrimSuffix(asset, ext) + ".sh"
		if err = os.WriteFile(sh, append([]byte(header), data...), 0755); err != nil {
			return err
		}
	}
	fmt.Println(asset)
	return nil
}
