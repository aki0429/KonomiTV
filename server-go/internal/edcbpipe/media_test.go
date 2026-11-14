package edcbpipe

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/testutil/edcbipc"
)

// TestLocalStreamMedia は明示した検証用映像だけを実パイプで転送し、実デコーダーで確認する。
func TestLocalStreamMedia(t *testing.T) {
	media, ffmpeg := os.Getenv("KONOMITV_EDCB_PIPE_MEDIA"), os.Getenv("KONOMITV_EDCB_PIPE_FFMPEG")
	if media == "" || ffmpeg == "" {
		t.Skip("set KONOMITV_EDCB_PIPE_MEDIA and KONOMITV_EDCB_PIPE_FFMPEG for native media verification")
	}
	source, err := os.Open(media)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	want := sha256.New()
	wantSize, err := io.Copy(want, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	directory := edcbipc.Directory(t)
	accept := edcbipc.StreamListener(t, edcbipc.StreamPath(directory, 0, 2345, 0))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := OpenStream(ctx, 2345, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stop()
	writer, err := accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(writer, source)
		_ = writer.Close()
		done <- err
	}()
	output, err := os.CreateTemp(t.TempDir(), "received-*.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	got := sha256.New()
	gotSize, err := io.Copy(io.MultiWriter(output, got), reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if gotSize != wantSize || string(got.Sum(nil)) != string(want.Sum(nil)) {
		t.Fatalf("media changed in the pipe: %d/%d bytes", gotSize, wantSize)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"0:v:0", "0:a:0"} {
		cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-threads", "1", "-i", output.Name(),
			"-map", stream, "-t", "1", "-threads", "1", "-f", "null", "-")
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("decode %s: %v: %s", stream, err, data)
		}
	}
	t.Logf("native pipe transfer and video/audio decoding passed: bytes=%d SHA256=%s", gotSize, fmt.Sprintf("%x", got.Sum(nil)))
}
