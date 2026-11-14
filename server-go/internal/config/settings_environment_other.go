//go:build !linux && !windows

package config

import (
	"context"
	"fmt"
	"runtime"
)

func runningAsLocalSystem() (bool, error) { return false, nil }
func settingsIgnoredPathError(error) bool { return false }
func settingsListeningPorts(context.Context) ([]SettingsListener, error) {
	return nil, fmt.Errorf("settings port inspection is unsupported on %s", runtime.GOOS)
}
