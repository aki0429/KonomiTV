package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
)

type pm2Process struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Env  struct {
		ID     *int            `json:"pm_id"`
		Script string          `json:"pm_exec_path"`
		Cwd    string          `json:"pm_cwd"`
		Args   json.RawMessage `json:"args"`
		Status string          `json:"status"`
	} `json:"pm2_env"`
}

func pm2(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "pm2", args...)
	output, err := command.Output()
	if err != nil {
		// jlistや環境変数の内容をエラーへ埋め込まない。
		return nil, fmt.Errorf("PM2 %s failed: %w", args[0], err)
	}
	return output, nil
}

func pm2Find(ctx context.Context, d *Definition) (*pm2Process, error) {
	data, err := pm2(ctx, "jlist")
	if err != nil {
		return nil, err
	}
	var rows []pm2Process
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("invalid PM2 process list: %w", err)
	}
	var selected *pm2Process
	for _, row := range rows {
		if row.Name == d.Name {
			if selected != nil {
				return nil, fmt.Errorf("multiple PM2 services have the requested name")
			}
			selected = &row
		}
	}
	return selected, nil
}

func pm2Owned(ctx context.Context, d *Definition) (*pm2Process, error) {
	row, err := pm2Find(ctx, d)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotInstalled
	}
	var args []string
	argsError := json.Unmarshal(row.Env.Args, &args)
	if filepath.Clean(row.Env.Script) != filepath.Clean(d.ManagerBinary) || filepath.Clean(row.Env.Cwd) != filepath.Clean(d.Root) || argsError != nil || !reflect.DeepEqual(args, []string{"run", "-root", d.Root}) {
		return nil, fmt.Errorf("PM2 service is not owned by this installation")
	}
	if row.Env.ID == nil || *row.Env.ID < 0 {
		return nil, fmt.Errorf("PM2 service ID is missing")
	}
	return row, nil
}

func platformOwned(ctx context.Context, d *Definition) error {
	_, err := pm2Owned(ctx, d)
	return err
}

func platformInstall(ctx context.Context, d *Definition, account, password string) error {
	if account != "" || password != "" {
		return fmt.Errorf("Linux services use the current PM2 user")
	}
	row, err := pm2Find(ctx, d)
	if err != nil {
		return err
	}
	if row != nil {
		return fmt.Errorf("PM2 service name is already in use")
	}
	// PM2の現在ユーザーのプロファイルだけに登録する。sudoやstartup設定の変更は行わない。
	data, err := json.MarshalIndent(map[string]any{"apps": []any{map[string]any{
		"name": d.Name, "script": d.ManagerBinary, "cwd": d.Root, "args": []string{"run", "-root", d.Root},
		"interpreter": "none", "autorestart": true, "kill_timeout": 25000,
		"out_file":   filepath.Join(d.Root, "server", "logs", "Native-Service.log"),
		"error_file": filepath.Join(d.Root, "server", "logs", "Native-Service-Error.log"),
	}}}, "", "  ")
	if err != nil {
		return err
	}
	configuration := filepath.Join(d.Root, "server", "data", "native-pm2.config.json")
	if err := atomicWrite(configuration, data, 0600); err != nil {
		return err
	}
	_, err = pm2(ctx, "start", configuration, "--only", d.Name)
	return err
}

func platformControl(ctx context.Context, d *Definition, action string) error {
	row, err := pm2Owned(ctx, d)
	if err != nil {
		return err
	}
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid PM2 service action")
	}
	// 数字だけのサービス名もIDと混同せず、照合済みの登録IDだけを操作する。
	_, err = pm2(ctx, action, strconv.Itoa(*row.Env.ID))
	return err
}

func platformDelete(ctx context.Context, d *Definition) error {
	row, err := pm2Owned(ctx, d)
	if err != nil {
		return err
	}
	_, err = pm2(ctx, "delete", strconv.Itoa(*row.Env.ID))
	return err
}

func platformPersist(ctx context.Context) error {
	_, err := pm2(ctx, "save")
	return err
}

func platformChangeManager(ctx context.Context, old, next *Definition) error {
	if err := platformDelete(ctx, old); err != nil {
		return err
	}
	return platformInstall(ctx, next, "", "")
}

func platformStatus(ctx context.Context, d *Definition) (string, error) {
	if err := platformOwned(ctx, d); err != nil {
		return "", err
	}
	row, err := pm2Find(ctx, d)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotInstalled
	}
	return row.Env.Status, nil
}

// RunService はPM2またはコンソールからGo本体とHTTPSを起動する。
func RunService(ctx context.Context, d *Definition) error { return Run(ctx, d) }
