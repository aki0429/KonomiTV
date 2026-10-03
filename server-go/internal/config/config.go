// Package config は KonomiTV の config.yaml (server/app/config.py 互換) を読み込む。
//
// 注意: Python 版の LoadConfig() はバックエンド (EDCB/Mirakurun) への接続確認まで行うが、
// Go 版ではサーバー単体で起動できることを優先し、接続確認は行わない。
// 接続確認が必要になった時点でオプションとして実装する。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config は config.yaml 全体のうち、Go 版サーバーが利用する設定値のみを保持する。
// config.yaml に存在しない設定値は Python 版と同じデフォルト値で補完される。
type Config struct {
	General GeneralConfig `yaml:"general"`
	Server  ServerConfig  `yaml:"server"`
	IPTV    IPTVConfig    `yaml:"iptv"`
}

// GeneralConfig は general セクションの設定。
type GeneralConfig struct {
	// Backend は利用するバックエンド (EDCB / Mirakurun / IPTV) 。
	Backend string `yaml:"backend"`
	// AlwaysReceiveTVFromMirakurun は常に Mirakurun から放送波を受信するか。
	AlwaysReceiveTVFromMirakurun bool `yaml:"always_receive_tv_from_mirakurun"`
	// EDCBURL は EDCB の TCP API の URL。
	EDCBURL string `yaml:"edcb_url"`
	// MirakurunURL は Mirakurun / mirakc の HTTP API の URL。
	MirakurunURL string `yaml:"mirakurun_url"`
	// Encoder は利用するエンコーダー (FFmpeg / QSVEncC / NVEncC / VCEEncC / rkmppenc) 。
	Encoder string `yaml:"encoder"`
	// ProgramUpdateInterval は番組情報の更新間隔 (分) 。
	ProgramUpdateInterval float64 `yaml:"program_update_interval"`
	// Debug はデバッグモードが有効か。
	Debug bool `yaml:"debug"`
	// DebugEncoder はエンコーダーのログ出力が有効か。
	DebugEncoder bool `yaml:"debug_encoder"`
}

// ServerConfig は server セクションの設定。
type ServerConfig struct {
	// Port は KonomiTV サーバーのリッスンポート (外側から見えるポート) 。
	Port int `yaml:"port"`
	// CustomHTTPSCertificate はカスタム HTTPS 証明書のパス (未設定なら null) 。
	CustomHTTPSCertificate *string `yaml:"custom_https_certificate"`
	// CustomHTTPSPrivateKey はカスタム HTTPS 秘密鍵のパス (未設定なら null) 。
	CustomHTTPSPrivateKey *string `yaml:"custom_https_private_key"`
}

// IPTVConfig は iptv セクションの設定。
type IPTVConfig struct {
	// Enabled は IPTV 機能が有効か。
	Enabled bool `yaml:"enabled"`
	// Sources は取り込む M3U プレイリストの URL (またはローカルファイルパス) の一覧。
	Sources []string `yaml:"sources"`
	// CacheTTL はプレイリスト取得結果をキャッシュする時間 (秒) 。
	CacheTTL int `yaml:"cache_ttl"`
	// RequestTimeout はプレイリストやストリームの取得時のタイムアウト (秒) 。
	RequestTimeout float64 `yaml:"request_timeout"`
	// UserAgent は IPTV の取得時に送信する User-Agent。
	UserAgent string `yaml:"user_agent"`
}

// バックエンドとして指定できる値 (server/app/config.py と一致させること) 。
var validBackends = []string{"EDCB", "Mirakurun", "IPTV"}

// エンコーダーとして指定できる値 (server/app/config.py と一致させること) 。
var validEncoders = []string{"FFmpeg", "QSVEncC", "NVEncC", "VCEEncC", "rkmppenc"}

// Default は Python 版の Pydantic モデルに定義されたデフォルト値を持つ Config を返す。
func Default() *Config {
	config := &Config{}
	config.General.Backend = "EDCB"
	config.General.EDCBURL = "tcp://127.0.0.1:4510/"
	config.General.MirakurunURL = "http://127.0.0.1:40772/"
	config.General.Encoder = "FFmpeg"
	config.General.ProgramUpdateInterval = 5.0
	config.Server.Port = 7000
	config.IPTV.Enabled = false
	config.IPTV.CacheTTL = 3600
	config.IPTV.RequestTimeout = 20.0
	config.IPTV.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	return config
}

// Load は指定されたパスの config.yaml を読み込み、デフォルト値で補完した Config を返す。
func Load(path string) (*Config, error) {
	config := Default()

	// config.yaml を読み込む
	// ファイルが存在しない場合は Python 版と同様に致命的エラーとする
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config.yaml (%s): %w", path, err)
	}
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config.yaml (%s): %w", path, err)
	}

	// 値の検証
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// Validate は設定値の基本的な検証を行う。
func (c *Config) Validate() error {
	if !contains(validBackends, c.General.Backend) {
		return fmt.Errorf("general.backend must be one of %s, but got %q", strings.Join(validBackends, ", "), c.General.Backend)
	}
	if !contains(validEncoders, c.General.Encoder) {
		return fmt.Errorf("general.encoder must be one of %s, but got %q", strings.Join(validEncoders, ", "), c.General.Encoder)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, but got %d", c.Server.Port)
	}
	if c.General.ProgramUpdateInterval < 0.1 {
		return fmt.Errorf("general.program_update_interval must be 0.1 or more, but got %v", c.General.ProgramUpdateInterval)
	}
	return nil
}

// BackendAPIURL はバックエンド側 (Python サーバー) の内部リッスン URL を返す。
// Python 版は 127.0.0.77:(server.port + 10) でリッスンする (KonomiTV.py を参照) 。
func (c *Config) BackendAPIURL() string {
	return fmt.Sprintf("http://127.0.0.77:%d/", c.Server.Port+10)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
