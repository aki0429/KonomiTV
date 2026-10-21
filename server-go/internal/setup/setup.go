// Package setup は新規の専用ディレクトリにだけインストールする。既存環境の更新や削除は行わない。
package setup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/config"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Version はインストーラーの表示バージョン。サーバーAPI互換バージョンは0.14.1を維持する。
const Version = "akki-0.14.1-β0.1"

// schema は既存のPython移行0〜12のupgrade SQLを番号順に連結したもの。新規DB専用である。
//
//go:embed schema.sql
var schema string

// Options は新規インストールの選択。既存のパスを更新対象に流用しない。
type Options struct {
	Directory         string
	IPTV              bool
	Backend           string
	BackendURL        string
	Recordings        []string
	DependencyArchive string
}

// Dependency は公式Actions成果物の固定IDと配布アーカイブのSHA256。
type Dependency struct{ URL, SHA256, Inner string }

// OfficialDependency はOSごとに検証済みの固定成果物を返す。期限切れ時に最新版へフォールバックしない。
func OfficialDependency() (Dependency, error) {
	if runtime.GOARCH != "amd64" {
		return Dependency{}, fmt.Errorf("only amd64 is supported")
	}
	switch runtime.GOOS {
	case "windows":
		return Dependency{"https://nightly.link/tsukumijima/KonomiTV/actions/runs/39397337417/thirdparty-windows.7z.zip", "f0704fea273fe7961cc3251ee97e9dca105d2c58e574b14cb48983cd7c8a160f", "thirdparty-windows.7z"}, nil
	case "linux":
		return Dependency{"https://nightly.link/tsukumijima/KonomiTV/actions/runs/39397337417/thirdparty-linux.tar.xz.zip", "e1274c0971ba9fd2e5fc20eef73b5cc7027b56a494ed8fdee6be9802d4dc4a76", "thirdparty-linux.tar.xz"}, nil
	default:
		return Dependency{}, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// Manifest はインストール済みファイルだけをハッシュ付きで記録する。ユーザーデータは含めない。
type Manifest struct {
	Version string            `json:"version"`
	IPTV    bool              `json:"iptv"`
	Files   map[string]string `json:"files"`
}

// CreateDatabase は空の新規ファイルにだけ公式移行SQLを適用する。既存DBを開く前に拒否する。
func CreateDatabase(path string) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("database must be new: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(path)
		}
	}()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	// 初期移行のDROPは既存DBには適用されない。全SQLを1トランザクションで完了させる。
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(schema); err != nil {
		return fmt.Errorf("initialize clean schema: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	ok = true
	return nil
}

func safePath(root, name string) (string, error) {
	name = filepath.FromSlash(name)
	if name == "." || filepath.IsAbs(name) || strings.Contains(name, ":") {
		return "", fmt.Errorf("unsafe archive path: %q", name)
	}
	p := filepath.Join(root, name)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe archive path: %q", name)
	}
	return p, nil
}

func writeNew(path string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// ExtractPayload は選んだフロントエンドだけを配置する。ZIP内のリンクやトラバーサルは拒否する。
func ExtractPayload(z *zip.Reader, root string, iptv bool) error {
	wanted := "client-noiptv/"
	if iptv {
		wanted = "client-iptv/"
	}
	for _, f := range z.File {
		name := f.Name
		if strings.HasPrefix(name, "client-iptv/") || strings.HasPrefix(name, "client-noiptv/") {
			if !strings.HasPrefix(name, wanted) {
				continue
			}
			name = "client/dist/" + strings.TrimPrefix(name, wanted)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&os.ModeType != 0 {
			return fmt.Errorf("payload contains a link or special file: %s", f.Name)
		}
		if f.UncompressedSize64 > 512<<20 {
			return fmt.Errorf("payload entry too large: %s", f.Name)
		}
		p, err := safePath(root, name)
		if err != nil {
			return err
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = writeNew(p, r, f.Mode().Perm()|0400)
		r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// VerifyArchive はローカルキャッシュにもネットワーク取得と同じ固定ハッシュ検証を適用する。
func VerifyArchive(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return fmt.Errorf("dependency SHA256 mismatch; refusing to extract")
	}
	return nil
}

func download(ctx context.Context, path string, d Dependency) error {
	client := &http.Client{Timeout: 20 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 10 {
			return fmt.Errorf("unsafe redirect")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("official dependency download returned %s (fixed artifact may have expired)", resp.Status)
	}
	return writeNew(path, io.LimitReader(resp.Body, 2<<30), 0600)
}

func installDependencies(ctx context.Context, root string, d Dependency, archive string) error {
	if _, err := exec.LookPath("tar"); err != nil {
		return fmt.Errorf("tar is required (Windows built-in tar or Linux tar/xz): %w", err)
	}
	work, err := os.MkdirTemp(filepath.Dir(root), ".konomitv-dependencies-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if archive == "" {
		archive = filepath.Join(work, "official.zip")
		fmt.Println("Downloading fixed official dependency archive...")
		if err = download(ctx, archive, d); err != nil {
			return err
		}
	}
	if err = VerifyArchive(archive, d.SHA256); err != nil {
		return err
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	if len(z.File) != 1 || z.File[0].Name != d.Inner {
		return fmt.Errorf("unexpected official artifact contents")
	}
	r, err := z.File[0].Open()
	if err != nil {
		return err
	}
	inner := filepath.Join(work, d.Inner)
	err = writeNew(inner, r, 0600)
	r.Close()
	if err != nil {
		return err
	}
	// 固定ハッシュの公式アーカイブでも、展開先外を指すパスは事前に拒否する。
	out, err := exec.CommandContext(ctx, "tar", "-tf", inner).Output()
	if err != nil {
		return fmt.Errorf("list dependency archive: %w", err)
	}
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		n = strings.TrimSuffix(n, "\r")
		n = strings.TrimSuffix(n, "/")
		if n == "" {
			continue
		}
		if _, err = safePath(filepath.Join(root, "server"), n); err != nil {
			return err
		}
		if !strings.HasPrefix(n, "thirdparty/") && n != "thirdparty" {
			return fmt.Errorf("unexpected dependency root: %s", n)
		}
	}
	// GPUツールはこのCPU版の対象外。VCEEncCのWindows保護機能による拒否を解除・再試行しない。
	args := []string{"-xf", inner, "-C", filepath.Join(root, "server"), "thirdparty/FFmpeg", "thirdparty/tsreadex", "thirdparty/psisiarc", "thirdparty/psisimux"}
	if runtime.GOOS == "linux" {
		args = append(args, "thirdparty/Library")
	}
	cmd := exec.CommandContext(ctx, "tar", args...)
	if out, err = cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract official dependencies: %w: %s", err, out)
	}
	ext := ".elf"
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, n := range []string{"FFmpeg/ffmpeg", "FFmpeg/ffprobe", "tsreadex/tsreadex", "psisiarc/psisiarc"} {
		p := filepath.Join(root, "server", "thirdparty", filepath.FromSlash(n)+ext)
		if info, e := os.Stat(p); e != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("required dependency missing: %s", n)
		}
	}
	// インストール完了と表示する前にCPU用エンコーダーの実行時依存も確認する。チューナーは開かない。
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	check := exec.CommandContext(checkCtx, filepath.Join(root, "server", "thirdparty", "FFmpeg", "ffmpeg"+ext), "-version")
	check.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(root, "server", "thirdparty", "FFmpeg")+":"+filepath.Join(root, "server", "thirdparty", "Library"))
	if output, e := check.CombinedOutput(); e != nil {
		return fmt.Errorf("FFmpeg dependency check failed: %w: %s", e, output)
	}
	return nil
}

// Install は存在しないディレクトリへステージング後に一括配置する。失敗時は専用一時フォルダーだけを除去する。
func Install(ctx context.Context, z *zip.Reader, o Options) error {
	root, err := filepath.Abs(o.Directory)
	if err != nil {
		return err
	}
	if o.Directory == "" {
		return fmt.Errorf("installation directory is required")
	}
	if _, err = os.Lstat(root); !os.IsNotExist(err) {
		return fmt.Errorf("destination already exists; choose a new directory (existing data is never overwritten)")
	}
	if o.Backend != "EDCB" && o.Backend != "Mirakurun" && o.Backend != "IPTV" {
		return fmt.Errorf("backend must be EDCB, Mirakurun or IPTV")
	}
	if !o.IPTV && o.Backend == "IPTV" {
		return fmt.Errorf("IPTV backend requires IPTV enabled")
	}
	if err = os.MkdirAll(filepath.Dir(root), 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".konomitv-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = ExtractPayload(z, stage, o.IPTV); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(stage, "server", "data"), 0700); err != nil {
		return err
	}
	if err = CreateDatabase(filepath.Join(stage, "server", "data", "database.sqlite")); err != nil {
		return err
	}
	cfg := config.Default()
	cfg.IPTV.Enabled = o.IPTV
	cfg.General.Backend = o.Backend
	cfg.Video.RecordedFolders = o.Recordings
	if o.BackendURL != "" {
		u, e := url.Parse(o.BackendURL)
		if e != nil || u.Host == "" {
			return fmt.Errorf("invalid backend URL")
		}
		if o.Backend == "EDCB" {
			if u.Scheme != "tcp" {
				return fmt.Errorf("EDCB URL must use tcp")
			}
			cfg.General.EDCBURL = o.BackendURL
		} else if o.Backend == "Mirakurun" {
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("Mirakurun URL must use http(s)")
			}
			cfg.General.MirakurunURL = o.BackendURL
		}
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(stage, "config.yaml"), b, 0600); err != nil {
		return err
	}
	b, _ = json.Marshal(map[string]bool{"iptv": o.IPTV})
	if err = os.WriteFile(filepath.Join(stage, "installer-features.json"), b, 0600); err != nil {
		return err
	}
	d, err := OfficialDependency()
	if err != nil {
		return err
	}
	if err = installDependencies(ctx, stage, d, o.DependencyArchive); err != nil {
		return err
	}
	// ユーザーが明示して起動するまで、サービス登録・FW変更・チューナー接続は行わない。
	launcher := "#!/usr/bin/env bash\nset -euo pipefail\ncd -- \"$(dirname -- \"$0\")\"\nexport LD_LIBRARY_PATH=\"$PWD/server/thirdparty/FFmpeg:$PWD/server/thirdparty/Library${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\nexec ./bin/konomitv-go -no-proxy -server-dir ./server -listen 127.0.0.1:7002 \"$@\"\n"
	name := "Run-Go.sh"
	if runtime.GOOS == "windows" {
		name = "Run-Go.cmd"
		launcher = "@echo off\r\ncd /d \"%~dp0\"\r\nbin\\konomitv-go.exe -no-proxy -server-dir server -listen 127.0.0.1:7002 %*\r\n"
	}
	if err = os.WriteFile(filepath.Join(stage, name), []byte(launcher), 0755); err != nil {
		return err
	}
	m := Manifest{Version: Version, IPTV: o.IPTV, Files: map[string]string{}}
	err = filepath.WalkDir(stage, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(stage, p)
		rel = filepath.ToSlash(rel)
		if rel == "config.yaml" || rel == "installer-features.json" || strings.HasPrefix(rel, "server/data/") || strings.HasPrefix(rel, "server/logs/") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(p)
			if e != nil {
				return e
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("absolute dependency link: %s", rel)
			}
			resolved, e := filepath.EvalSymlinks(p)
			if e != nil {
				return e
			}
			inside, e := filepath.Rel(stage, resolved)
			if e != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("dependency link escapes installation: %s", rel)
			}
			m.Files[rel] = "symlink:" + target
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported dependency link: %s", rel)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		m.Files[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		return err
	}
	b, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(stage, "install-manifest.json"), b, 0600); err != nil {
		return err
	}
	if _, e := os.Lstat(root); !os.IsNotExist(e) {
		return fmt.Errorf("destination appeared during installation; refusing to replace it")
	}
	if err = os.Rename(stage, root); err != nil {
		return fmt.Errorf("publish completed installation: %w", err)
	}
	fmt.Printf("Installed %s to %s\nIPTV enabled: %v (shared server code remains bundled).\nStart %s, then open http://localhost:7002/\n", Version, root, o.IPTV, name)
	return nil
}

