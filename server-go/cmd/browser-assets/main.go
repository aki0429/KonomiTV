// browser-assets はTwitter連携用ChromeをKonomiTV専用のディレクトリへ導入する。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/aki0429/KonomiTV/server-go/internal/browserassets"
)

func main() {
	var options browserassets.Options
	flag.StringVar(&options.Output, "output", "../server/go-native/Chrome", "browser installation directory")
	flag.StringVar(&options.Archive, "archive", "", "reuse an official ZIP (SHA-256 is still verified)")
	flag.BoolVar(&options.UbuntuLibraries, "ubuntu-libraries", false, "extract Ubuntu dependencies into the browser directory without installing OS packages")
	flag.StringVar(&options.AppArmorProfile, "apparmor-profile", "", "use an existing AppArmor profile via aa-exec")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	manifest, err := browserassets.Install(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Chrome %s verified: %s (%d files)\n", manifest.Release.Version, options.Output, len(manifest.Files))
}
