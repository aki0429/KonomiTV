package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/worklimit"
)

// TestCaptureRegistrationSharesRecordingDrive は実画像の取り込みが録画解析の共有枠を迂回しないことを確認する。
func TestCaptureRegistrationSharesRecordingDrive(t *testing.T) {
	c, _, folder, _ := newCatalogTest(t)
	path := filepath.Join(folder, "limited.jpg")
	if err := os.WriteFile(path, catalogJPEG(t, nil, 100), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := worklimit.AcquireDrive(context.Background(), worklimit.ResolvePath(path), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.register(ctx, path, nil, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("registration bypassed drive limit: %v", err)
	}
	release()
	if id, err := c.register(context.Background(), path, nil, false); err != nil || id <= 0 {
		t.Fatalf("registration did not resume: %d %v", id, err)
	}
}