// Uninstall は記録された未変更のアプリファイルだけを削除する。設定、DB、録画、追加ファイルは残す。
func Uninstall(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(root, "install-manifest.json"))
	if err != nil {
		return err
	}
	var m Manifest
	if err = json.Unmarshal(b, &m); err != nil || m.Version == "" || len(m.Files) == 0 {
		return fmt.Errorf("invalid installation manifest")
	}
	for name, expected := range m.Files {
		if name == "config.yaml" || name == "installer-features.json" || strings.HasPrefix(name, "server/data/") || strings.HasPrefix(name, "server/logs/") {
			return fmt.Errorf("manifest refers to protected user data")
		}
		p, err := safePath(root, name)
		if err != nil {
			return err
		}
		for parent := filepath.Dir(p); parent != root; parent = filepath.Dir(parent) {
			info, e := os.Lstat(parent)
			if e == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink directory")
			}
		}
		info, e := os.Lstat(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if strings.HasPrefix(expected, "symlink:") {
			target, e := os.Readlink(p)
			if e != nil || "symlink:"+target != expected {
				fmt.Printf("Keeping modified link: %s\n", name)
				continue
			}
			if err = os.Remove(p); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular installed file: %s", name)
		}
		if VerifyArchive(p, expected) != nil {
			fmt.Printf("Keeping modified file: %s\n", name)
			continue
		}
		if err = os.Remove(p); err != nil {
			return err
		}
	}
	fmt.Println("Application files removed. Configuration, database, recordings and unlisted/modified files were retained.")
	return nil
}
