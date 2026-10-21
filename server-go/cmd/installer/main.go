// installer は署名鍵や追加ビルドツールを必要としない対話式のGo版インストーラー。
package main

import (
	"archive/zip"
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aki0429/KonomiTV/server-go/internal/setup"
)

func run() error {
	dir := flag.String("dir", "", "new installation directory (must not exist)")
	iptv := flag.String("iptv", "", "enabled or disabled; disabled blocks IPTV UI/API; common code is bundled")
	backend := flag.String("backend", "EDCB", "EDCB, Mirakurun or IPTV")
	backendURL := flag.String("backend-url", "", "tuner/backend URL (EDCB tcp:// or Mirakurun http(s)://)")
	recordings := flag.String("recordings", "", "existing recording directory to read (never copied or deleted)")
	archive := flag.String("dependency-archive", "", "cached official ZIP (same fixed SHA256 verification)")
	yes := flag.Bool("yes", false, "non-interactive; requires --dir and --iptv")
	remove := flag.Bool("uninstall", false, "remove unchanged managed app files only, preserving user data")
	version := flag.Bool("version", false, "show installer version")
	flag.Parse()
	if *version {
		fmt.Println(setup.Version)
		return nil
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	input := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		s, _ := input.ReadString('\n')
		return strings.TrimSpace(s)
	}
	if *dir == "" && !*yes {
		*dir = ask("New installation directory: ")
	}
	if *remove {
		if *dir == "" {
			return fmt.Errorf("--dir is required")
		}
		if !*yes && ask("Remove managed application files, retain all user data? Type YES: ") != "YES" {
			return fmt.Errorf("cancelled")
		}
		return setup.Uninstall(*dir)
	}
	if *iptv == "" && !*yes {
		*iptv = ask("IPTV enabled / disabled (disabled removes UI and blocks APIs; shared code is bundled): ")
	}
	if *dir == "" || (*iptv != "enabled" && *iptv != "disabled") {
		return fmt.Errorf("--dir and --iptv enabled|disabled are required")
	}
	if !*yes {
		fmt.Println("Dependencies will be fetched from a fixed official KonomiTV Actions artifact and checked with SHA256.")
		if ask("Continue? Type YES: ") != "YES" {
			return fmt.Errorf("cancelled")
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(self)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	z, err := zip.NewReader(f, info.Size())
	if err != nil {
		return fmt.Errorf("installer payload is missing or invalid: %w", err)
	}
	o := setup.Options{Directory: *dir, IPTV: *iptv == "enabled", Backend: *backend, BackendURL: *backendURL, DependencyArchive: *archive}
	if *recordings != "" {
		o.Recordings = []string{*recordings}
	}
	return setup.Install(context.Background(), z, o)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Installation failed:", err)
		os.Exit(1)
	}
}
