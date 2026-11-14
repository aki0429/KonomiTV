// konomitv-service は準備済みGo配布物のサービス登録・更新・削除を行う。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/service"
)

// buildRevision は配布用のサービス管理コマンドとGitを照合するために埋め込む。
var buildRevision = "development"

func main() {
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: konomitv-service install|update|uninstall|start|stop|restart|status|run -root <directory>")
	}
	action := os.Args[1]
	if action == "version" {
		fmt.Printf("KonomiTV native service manager (revision %s)\n", buildRevision)
		return nil
	}
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	root := flags.String("root", "", "prepared KonomiTV Go installation directory")
	name := flags.String("name", "", "unique service name (default: root directory name)")
	backend := flags.String("backend", "", "Go backend filename in the installation root (default: kgo or kgo.exe)")
	listen := flags.String("listen", "", "override internal listen address (default: 127.0.0.77:<server.port+10>)")
	externalHTTPS := flags.Bool("external-https", false, "leave an existing HTTPS proxy under its current management")
	replacement := flags.String("replacement", "", "new Go backend executable for update")
	account := flags.String("account", "", "Windows service account (required for a new Windows registration)")
	passwordEnv := flags.String("password-env", "", "environment variable holding the Windows service password")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *root == "" || len(flags.Args()) > 0 {
		return fmt.Errorf("an explicit installation root is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if action == "run" {
		d, err := service.Load(*root)
		if err != nil {
			return err
		}
		return service.RunService(ctx, d)
	}
	// 一般の登録操作は無期限に待たず、稼働プロセスの子だけが常駐する。
	ctx, timeoutCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer timeoutCancel()
	credentials := service.Credentials{Account: *account}
	if *passwordEnv != "" {
		var exists bool
		credentials.Password, exists = os.LookupEnv(*passwordEnv)
		if !exists {
			return fmt.Errorf("Windows service password environment variable is not set")
		}
	}
	switch action {
	case "install":
		d, err := service.NewDefinition(ctx, *root, *name, *backend, *listen, *externalHTTPS)
		if err != nil {
			return err
		}
		if err := service.Install(ctx, d, credentials); err != nil {
			return err
		}
		fmt.Printf("Native service installed: %s\n", d.Name)
	case "update":
		if *replacement == "" {
			return fmt.Errorf("update requires -replacement")
		}
		result, err := service.Update(ctx, *root, *replacement, credentials)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "uninstall":
		if err := service.Uninstall(ctx, *root); err != nil {
			return err
		}
		fmt.Println("Native service registration removed. Application data is retained.")
	case "start", "stop", "restart":
		return service.Control(ctx, *root, action)
	case "status":
		state, err := service.Status(ctx, *root)
		if err != nil {
			return err
		}
		fmt.Println(state)
	default:
		return fmt.Errorf("unknown native service action: %s", action)
	}
	return nil
}
