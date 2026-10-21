//go:build !windows

package config

import "os"

// replaceConfigFile は同一ファイルシステム上の rename で設定を原子的に置換する。
func replaceConfigFile(source, target string) error {
	return os.Rename(source, target)
}
