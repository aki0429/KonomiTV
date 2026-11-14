package worklimit

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDriveIdentity(t *testing.T) {
	mounts, err := readMounts(strings.NewReader("1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 8:2 / /mnt/disk rw - ext4 /dev/data rw\n3 1 8:2 / /mnt/bind rw - ext4 /dev/data rw\n4 2 0:1 / /mnt/disk/net\\040share rw - nfs4 host:/share rw\n5 1 0:2 / /remote rw,remote - fuse remote-device rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, platform, id string
		network            bool
	}{
		{"/mnt/disk/file", "linux", "/dev/data", false},
		{"/mnt/bind/file", "linux", "/dev/data", false},
		{"/mnt/disk2/file", "linux", "/dev/root", false},
		{"/mnt/disk/net share/file", "linux", "host:/share", true},
		{"/remote/file", "linux", "remote-device", true},
		{`e:\recordings\file`, "windows", "E:", false},
		{`\\Server\Share\file`, "windows", `\\server\share`, true},
		{`\\Server\Different\file`, "windows", `\\server\different`, true},
	} {
		id, network := driveID(tc.path, tc.platform, mounts)
		if id != tc.id || network != tc.network {
			t.Errorf("%q = %q %v; want %q %v", tc.path, id, network, tc.id, tc.network)
		}
	}
	if _, err := readMounts(strings.NewReader("invalid")); err == nil {
		t.Fatal("malformed mounts accepted")
	}
	if id, network := driveID("unmounted/file", "linux", nil); id != "unmounted/file" || network {
		t.Fatal(id, network)
	}
}

// TestLocalDriveSharedAndCancellation は録画1枠とキャプチャ4枠の要求が同じ実ドライブで競合することを確認する。
func TestLocalDriveSharedAndCancellation(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireDrive(context.Background(), filepath.Join(root, "record.ts"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := AcquireDrive(ctx, filepath.Join(root, "capture.jpg"), 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture bypassed recording: %v", err)
	}
	release()
	release()
	release2, err := AcquireDrive(context.Background(), filepath.Join(root, "capture.jpg"), 4)
	if err != nil {
		t.Fatal(err)
	}
	release2()
}

func TestSemaphoreCapacityAndCancelledRelease(t *testing.T) {
	semaphore := make(chan struct{}, 4)
	for range 4 {
		release, err := acquire(context.Background(), semaphore)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquire(ctx, semaphore); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(semaphore) != 4 {
		t.Fatal("cancellation leaked a slot")
	}
}

func TestNetworkConcurrencyAndBackgroundCancellation(t *testing.T) {
	path := `\\Server\Share\capture.jpg`
	if runtime.GOOS != "windows" {
		path = "/test-network/capture.jpg"
		drives.Lock()
		oldMounts, oldFetched := drives.mounts, drives.fetched
		drives.mounts, drives.fetched = []mount{{"/test-network", "test:/share", "nfs4", "rw"}}, time.Now()
		drives.Unlock()
		defer func() { drives.Lock(); drives.mounts, drives.fetched = oldMounts, oldFetched; drives.Unlock() }()
	}
	for range 4 {
		release, err := AcquireDrive(context.Background(), path, 4)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := AcquireDrive(ctx, path, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("network cap: %v", err)
	}
	// 録画1枠はネットワーク上ではキャプチャ4枠と別キー。
	release, err := AcquireDrive(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := AcquireBackground(ctx2, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("background cap: %v", err)
	}
	if len(background) != 0 {
		t.Fatal("cancelled drive wait retained CPU slot")
	}
}
