package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func withWindowsService(ctx context.Context, d *Definition, action func(*mgr.Service) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(d.Name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return ErrNotInstalled
	}
	if err != nil {
		return err
	}
	defer service.Close()
	configuration, err := service.Config()
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(configuration.BinaryPathName)
	if err != nil || !reflect.DeepEqual(args, []string{d.ManagerBinary, "run", "-root", d.Root}) {
		return fmt.Errorf("Windows service is not owned by this installation")
	}
	return action(service)
}

func platformOwned(ctx context.Context, d *Definition) error {
	return withWindowsService(ctx, d, func(*mgr.Service) error { return nil })
}

func platformInstall(ctx context.Context, d *Definition, account, password string) error {
	if account == "" {
		return fmt.Errorf("a new Windows service requires an explicit -account")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	if existing, err := manager.OpenService(d.Name); err == nil {
		existing.Close()
		return fmt.Errorf("Windows service name is already in use")
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	service, err := manager.CreateService(d.Name, d.ManagerBinary, mgr.Config{
		DisplayName: "KonomiTV Go (" + d.Name + ")", Description: "KonomiTV native Go server and HTTPS proxy",
		StartType: mgr.StartAutomatic, DelayedAutoStart: true, ServiceStartName: account, Password: password,
	}, "run", "-root", d.Root)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 86400); err != nil {
		_ = service.Delete()
		return err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		_ = service.Delete()
		return err
	}
	if err := service.Start(); err != nil {
		_ = service.Delete()
		return err
	}
	return nil
}

func controlWindows(ctx context.Context, service *mgr.Service, action string) error {
	state, err := service.Query()
	if err != nil {
		return err
	}
	if action == "restart" {
		if err := controlWindows(ctx, service, "stop"); err != nil {
			return err
		}
		return controlWindows(ctx, service, "start")
	}
	want := svc.Stopped
	if action == "start" {
		want = svc.Running
		if state.State != want {
			if err := service.Start(); err != nil {
				return err
			}
		}
	} else if action == "stop" {
		if state.State != want {
			if _, err := service.Control(svc.Stop); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("invalid Windows service action")
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		state, err := service.Query()
		if err != nil {
			return err
		}
		if state.State == want {
			return nil
		}
		select {
		case <-deadline.Done():
			return deadline.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func platformControl(ctx context.Context, d *Definition, action string) error {
	return withWindowsService(ctx, d, func(service *mgr.Service) error { return controlWindows(ctx, service, action) })
}

func platformDelete(ctx context.Context, d *Definition) error {
	return withWindowsService(ctx, d, func(service *mgr.Service) error {
		if err := controlWindows(ctx, service, "stop"); err != nil {
			return err
		}
		return service.Delete()
	})
}

func platformChangeManager(ctx context.Context, old, next *Definition) error {
	return withWindowsService(ctx, old, func(service *mgr.Service) error {
		// 実行パスだけを更新し、SCMに保存された実行アカウント・パスワード・起動設定を維持する。
		line := syscall.EscapeArg(next.ManagerBinary) + " run -root " + syscall.EscapeArg(next.Root)
		binaryPath, err := windows.UTF16PtrFromString(line)
		if err != nil {
			return err
		}
		return windows.ChangeServiceConfig(service.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE,
			binaryPath, nil, nil, nil, nil, nil, nil)
	})
}

func platformPersist(context.Context) error { return nil }

func platformStatus(ctx context.Context, d *Definition) (string, error) {
	state := ""
	err := withWindowsService(ctx, d, func(service *mgr.Service) error {
		current, err := service.Query()
		if err != nil {
			return err
		}
		switch current.State {
		case svc.Running:
			state = "running"
		case svc.Stopped:
			state = "stopped"
		default:
			state = "pending"
		}
		return nil
	})
	return state, err
}

type windowsHandler struct{ definition *Definition }

func (h *windowsHandler) Execute(args []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 30000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h.definition) }()
	current := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- current
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- current
			case svc.Stop, svc.Shutdown:
				current = svc.Status{State: svc.StopPending, WaitHint: 30000}
				statuses <- current
				cancel()
			}
		}
	}
}

// RunService はSCMからの起動ではWindowsサービスとして、それ以外はコンソールで実行する。
func RunService(ctx context.Context, d *Definition) error {
	managed, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if managed {
		return svc.Run(d.Name, &windowsHandler{definition: d})
	}
	return Run(ctx, d)
}
