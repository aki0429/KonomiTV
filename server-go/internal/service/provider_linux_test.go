package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	fixture := os.Getenv("KONOMITV_SERVICE_PM2_FIXTURE")
	if fixture == "" || filepath.Base(os.Args[0]) != "pm2" {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "jlist" {
		data, err := os.ReadFile(fixture)
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.Write(data)
		os.Exit(0)
	}
	_ = os.WriteFile(fixture+".mutation", []byte(strings.Join(os.Args[1:], ":")), 0600)
	if strings.Join(os.Args[1:], ":") == os.Getenv("KONOMITV_SERVICE_PM2_ALLOWED_COMMAND") {
		os.Exit(0)
	}
	os.Exit(3)
}

func TestPM2NumericNameUsesVerifiedIDAndIgnoresUnrelatedArgumentShapes(t *testing.T) {
	d := fixtureDefinition(t)
	d.Name = "123"
	d.ManagerBinary = filepath.Join(d.Root, "server", "go-native", "Service", "fixture", "manager")
	shim := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(self, filepath.Join(shim, "pm2"), 0755); err != nil {
		t.Fatal(err)
	}
	rows := []any{
		map[string]any{"name": "unrelated", "pm2_env": map[string]any{"args": "a different PM2 argument representation"}},
		map[string]any{"name": d.Name, "pm2_env": map[string]any{"pm_id": 7, "pm_exec_path": d.ManagerBinary, "pm_cwd": d.Root, "args": []string{"run", "-root", d.Root}, "status": "online"}},
	}
	data, _ := json.Marshal(rows)
	fixture := filepath.Join(shim, "processes.json")
	if err := os.WriteFile(fixture, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_SERVICE_PM2_FIXTURE", fixture)
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KONOMITV_SERVICE_PM2_ALLOWED_COMMAND", "stop:7")
	if err := platformControl(context.Background(), d, "stop"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOMITV_SERVICE_PM2_ALLOWED_COMMAND", "delete:7")
	if err := platformDelete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(fixture + ".mutation")
	if err != nil || string(actual) != "delete:7" {
		t.Fatalf("operation was not scoped to verified ID: %q %v", actual, err)
	}
}

func TestPM2ForeignAndDuplicateNamesCannotBeStoppedOrDeleted(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "duplicate"}[duplicate], func(t *testing.T) {
			d := fixtureDefinition(t)
			d.ManagerBinary = filepath.Join(d.Root, "server", "go-native", "Service", "fixture", "manager")
			shim := t.TempDir()
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := copyFile(self, filepath.Join(shim, "pm2"), 0755); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"name": d.Name, "pm2_env": map[string]any{"pm_exec_path": "/foreign/manager", "pm_cwd": "/foreign", "args": []string{"run", "-root", "/foreign"}, "status": "online"}}
			rows := []any{row}
			if duplicate {
				rows = append(rows, row)
			}
			data, _ := json.Marshal(rows)
			fixture := filepath.Join(shim, "processes.json")
			if err := os.WriteFile(fixture, data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KONOMITV_SERVICE_PM2_FIXTURE", fixture)
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			for _, action := range []string{"start", "stop", "restart"} {
				if err := platformControl(context.Background(), d, action); err == nil {
					t.Fatalf("foreign service accepted %s", action)
				}
			}
			if err := platformDelete(context.Background(), d); err == nil {
				t.Fatal("foreign service was accepted for deletion")
			}
			if _, err := os.Stat(fixture + ".mutation"); !os.IsNotExist(err) {
				t.Fatal("PM2 mutation was attempted")
			}
		})
	}
}
