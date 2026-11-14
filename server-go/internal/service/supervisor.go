package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type child struct {
	command *exec.Cmd
	done    chan error
	close   func()
	log     *os.File
}

var localHTTPClient = &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 15 * time.Second}}

func startChild(command Command, logPath string) (*child, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = command.Dir, log, log
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	close, err := ownChild(cmd)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		log.Close()
		return nil, err
	}
	result := &child{command: cmd, done: make(chan error, 1), close: close, log: log}
	go func() { result.done <- cmd.Wait() }()
	return result, nil
}

func (c *child) stop() {
	if c == nil {
		return
	}
	// 所有するプロセスとその子だけを回収する。名前による一括終了はしない。
	signalChild(c.command)
	select {
	case <-c.done:
	case <-time.After(8 * time.Second):
	}
	c.close()
	c.log.Close()
}

// Healthy は既知の内部URLへ接続し、Go APIのバージョン応答を確認する。
func Healthy(ctx context.Context, d *Definition) error {
	launch, err := d.Launch(ctx)
	if err != nil {
		return err
	}
	if err := healthURL(ctx, launch.HealthURL); err != nil {
		return err
	}
	if launch.HTTPS != nil {
		_, port, _ := net.SplitHostPort(launch.HTTPS.Args[1])
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			return fmt.Errorf("HTTPS listener is not ready: %w", err)
		}
		connection.Close()
	}
	return nil
}

func healthURL(ctx context.Context, base string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/version", nil)
	if err != nil {
		return err
	}
	response, err := localHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var version struct {
		Version string `json:"version"`
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("native service health: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&version); err != nil || version.Version == "" {
		return fmt.Errorf("native service returned an invalid version response")
	}
	return nil
}

func awaitBackend(ctx context.Context, launch *Launch, backend *child) error {
	deadline, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for deadline.Err() == nil {
		select {
		case err := <-backend.done:
			backend.done <- err
			return fmt.Errorf("backend stopped before startup completed: %v", err)
		default:
		}
		probe, cancel := context.WithTimeout(deadline, time.Second)
		err := healthURL(probe, launch.HealthURL)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	return deadline.Err()
}

func shutdownBackend(launch *Launch) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, launch.HealthURL+"/api/maintenance/shutdown", nil)
	if err != nil {
		return
	}
	// 本家と同じ内部Hostによるサービス停止を使い、JWTを保存・生成しない。
	request.Host = launch.InternalHost
	response, err := localHTTPClient.Do(request)
	if err == nil {
		response.Body.Close()
	}
}

// Run はGo本体と任意のHTTPS配信を管理する。再起動要求のときは設定を読み直す。
// PM2環境のshutdownは本家同様、外側のPM2がプロセスを再起動する。
func Run(ctx context.Context, d *Definition) error {
	unlock, err := acquireLock(filepath.Join(d.Root, "server", "data", ".native-service-run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	for {
		launch, err := d.Launch(ctx)
		if err != nil {
			return err
		}
		lock := filepath.Join(d.Root, "server", "data", "restart_required.lock")
		// 前回の強制終了で残った要求は、今回の新規起動で消費済みとして扱う。
		if err := os.Remove(lock); err != nil && !os.IsNotExist(err) {
			return err
		}
		backend, err := startChild(launch.Backend, filepath.Join(d.Root, "server", "logs", "Native-Service-Backend.log"))
		if err != nil {
			return err
		}
		// Go本体が行う公開ポートの空き検査を先に終える。Akebiを先に起動すると、
		// 自分のHTTPSポートを他アプリの使用中ポートと誤認して起動できなくなる。
		if err := awaitBackend(ctx, launch, backend); err != nil {
			if ctx.Err() != nil {
				// 健康確認と停止が同時になった場合も、起動済みHTTPへ正常終了を依頼する。
				shutdownBackend(launch)
			}
			backend.stop()
			if ctx.Err() != nil {
				return nil
			}
			// 最初のHTTP受付直後に再起動APIが呼ばれると、健康確認が完了する前に
			// 子が正常終了することがある。この場合も通常の再起動要求として消費する。
			if _, statErr := os.Stat(lock); statErr == nil {
				continue
			}
			return err
		}
		var https *child
		if launch.HTTPS != nil {
			https, err = startChild(*launch.HTTPS, filepath.Join(d.Root, "server", "logs", "Akebi-HTTPS-Server.log"))
			if err != nil {
				backend.stop()
				return err
			}
		}
		var httpsDone <-chan error
		if https != nil {
			httpsDone = https.done
		}
		var processError error
		select {
		case <-ctx.Done():
			shutdownBackend(launch)
		case processError = <-backend.done:
			// Wait結果をstop()でも受け取れるように戻す。
			backend.done <- processError
		case processError = <-httpsDone:
			https.done <- processError
			if processError == nil {
				processError = fmt.Errorf("HTTPS process stopped before the backend")
			}
			shutdownBackend(launch)
		}
		backend.stop()
		if https != nil {
			// HTTPSには終了APIがないため、所有するプロセスを直接回収する。
			https.close()
			select {
			case <-https.done:
			case <-time.After(2 * time.Second):
			}
			https.log.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		if _, err := os.Stat(lock); err == nil {
			continue
		}
		return processError
	}
}
