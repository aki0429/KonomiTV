package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// テスト専用子プロセス。既存アプリ・チューナー・外部APIを起動しない。
func init() {
	mode := os.Getenv("KONOMITV_SERVICE_TEST_HELPER")
	if mode == "" {
		return
	}
	if os.Args[1] == "-version" {
		fmt.Println("KonomiTV version fixture (server-go, revision fixture)")
		os.Exit(0)
	}
	if os.Args[1] == "owner" {
		d, err := Load(os.Getenv("KONOMITV_SERVICE_TEST_ROOT"))
		if err != nil {
			os.Exit(5)
		}
		if err := Run(context.Background(), d); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	}
	if os.Args[1] == "--listen-address" {
		listener, err := net.Listen("tcp", os.Args[2])
		if err != nil {
			os.Exit(8)
		}
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		os.Exit(0)
	}
	if os.Args[1] != "-no-proxy" {
		return
	}
	var listen, serverDir string
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "-listen" {
			listen = os.Args[i+1]
		} else if os.Args[i] == "-server-dir" {
			serverDir = os.Args[i+1]
		}
	}
	if os.Getenv("KONOMITV_SERVICE_TEST_CHECK_PORT_ORDER") != "" {
		_, portText, _ := net.SplitHostPort(listen)
		port, _ := strconv.Atoi(portText)
		time.Sleep(100 * time.Millisecond)
		public, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port-10)))
		if err != nil {
			os.Exit(9)
		}
		public.Close()
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		os.Exit(2)
	}
	file, err := os.OpenFile(filepath.Join(serverDir, "data", "helper-starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintf(file, "%d %s\n", os.Getpid(), listen)
	file.Close()
	quit := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "fixture"})
		case "/api/maintenance/restart", "/api/maintenance/shutdown":
			if r.URL.Path == "/api/maintenance/restart" {
				_ = os.WriteFile(filepath.Join(serverDir, "data", "restart_required.lock"), nil, 0600)
			}
			w.WriteHeader(http.StatusNoContent)
			go func() { time.Sleep(20 * time.Millisecond); quit <- struct{}{} }()
		}
	})}
	go server.Serve(listener)
	<-quit
	server.Close()
	os.Exit(0)
}

func awaitFixtureHealth(t *testing.T, d *Definition, done chan error) {
	t.Helper()
	deadline, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for deadline.Err() == nil {
		probe, stop := context.WithTimeout(deadline, time.Second)
		err := Healthy(probe, d)
		stop()
		if err == nil {
			return
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("supervisor stopped before ready: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("fixture backend did not start")
}

func TestSupervisorRestartReloadsConfigurationAndStopsOwnedChild(t *testing.T) {
	d := fixtureDefinition(t)
	t.Setenv("KONOMITV_SERVICE_TEST_HELPER", "backend")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			t.Error("supervisor cleanup timed out")
		}
	})
	awaitFixtureHealth(t, d, done)
	first, err := d.Launch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.77:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	if err := os.WriteFile(filepath.Join(d.Root, "config.yaml"), []byte(fmt.Sprintf("server:\n  port: %d\n", port-10)), 0600); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, first.HealthURL+"/api/maintenance/restart", nil)
	response, err := localHTTPClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	awaitFixtureHealth(t, d, done)
	starts, err := os.ReadFile(filepath.Join(d.Root, "server", "data", "helper-starts"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(starts)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[1], ":"+strconv.Itoa(port)) {
		t.Fatalf("restart did not replace the backend: %q", starts)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		done <- nil // Cleanupでも終了を観測できるようにする。
	case <-time.After(12 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	probe, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := Healthy(probe, d); err == nil {
		t.Fatal("backend remained after supervisor shutdown")
	}
}

func TestHTTPSStartsAfterBackendPortValidation(t *testing.T) {
	d := fixtureDefinition(t)
	d.ExternalHTTPS = false
	name := "akebi-https-server.elf"
	if strings.HasSuffix(d.BackendFile, ".exe") {
		name = "akebi-https-server.exe"
	}
	if err := copyFile(filepath.Join(d.Root, d.BackendFile), filepath.Join(d.Root, "server", "thirdparty", "Akebi", name), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_SERVICE_TEST_HELPER", "backend")
	t.Setenv("KONOMITV_SERVICE_TEST_CHECK_PORT_ORDER", "1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, d) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(12 * time.Second):
			t.Error("HTTPS supervisor did not stop")
		}
	}()
	awaitFixtureHealth(t, d, done)
	launch, err := d.Launch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, publicPort, _ := net.SplitHostPort(launch.HTTPS.Args[1])
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", publicPort), 100*time.Millisecond)
		if err == nil {
			connection.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("HTTPS process did not start after backend validation")
}

func TestOwnerCrashStopsBackendProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("process ownership is implemented for Windows and Linux")
	}
	d := fixtureDefinition(t)
	d.ManagerBinary = filepath.Join(d.Root, "server", "go-native", "Service", "fixture", "manager")
	if err := save(d); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_SERVICE_TEST_HELPER", "owner")
	t.Setenv("KONOMITV_SERVICE_TEST_ROOT", d.Root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := startChild(Command{Path: self, Args: []string{"owner"}, Dir: d.Root}, filepath.Join(d.Root, "server", "logs", "owner.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.stop()
	awaitFixtureHealth(t, d, owner.done)
	starts, err := os.ReadFile(filepath.Join(d.Root, "server", "data", "helper-starts"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(starts))
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	defer process.Kill()
	if err := owner.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if processExited(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("backend PID %d survived owner termination", pid)
}
