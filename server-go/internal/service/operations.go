package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"modernc.org/sqlite"
)

// ErrNotInstalled はこのルートのサービスが登録されていない場合のエラー。
var ErrNotInstalled = errors.New("native service is not installed")

// Credentials はWindowsサービス登録に渡す情報。状態ファイルへは保存しない。
type Credentials struct{ Account, Password string }

// UpdateResult は保存済みバックアップと新しい実行ファイルの識別情報。
type UpdateResult struct {
	BackupDirectory string `json:"backup_directory"`
	Version         string `json:"version"`
	SHA256          string `json:"sha256"`
}

func operationLock(root string) (func(), error) {
	return acquireLock(filepath.Join(root, "server", "data", ".native-service-operation.lock"))
}

// Install は準備済み配布物を現在ユーザーのPM2またはWindowsサービスへ登録する。
// 同名の別サービスは変更しない。同じ所有情報の再実行は登録を重複させない。
func Install(ctx context.Context, d *Definition, credentials Credentials) error {
	if err := d.Validate(); err != nil {
		return err
	}
	unlock, err := operationLock(d.Root)
	if err != nil {
		return err
	}
	defer unlock()
	if existing, err := Load(d.Root); err == nil {
		if existing.Name != d.Name || existing.BackendFile != d.BackendFile || existing.ListenOverride != d.ListenOverride || existing.ExternalHTTPS != d.ExternalHTTPS {
			return fmt.Errorf("installation already has a different native service definition")
		}
		if err := platformOwned(ctx, existing); err != nil {
			if !errors.Is(err, ErrNotInstalled) {
				return err
			}
			if err := platformInstall(ctx, existing, credentials.Account, credentials.Password); err != nil {
				return err
			}
		} else if state, err := platformStatus(ctx, existing); err != nil {
			return err
		} else if state != "online" && state != "running" {
			if err := platformControl(ctx, existing, "start"); err != nil {
				return err
			}
		}
		if err := waitHealthy(ctx, existing); err != nil {
			return err
		}
		return platformPersist(ctx)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := version(ctx, filepath.Join(d.Root, d.BackendFile)); err != nil {
		return err
	}
	launch, err := d.Launch(ctx)
	if err != nil {
		return err
	}
	// 登録後に他のGoサーバーへ健康確認を誤送信しないよう、起動前に使用ポートを確認する。
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(launch.HealthURL, "http://"))
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(launch.HealthURL, "http://"))
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("internal port is already in use: %w", err)
	}
	listener.Close()
	d.ManagerBinary, err = prepareManager(d.Root)
	if err != nil {
		return err
	}
	if ownedErr := platformOwned(ctx, d); !errors.Is(ownedErr, ErrNotInstalled) {
		if ownedErr != nil {
			return ownedErr
		}
		return fmt.Errorf("service name is already registered")
	}
	if err := save(d); err != nil {
		return err
	}
	if err := platformInstall(ctx, d, credentials.Account, credentials.Password); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ownedErr := platformOwned(cleanupCtx, d)
		if ownedErr == nil {
			if cleanupErr := platformDelete(cleanupCtx, d); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
		} else if !errors.Is(ownedErr, ErrNotInstalled) {
			return errors.Join(err, ownedErr)
		}
		return errors.Join(err, os.Remove(d.StatePath()))
	}
	if err := waitHealthy(ctx, d); err != nil {
		// 健康確認に失敗した登録だけを回収する。外部HTTPSや別のPM2サービスは対象外。
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := platformDelete(cleanupCtx, d)
		if cleanupErr == nil {
			_ = os.Remove(d.StatePath())
		}
		return errors.Join(err, cleanupErr)
	}
	return platformPersist(ctx)
}

