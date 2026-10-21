package iptv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type qualityMemoryTransport func(*http.Request) (*http.Response, error)

func (f qualityMemoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// BenchmarkDetectChannelsQualities は実一覧規模の 11152 件を同じ合成 HLS 応答で測定する。
// 最初の取得を止めた時点で goroutine と heap の profile を取得でき、外部通信は行わない。
func BenchmarkDetectChannelsQualities(b *testing.B) {
	channels := make([]*Channel, 11152)
	for index := range channels {
		channels[index] = &Channel{ID: fmt.Sprint(index), URL: fmt.Sprintf("http://synthetic.invalid/%d.m3u8", index)}
	}
	b.ReportAllocs()
	for b.Loop() {
		manager := New(Options{Config: testConfig(), HTTPClient: &http.Client{Transport: qualityMemoryTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720,BANDWIDTH=2000000\nvideo.m3u8\n")), Header: make(http.Header)}, nil
		})}})
		result := manager.DetectChannelsQualities(context.Background(), channels)
		if len(result) != len(channels) {
			b.Fatal("missing results")
		}
	}
}

// TestDetectChannelsQualitiesMemory は同時 HTTP 数を変えずに待機 goroutine 数を測定する。
func TestDetectChannelsQualitiesMemory(t *testing.T) {
	channels := make([]*Channel, 11152)
	for index := range channels {
		channels[index] = &Channel{ID: fmt.Sprint(index), URL: fmt.Sprintf("http://synthetic.invalid/%d.m3u8", index)}
	}
	gate, entered := make(chan struct{}), make(chan struct{}, QualityDetectionConcurrency)
	var once sync.Once
	manager := New(Options{Config: testConfig(), HTTPClient: &http.Client{Transport: qualityMemoryTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n")), Header: make(http.Header)}, nil
	})}})
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	runtime.GC()
	before := runtime.NumGoroutine()
	done := make(chan map[string]*QualityResult, 1)
	go func() { done <- manager.DetectChannelsQualities(context.Background(), channels) }()
	for count := 0; count < QualityDetectionConcurrency; count++ {
		<-entered
	}
	// 全件を enqueue する旧実装でも、生成が完了した後の待機数を採る。
	time.Sleep(200 * time.Millisecond)
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	t.Logf("channels=%d additional_goroutines=%d heap_inuse=%d stack_inuse=%d", len(channels), runtime.NumGoroutine()-before, stats.HeapInuse, stats.StackInuse)
	if count := runtime.NumGoroutine() - before; count > QualityDetectionConcurrency+2 {
		t.Errorf("too many waiting goroutines: %d", count)
	}
	once.Do(func() { close(gate) })
	if len(<-done) != len(channels) {
		t.Fatal("missing results")
	}
}

// TestQualityCacheBound は上限を超えた URL のみを再取得し、検出情報を削減しないことを確認する。
func TestQualityCacheBound(t *testing.T) {
	requests := 0
	manager := New(Options{Config: testConfig(), HTTPClient: &http.Client{Transport: qualityMemoryTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\nvideo.m3u8\n")), Header: make(http.Header)}, nil
	})}})
	for index := 0; index <= qualityCacheLimit; index++ {
		channel := &Channel{URL: fmt.Sprintf("http://synthetic.invalid/%d", index)}
		if result := manager.DetectChannelQualities(context.Background(), channel); len(result.Qualities) != 1 {
			t.Fatal("quality was lost")
		}
	}
	if len(manager.qualityCache) != qualityCacheLimit || len(manager.qualityCacheOrder) != qualityCacheLimit {
		t.Fatal("unbounded cache")
	}
	before := requests
	manager.DetectChannelQualities(context.Background(), &Channel{URL: "http://synthetic.invalid/1"})
	if requests != before {
		t.Fatal("retained cache entry was fetched again")
	}
	manager.DetectChannelQualities(context.Background(), &Channel{URL: "http://synthetic.invalid/0"})
	if requests != before+1 {
		t.Fatal("evicted URL was not detected again")
	}
}
