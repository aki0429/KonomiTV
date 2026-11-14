//go:build !linux && !windows

package service

import (
	"context"
	"fmt"
)

func unsupported() error                                                    { return fmt.Errorf("native service registration supports Windows and Linux") }
func platformOwned(context.Context, *Definition) error                      { return unsupported() }
func platformInstall(context.Context, *Definition, string, string) error    { return unsupported() }
func platformControl(context.Context, *Definition, string) error            { return unsupported() }
func platformDelete(context.Context, *Definition) error                     { return unsupported() }
func platformPersist(context.Context) error                                 { return unsupported() }
func platformChangeManager(context.Context, *Definition, *Definition) error { return unsupported() }
func platformStatus(context.Context, *Definition) (string, error)           { return "", unsupported() }
func acquireLock(string) (func(), error)                                    { return nil, unsupported() }

// RunService はこのOSのサービス登録に対応していないことを返す。
func RunService(context.Context, *Definition) error { return unsupported() }