func waitHealthy(ctx context.Context, d *Definition) error {
	deadline, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var last error
	for deadline.Err() == nil {
		state, err := platformStatus(deadline, d)
		if err == nil && (state == "online" || state == "running") {
			probe, cancel := context.WithTimeout(deadline, 2*time.Second)
			last = Healthy(probe, d)
			cancel()
			if last == nil {
				return nil
			}
		} else {
			last = err
		}
		select {
		case <-deadline.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("native service did not become healthy: %w", errors.Join(deadline.Err(), last))
}

// Control は所有を照合してから、このインストールのサービスだけを操作する。
func Control(ctx context.Context, root, action string) error {
	d, err := Load(root)
	if err != nil {
		return err
	}
	unlock, err := operationLock(d.Root)
	if err != nil {
		return err
	}
	defer unlock()
	d, err = Load(root)
	if err != nil {
		return err
	}
	if err := platformControl(ctx, d, action); err != nil {
		return err
	}
	if action == "start" || action == "restart" {
		if err := waitHealthy(ctx, d); err != nil {
			return err
		}
	}
	return platformPersist(ctx)
}

// Status は所有情報と実際の登録先を照合した状態を返す。
func Status(ctx context.Context, root string) (string, error) {
	d, err := Load(root)
	if err != nil {
		return "", err
	}
	return platformStatus(ctx, d)
}

// Uninstall は所有するサービス登録を削除する。アプリの設定・DB・録画は保持する。
func Uninstall(ctx context.Context, root string) error {
	d, err := Load(root)
	if err != nil {
		return err
	}
	unlock, err := operationLock(d.Root)
	if err != nil {
		return err
	}
	defer unlock()
	d, err = Load(root)
	if err != nil {
		return err
	}
	if err := platformDelete(ctx, d); err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}
	if err := platformPersist(ctx); err != nil {
		return err
	}
	return os.Remove(d.StatePath())
}

// Update はDB・設定・認証鍵をバックアップしてGo本体を入れ替える。起動失敗時は旧実行ファイルへ戻す。
func Update(ctx context.Context, root, replacement string, credentials Credentials) (*UpdateResult, error) {
	d, err := Load(root)
	if err != nil {
		return nil, err
	}
	unlock, err := operationLock(d.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	d, err = Load(root)
	if err != nil {
		return nil, err
	}
	if err := platformOwned(ctx, d); err != nil {
		return nil, err
	}
	newVersion, err := version(ctx, replacement)
	if err != nil {
		return nil, err
	}
	newHash, err := hashFile(replacement)
	if err != nil {
		return nil, err
	}
	newDefinition := *d
	newDefinition.ManagerBinary, err = prepareManager(d.Root)
	if err != nil {
		return nil, err
	}
	backup, err := backupInstallation(ctx, d)
	if err != nil {
		return nil, err
	}
	if err := platformControl(ctx, d, "stop"); err != nil {
		return nil, err
	}
	managerChanged := newDefinition.ManagerBinary != d.ManagerBinary
	rollback := func(cause error) (*UpdateResult, error) {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var restoreErrors []error
		// 起動に失敗した版がまだ動いている場合も、実行ファイルを戻す前に停止する。
		// 特にWindowsでは、実行中のexeを置き換えられない。
		if platformOwned(restoreCtx, &newDefinition) == nil {
			restoreErrors = append(restoreErrors, platformControl(restoreCtx, &newDefinition, "stop"))
		} else if platformOwned(restoreCtx, d) == nil {
			restoreErrors = append(restoreErrors, platformControl(restoreCtx, d, "stop"))
		}
		restoreErrors = append(restoreErrors, copyFile(filepath.Join(backup, d.BackendFile), filepath.Join(d.Root, d.BackendFile), 0755), save(d))
		if managerChanged && platformOwned(restoreCtx, &newDefinition) == nil {
			restoreErrors = append(restoreErrors, platformChangeManager(restoreCtx, &newDefinition, d))
		}
		if ownedErr := platformOwned(restoreCtx, d); errors.Is(ownedErr, ErrNotInstalled) {
			restoreErrors = append(restoreErrors, platformInstall(restoreCtx, d, credentials.Account, credentials.Password))
		} else if ownedErr == nil {
			restoreErrors = append(restoreErrors, platformControl(restoreCtx, d, "start"))
		} else {
			restoreErrors = append(restoreErrors, ownedErr)
		}
		restoreErrors = append(restoreErrors, waitHealthy(restoreCtx, d), platformPersist(restoreCtx))
		return nil, errors.Join(append([]error{cause}, restoreErrors...)...)
	}
	if err := copyFile(replacement, filepath.Join(d.Root, d.BackendFile), 0755); err != nil {
		return rollback(err)
	}
	if installedHash, err := hashFile(filepath.Join(d.Root, d.BackendFile)); err != nil || installedHash != newHash {
		return rollback(fmt.Errorf("installed backend SHA-256 mismatch"))
	}
	if err := save(&newDefinition); err != nil {
		return rollback(err)
	}
	if managerChanged {
		if err = platformChangeManager(ctx, d, &newDefinition); err != nil {
			return rollback(err)
		}
	}
	err = platformControl(ctx, &newDefinition, "start")
	if err != nil {
		return rollback(err)
	}
	if err := waitHealthy(ctx, &newDefinition); err != nil {
		return rollback(err)
	}
	if err := platformPersist(ctx); err != nil {
		return nil, err
	}
	return &UpdateResult{BackupDirectory: backup, Version: newVersion, SHA256: newHash}, nil
}

type versionOutput struct{ buffer bytes.Buffer }

func (b *versionOutput) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > 4096 {
		return 0, fmt.Errorf("backend version output exceeds limit")
	}
	return b.buffer.Write(data)
}

func version(ctx context.Context, binary string) (string, error) {
	limited, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(limited, binary, "-version")
	configureChild(command)
	var output versionOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("backend version check failed: %w", err)
	}
	text := strings.TrimSpace(output.buffer.String())
	if len(text) > 4096 || !strings.HasPrefix(text, "KonomiTV version ") || !strings.Contains(text, "(server-go, revision ") {
		return "", fmt.Errorf("replacement is not a KonomiTV Go backend")
	}
	return text, nil
}

