package api

// PG-5: 録画フォルダ一括スキャン (POST /api/maintenance/run-batch-scan) の排他ガードが
// リクエスト (= 新しい metadata.Service インスタンス) を跨ぐことを検証する回帰テスト。
//
// 移植元 Python 実装 (MaintenanceRouter.py) はモジュールグローバルの batch_scan_task で排他し、
// 実行中に届いた 2 本目のリクエストには HTTP 429 を返す。
// Go 版のハンドラー (internal/api/metadata.go) はリクエストごとに newMetadataService() で
// 新しい metadata.Service を生成するため、排他ガードも Service インスタンスを跨ぐ
// 共有スコープでなければならない。

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/metadata"
)

// pg5BlockingAnalyzer は Analyze が呼ばれたことを entered に送り、release が閉じられるまでブロックする。
// 一括スキャンを「実行中」の状態で維持するためのテスト用フェイク。
type pg5BlockingAnalyzer struct {
	entered chan struct{}
	release chan struct{}
}

func (a *pg5BlockingAnalyzer) Analyze(ctx context.Context, _ string) (*metadata.RecordedProgram, error) {
	a.entered <- struct{}{}
	select {
	case <-a.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pg5NewSyntheticRecordingFolder は合成 .ts ファイルを 1 つ持つ一時録画フォルダを作る。
func pg5NewSyntheticRecordingFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.ts")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x47}, 188), 0o600); err != nil {
		t.Fatal(err)
	}
	// 更新日時を 1 時間前にして「録画完了扱い」の判定を安定させる
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPG5BatchScanGuardSpansServiceInstances は、ハンドラーと同じ生成パターン
// (リクエストごとに newMetadataService()) で作られた 2 つの Service の間で
// 排他ガードが共有され、2 本目の RunBatchScan が (false, ErrBatchScanRunning) を返すこと
// (API では 429 に対応) を検証する。
func TestPG5BatchScanGuardSpansServiceInstances(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.Video.RecordedFolders = []string{pg5NewSyntheticRecordingFolder(t)}

	fake := &pg5BlockingAnalyzer{entered: make(chan struct{}, 2), release: make(chan struct{})}
	first := s.newMetadataService()
	first.Analyzer = fake
	first.Thumbnails = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	defer func() {
		close(fake.release)
		for range 2 {
			select {
			case <-done:
			case <-ctx.Done():
				t.Error("synthetic scan did not stop")
			}
		}
	}()

	// 1 本目のスキャンを開始し、analyzer でブロックさせて実行中の状態にする
	go func() {
		_, err := first.RunBatchScan(ctx)
		done <- err
	}()
	select {
	case <-fake.entered:
	case <-ctx.Done():
		t.Fatal("first scan did not enter analyzer")
	}

	// 2 本目は別インスタンス (現行ハンドラーと同じ生成パターン)
	second := s.newMetadataService()
	second.Analyzer = fake
	second.Thumbnails = nil
	go func() {
		_, err := second.RunBatchScan(ctx)
		done <- err
	}()
	select {
	case <-fake.entered:
		t.Error("second scan entered analyzer; want ErrBatchScanRunning (HTTP 429)")
	case err := <-done:
		done <- err // 後片付けで回収する
		if !errors.Is(err, metadata.ErrBatchScanRunning) {
			t.Errorf("second RunBatchScan error = %v, want metadata.ErrBatchScanRunning", err)
		}
	case <-ctx.Done():
		t.Error("second scan hung")
	}
}

// TestPG5BatchScanSecondRequestMapsTo429 は、実行中の一括スキャンに対して
// ハンドラー経由の 2 本目のリクエストが Python 版と同じく HTTP 429 を返すことを検証する。
func TestPG5BatchScanSecondRequestMapsTo429(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.config.Video.RecordedFolders = []string{pg5NewSyntheticRecordingFolder(t)}

	fake := &pg5BlockingAnalyzer{entered: make(chan struct{}, 2), release: make(chan struct{})}
	first := s.newMetadataService()
	first.Analyzer = fake
	first.Thumbnails = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	defer func() {
		close(fake.release)
		select {
		case <-firstDone:
		case <-ctx.Done():
			t.Error("synthetic scan did not stop")
		}
	}()

	// 1 本目のスキャンを開始し、analyzer でブロックさせて実行中の状態にする
	go func() {
		_, err := first.RunBatchScan(ctx)
		firstDone <- err
	}()
	select {
	case <-fake.entered:
	case <-ctx.Done():
		t.Fatal("first scan did not enter analyzer")
	}

	// ハンドラーは毎回 newMetadataService() で新しい Service を生成する。
	// ガードがリクエストを跨いでいれば、2 本目はスキャンを実行せず 429 になる。
	response := doJSONRequest(t, s.Handler(), http.MethodPost, "/api/maintenance/run-batch-scan", "", "", "")
	if response.Code != http.StatusTooManyRequests {
		t.Errorf("second run-batch-scan status = %d body=%s, want 429", response.Code, response.Body.String())
	}
}
