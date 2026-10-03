// Package constants は KonomiTV サーバー全体で利用する定数とパス情報を提供する。
// 値は server/app/constants.py と互換になるように維持すること。
package constants

import (
	"path/filepath"
	"runtime"
	"time"
)

// Version は KonomiTV サーバーのバージョン。
// server/pyproject.toml の project.version と一致させること (TestVersionMatchesPyproject で検証している) 。
const Version = "0.14.1"

// JST は Asia/Tokyo の固定タイムゾーン。
// 日本国内向けのアプリケーションのため、夏時間などは考慮する必要がない。
var JST = time.FixedZone("Asia/Tokyo", 9*60*60)

// Paths はサーバーが必要とする主要なファイルパスを保持する。
type Paths struct {
	// ServerDir は Python 版サーバーのディレクトリ (例: <repo>/server) 。
	ServerDir string
	// RepoRootDir はリポジトリのルートディレクトリ (例: <repo>) 。
	RepoRootDir string
	// ConfigYAMLPath は config.yaml のパス (リポジトリルート直下) 。
	ConfigYAMLPath string
	// ClientDistDir はビルド済みクライアント (client/dist) のパス。
	ClientDistDir string
	// DataDir はサーバーデータディレクトリ (SQLite や JWT 秘密鍵など) 。
	DataDir string
	// DatabasePath は SQLite データベースファイルのパス。
	DatabasePath string
	// StaticDir はサーバーが同梱する静的ファイル (放送局ロゴなど) のパス。
	StaticDir string
	// ThumbnailsDir は録画番組のサムネイル画像ディレクトリ。
	ThumbnailsDir string
	// LogsDir はログ出力ディレクトリ。
	LogsDir string
	// ServerLogPath はサーバーログのパス (Python 版 KONOMITV_SERVER_LOG_PATH) 。
	ServerLogPath string
	// AccessLogPath はアクセスログのパス (Python 版 KONOMITV_ACCESS_LOG_PATH) 。
	AccessLogPath string
	// LibraryDir はサードパーティーライブラリ (FFmpeg など) のディレクトリ。
	LibraryDir string
}

// libraryBinaryPaths はサードパーティーライブラリの名前から、LibraryDir からの相対パスへの対応表。
// server/app/constants.py の LIBRARY_PATH と一致させること。
var libraryBinaryPaths = map[string]string{
	"Akebi":    "Akebi/akebi-https-server",
	"FFmpeg":   "FFmpeg/ffmpeg",
	"FFprobe":  "FFmpeg/ffprobe",
	"QSVEncC":  "QSVEncC/QSVEncC",
	"NVEncC":   "NVEncC/NVEncC",
	"VCEEncC":  "VCEEncC/VCEEncC",
	"rkmppenc": "rkmppenc/rkmppenc",
	"tsreadex": "tsreadex/tsreadex",
	"psisiarc": "psisiarc/psisiarc",
	"psisimux": "psisimux/psisimux",
}

// libraryExtension はサードパーティーライブラリの拡張子 (Windows は .exe、それ以外は .elf) 。
func libraryExtension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ".elf"
}

// LibraryPath はサードパーティーライブラリの絶対パスを返す (server/app/constants.py の LIBRARY_PATH 相当) 。
func (p Paths) LibraryPath(name string) string {
	return filepath.Join(p.LibraryDir, filepath.FromSlash(libraryBinaryPaths[name])) + libraryExtension()
}

// NewPaths は ServerDir から各パスを構築する。
func NewPaths(serverDir string) Paths {
	serverDir = filepath.Clean(serverDir)
	repoRoot := filepath.Dir(serverDir)
	return Paths{
		ServerDir:      serverDir,
		RepoRootDir:    repoRoot,
		ConfigYAMLPath: filepath.Join(repoRoot, "config.yaml"),
		ClientDistDir:  filepath.Join(repoRoot, "client", "dist"),
		DataDir:        filepath.Join(serverDir, "data"),
		DatabasePath:   filepath.Join(serverDir, "data", "database.sqlite"),
		StaticDir:      filepath.Join(serverDir, "static"),
		ThumbnailsDir:  filepath.Join(serverDir, "data", "thumbnails"),
		LogsDir:        filepath.Join(serverDir, "logs"),
		ServerLogPath:  filepath.Join(serverDir, "logs", "KonomiTV-Server.log"),
		AccessLogPath:  filepath.Join(serverDir, "logs", "KonomiTV-Access.log"),
		LibraryDir:     filepath.Join(serverDir, "thirdparty"),
	}
}
