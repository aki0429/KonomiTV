package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aki0429/KonomiTV/server-go/internal/frameextract"
	"os"
	"os/exec"
	"path/filepath"
)

func (r *FFMpegThumbnailRenderer) extractWorkerFrames(ctx context.Context, params ThumbnailRenderParams, offsets []float64, directory string) error {
	request := frameextract.Request{LibraryDirectory: r.FrameLibraryPath, FilePath: params.FilePath,
		ContainerFormat: params.ContainerFormat, Offsets: offsets, OutputDirectory: directory}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, r.FrameWorkerPath)
	command.Stdin = bytes.NewReader(payload)
	command.Env = append(os.Environ(), "LD_LIBRARY_PATH="+r.FrameLibraryPath)
	stdout, stderr := &limitedWriter{limit: 4096}, &limitedWriter{limit: 4096}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("thumbnail worker: %w: %s", err, stderr.String())
	}
	var result frameextract.Result
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
		return fmt.Errorf("decode thumbnail worker result: %w", err)
	}
	if result.Frames != len(offsets) {
		return fmt.Errorf("thumbnail worker returned %d frames, expected %d", result.Frames, len(offsets))
	}
	for index := range offsets {
		if _, err := os.Stat(filepath.Join(directory, frameFileName(index+1))); err != nil {
			return fmt.Errorf("thumbnail worker output: %w", err)
		}
	}
	r.logger().Debug("Thumbnail frames extracted through Go worker.", "frames", result.Frames,
		"black_frames", result.BlackFrames, "ffmpeg_version", result.LibraryVersion)
	return nil
}
