//go:build windows

package twitter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledChromeForTestingPath(t *testing.T) {
	root := t.TempDir()
	// システムに入っているChromeの優先探索を、この試験の間だけ空の場所へ向ける。
	for _, name := range []string{"PATH", "PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
		t.Setenv(name, root)
	}
	binary := filepath.Join(root, "server", "go-native", "Chrome", "chrome-win64", "chrome.exe")
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("path discovery fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := findBrowser("", filepath.Join(root, "server", "data"))
	if err != nil || got != binary {
		t.Fatalf("findBrowser() = %q, %v, want %q", got, err, binary)
	}
}
