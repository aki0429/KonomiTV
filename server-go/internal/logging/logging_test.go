package logging

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// TestPythonHandlerFormat はログ出力が Python 版 (uvicorn) と同じ形式になることを検証する。
func TestPythonHandlerFormat(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(NewPythonHandler([]io.Writer{&buffer}, slog.LevelInfo))
	logger.Info("IPTV playlists updating... (2 source(s))")
	logger.Warn("Batch scan of recording folders is already running.")
	logger.Debug("this message is not output")

	lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	// 例: [2026/09/22 12:53:30.229] INFO:     IPTV playlists updating... (2 source(s))
	pattern := regexp.MustCompile(`^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] INFO:     IPTV playlists updating\.\.\. \(2 source\(s\)\)$`)
	if !pattern.MatchString(lines[0]) {
		t.Errorf("lines[0] = %q", lines[0])
	}
	// Python 版 (uvicorn) はレベル名を 8 文字にパディングする ("WARNING:" + 2 スペース)
	pattern = regexp.MustCompile(`^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] WARNING:  Batch scan of recording folders is already running\.$`)
	if !pattern.MatchString(lines[1]) {
		t.Errorf("lines[1] = %q", lines[1])
	}
}

// TestPythonHandlerAttrs は属性付きのログが key=value 形式で出力されることを検証する。
func TestPythonHandlerAttrs(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(NewPythonHandler([]io.Writer{&buffer}, slog.LevelInfo)).With(slog.String("component", "iptv"))
	logger.Info("playlists updating", slog.Int("channels", 20), slog.String("detail", "with space"))

	line := strings.TrimSuffix(buffer.String(), "\n")
	if !strings.HasSuffix(line, `component=iptv channels=20 detail="with space"`) {
		t.Errorf("line = %q", line)
	}
}

// TestAccessLogFormat はアクセスログが Python 版 (uvicorn) と同じ形式になることを検証する。
func TestAccessLogFormat(t *testing.T) {
	var buffer bytes.Buffer
	AccessLog([]io.Writer{&buffer}, "127.0.0.1:35007", "GET", "/api/iptv/channels?search=Weathernews", "HTTP/1.1", 200)

	line := strings.TrimSuffix(buffer.String(), "\n")
	pattern := regexp.MustCompile(`^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] INFO:     127\.0\.0\.1:35007 - "GET /api/iptv/channels\?search=Weathernews HTTP/1\.1" 200 OK$`)
	if !pattern.MatchString(line) {
		t.Errorf("line = %q", line)
	}
}

// TestRotatingWriterRotate は日付が変わったときにログファイルが archives へ移動されることを検証する。
func TestRotatingWriterRotate(t *testing.T) {
	directory := t.TempDir()
	logPath := directory + "/logs/KonomiTV-Server.log"
	writer, err := NewRotatingWriter(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	// 日付が変わった状態を再現する
	writer.fileDate = "20260101"
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()

	// ローテーション後のファイルには新しい行のみが含まれる
	content, err := readFileString(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "second\n" {
		t.Errorf("content = %q", content)
	}
	// アーカイブには以前の行が移動される
	archive, err := readFileString(directory + "/logs/archives/KonomiTV-Server.20260101.log")
	if err != nil {
		t.Fatal(err)
	}
	if archive != "first\n" {
		t.Errorf("archive = %q", archive)
	}
}

// TestPythonLevelName はログレベル名が Python 版と同じになることを検証する。
func TestPythonLevelName(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "DEBUG",
		slog.LevelInfo:  "INFO",
		slog.LevelWarn:  "WARNING",
		slog.LevelError: "ERROR",
	}
	for level, expected := range cases {
		if actual := pythonLevelName(level); actual != expected {
			t.Errorf("pythonLevelName(%v) = %q, want %q", level, actual, expected)
		}
	}
}

// TestAccessLogJST はアクセスログのタイムスタンプが JST で出力されることを検証する。
func TestAccessLogJST(t *testing.T) {
	var buffer bytes.Buffer
	AccessLog([]io.Writer{&buffer}, "127.0.0.1:12345", "GET", "/api/version", "HTTP/1.1", 304)
	expected := time.Now().In(constants.JST).Format("2006/01/02")
	if !strings.Contains(buffer.String(), expected) {
		t.Errorf("line = %q, want date %s", buffer.String(), expected)
	}
	if !strings.Contains(buffer.String(), "304 Not Modified") {
		t.Errorf("line = %q", buffer.String())
	}
}

// readFileString はファイルの内容を文字列として読み取る。
func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