func prepareManager(root string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	hash, err := hashFile(self)
	if err != nil {
		return "", err
	}
	name := "konomitv-service"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(root, "server", "go-native", "Service", hash, name)
	if existingHash, err := hashFile(target); err == nil {
		if existingHash != hash {
			return "", fmt.Errorf("installed service manager SHA-256 mismatch")
		}
		return target, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return target, copyFile(self, target, 0755)
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(target), ".native-copy-")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	_, err = io.Copy(output, input)
	if err == nil {
		err = output.Chmod(mode)
	}
	closeErr := output.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(output.Name(), target)
}

func backupInstallation(ctx context.Context, d *Definition) (string, error) {
	directory := filepath.Join(d.Root, "server", "backups")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	backup, err := os.MkdirTemp(directory, "native-update-")
	if err != nil {
		return "", err
	}
	for _, relative := range []string{d.BackendFile, "config.yaml", "server/data/native-service.json", "server/data/jwt_secret.dat"} {
		input := filepath.Join(d.Root, filepath.FromSlash(relative))
		if _, err := os.Stat(input); os.IsNotExist(err) && relative == "server/data/jwt_secret.dat" {
			continue
		}
		if err := copyFile(input, filepath.Join(backup, filepath.FromSlash(relative)), 0600); err != nil {
			return "", err
		}
	}
	databaseFile := filepath.Join(d.Root, "server", "data", "database.sqlite")
	if _, err := os.Stat(databaseFile); err == nil {
		if err := backupDatabase(ctx, databaseFile, filepath.Join(backup, "database.sqlite")); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return backup, nil
}

func backupDatabase(ctx context.Context, source, target string) error {
	db, err := database.OpenReadOnly(source)
	if err != nil {
		return err
	}
	defer db.Close()
	connection, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	file.Close()
	// WAL内の未チェックポイント分も含むオンラインバックアップを使う。
	return connection.Raw(func(driver any) error {
		backupper, ok := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver does not support online backup")
		}
		backup, err := backupper.NewBackup("file:" + filepath.ToSlash(target))
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil {
				return err
			}
			if !more {
				finished = true
				return backup.Finish()
			}
		}
	})
}
