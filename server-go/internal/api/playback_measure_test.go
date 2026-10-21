package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/database"
	"github.com/aki0429/KonomiTV/server-go/internal/videostream"
)

// TestMeasuredHLSPlayback は明示指定した DB を読み取り専用で参照し、
// 一件の再生メタデータだけを合成 DB に移してアプリ経由の HLS を 30 秒測定する。
// 本番 DB・録画・認証・設定への書き込み、バックグラウンド処理は行わない。
func TestMeasuredHLSPlayback(t *testing.T) {
	sourcePath := os.Getenv("KONOMITV_MEASURE_DATABASE")
	if runtime.GOOS != "linux" || sourcePath == "" {
		t.Skip("opt-in Linux playback measurement requires KONOMITV_MEASURE_DATABASE")
	}
	id, err := strconv.ParseInt(os.Getenv("KONOMITV_MEASURE_VIDEO_ID"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.OpenReadOnly(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	detail, err := database.GetRecordedProgramDetail(context.Background(), source, id)
	if err != nil || detail == nil {
		t.Fatalf("read source metadata: %v", err)
	}
	var segmentMap string
	if err := source.QueryRow("SELECT segment_map FROM recorded_videos WHERE id=?", detail.Video.ID).Scan(&segmentMap); err != nil {
		t.Fatal(err)
	}
	s, localID := setupVideoStreamTest(t)
	v := detail.Video
	_, err = s.db.Exec(`UPDATE recorded_videos SET file_path=?,duration=?,container_format=?,video_codec=?,video_scan_type=?,video_frame_rate=?,video_resolution_width=?,video_resolution_height=?,has_video_stream_changes=?,segment_map=? WHERE recorded_program_id=?`,
		v.FilePath, v.Duration, v.ContainerFormat, v.VideoCodec, v.VideoScanType, v.VideoFrameRate, v.VideoResolutionWidth, v.VideoResolutionHeight, v.HasVideoStreamChanges, segmentMap, localID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Channel != nil {
		_, err = s.db.Exec("UPDATE channels SET network_id=?,service_id=?,transport_stream_id=?", detail.Channel.NetworkID, detail.Channel.ServiceID, detail.Channel.TransportStreamID)
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = source.Close()
	s.config.General.Encoder = "FFmpeg"
	libraryRoot := os.Getenv("KONOMITV_MEASURE_LIBRARIES")
	s.SetVideoSegmentEncoderFactory(videostream.NewSessionSegmentEncoderFactory(videostream.EncodingTaskOptions{
		LibraryPath: func(name string) string {
			return filepath.Join(libraryRoot, name, strings.ToLower(name)+".elf")
		},
	}))
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	base := fmt.Sprintf("%s/api/streams/video/%d/1080p/", h.URL, localID)
	response, err := client.Get(base + "playlist?session_id=measure&cache_key=fixed")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("playlist failed")
	}
	if session := s.videoStreams.Lookup("measure"); session != nil {
		defer session.Destroy()
	}
	ctx, cancel := context.WithCancel(context.Background())
	measured := make(chan playbackMeasurement, 1)
	go func() { measured <- samplePlayback(ctx) }()
	start := time.Now()
	type segmentResult struct {
		Sequence int
		Bytes    int64
		SHA256   string
		Latency  float64
	}
	segments := []segmentResult{}
	for sequence := 2; sequence < 7; sequence++ {
		requestStart := time.Now()
		response, err = client.Get(fmt.Sprintf("%ssegment?session_id=measure&cache_key=fixed&sequence=%d", base, sequence))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		hash := sha256.New()
		length, readErr := io.Copy(hash, response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != 200 || length == 0 || length%188 != 0 {
			cancel()
			t.Fatalf("sequence=%d status=%d bytes=%d err=%v", sequence, response.StatusCode, length, readErr)
		}
		segments = append(segments, segmentResult{sequence, length, fmt.Sprintf("%x", hash.Sum(nil)), time.Since(requestStart).Seconds()})
		// 先頭の 12.012～42.042 秒を約 1x 速で視聴する。keep-alive は 6 秒未満の間隔で届く。
		target := start.Add(time.Duration(float64(sequence-1) * 6.006 * float64(time.Second)))
		if delay := time.Until(target); delay > 0 {
			time.Sleep(delay)
		}
	}
	cancel()
	result := <-measured
	encoded, _ := json.Marshal(struct {
		SourceVideoID int64
		Measurement   playbackMeasurement
		Segments      []segmentResult
	}{id, result, segments})
	t.Log("PLAYBACK_MEASUREMENT " + string(encoded))
}

type playbackMeasurement struct {
	Samples         int
	Seconds         float64
	AppMeanRSSKiB   float64
	AppMeanPSSKiB   float64
	AppPeakPSSKiB   int64
	ChildMeanPSSKiB float64
	TotalCPUPercent float64
}

// samplePlayback は同じ間隔で /proc の RSS/PSS と process tree の CPU を採る。
// Linux の CLK_TCK=100 を測定前に確認すること。終了済み子プロセスの最終 tick も保持する。
func samplePlayback(ctx context.Context) playbackMeasurement {
	result := playbackMeasurement{}
	start := time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	cpu := map[int]int64{}
	root := os.Getpid()
	var initialRootTicks int64
	for {
		pids := []int{root}
		seen := map[int]bool{root: true}
		var childrenPSS int64
		for index := 0; index < len(pids); index++ {
			pid := pids[index]
			prefix := fmt.Sprintf("/proc/%d/", pid)
			stat, _ := os.ReadFile(prefix + "stat")
			fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
			if len(fields) > 12 {
				user, _ := strconv.ParseInt(fields[11], 10, 64)
				system, _ := strconv.ParseInt(fields[12], 10, 64)
				cpu[pid] = max(cpu[pid], user+system)
				if pid == root && result.Samples == 0 {
					initialRootTicks = user + system
				}
			}
			rollup, _ := os.ReadFile(prefix + "smaps_rollup")
			var rss, pss int64
			for _, line := range strings.Split(string(rollup), "\n") {
				columns := strings.Fields(line)
				if len(columns) < 2 {
					continue
				}
				switch columns[0] {
				case "Rss:":
					rss, _ = strconv.ParseInt(columns[1], 10, 64)
				case "Pss:":
					pss, _ = strconv.ParseInt(columns[1], 10, 64)
				}
			}
			if pid == root {
				result.AppMeanRSSKiB += float64(rss)
				result.AppMeanPSSKiB += float64(pss)
				result.AppPeakPSSKiB = max(result.AppPeakPSSKiB, pss)
			} else {
				childrenPSS += pss
			}
			tasks, _ := filepath.Glob(prefix + "task/*/children")
			for _, task := range tasks {
				data, _ := os.ReadFile(task)
				for _, text := range strings.Fields(string(data)) {
					child, _ := strconv.Atoi(text)
					if child > 0 && !seen[child] {
						seen[child] = true
						pids = append(pids, child)
					}
				}
			}
		}
		result.ChildMeanPSSKiB += float64(childrenPSS)
		result.Samples++
		select {
		case <-ctx.Done():
			result.Seconds = time.Since(start).Seconds()
			result.AppMeanRSSKiB /= float64(result.Samples)
			result.AppMeanPSSKiB /= float64(result.Samples)
			result.ChildMeanPSSKiB /= float64(result.Samples)
			cpu[root] -= initialRootTicks
			for _, ticks := range cpu {
				result.TotalCPUPercent += float64(ticks) / result.Seconds
			}
			return result
		case <-ticker.C:
		}
	}
}
