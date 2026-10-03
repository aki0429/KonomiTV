// Package logging は Python 版 (uvicorn) と互換の形式でログを出力する。
//
// Python 版のログ形式:
//
//	[2026/09/22 12:53:30.229] INFO:     IPTV playlists updating... (2 source(s))
//	[2026/09/22 16:27:44.864] INFO:     127.0.0.1:35007 - "GET /api/version HTTP/1.1" 200 OK
//
// サーバーログは標準出力と server/logs/KonomiTV-Server.log、アクセスログは標準出力と
// server/logs/KonomiTV-Access.log に出力する (クライアントのログビューアが読むファイル) 。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// pythonHandler は slog.Handler を Python 版 (uvicorn) 互換の形式で出力するハンドラー。
type pythonHandler struct {
	writers []io.Writer
	level   slog.Level
	attrs   []slog.Attr
	groups  []string
}

// NewPythonHandler は Python 版互換のログハンドラーを生成する。
func NewPythonHandler(writers []io.Writer, level slog.Level) slog.Handler {
	return &pythonHandler{writers: writers, level: level}
}

// Enabled はログレベルが有効かどうかを返す。
func (h *pythonHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle はログを Python 版互換の形式で出力する。
func (h *pythonHandler) Handle(_ context.Context, record slog.Record) error {
	// タイムスタンプは JST で出力する (Python 版と同じ)
	timestamp := record.Time.In(constants.JST)
	dateTime := timestamp.Format("2006/01/02 15:04:05")
	milliseconds := timestamp.Nanosecond() / 1000000
	// uvicorn のフォーマッターはレベル名を 8 文字にパディングして出力する
	level := fmt.Sprintf("%s:", pythonLevelName(record.Level))
	prefix := fmt.Sprintf("[%s.%03d] %-9s ", dateTime, milliseconds, level)

	var builder strings.Builder
	builder.WriteString(prefix)
	builder.WriteString(record.Message)
	// 属性は Python 版にはないが、Go 版のデバッグに必要なため key=value 形式で追加する
	for _, attr := range h.attrs {
		writeAttr(&builder, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(&builder, attr)
		return true
	})
	builder.WriteString("\n")

	line := builder.String()
	for _, writer := range h.writers {
		if _, err := io.WriteString(writer, line); err != nil {
			return err
		}
	}
	return nil
}

// WithAttrs は属性を追加したハンドラーを返す。
func (h *pythonHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &pythonHandler{writers: h.writers, level: h.level, attrs: merged, groups: h.groups}
}

// WithGroup はグループを追加したハンドラーを返す。
func (h *pythonHandler) WithGroup(name string) slog.Handler {
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &pythonHandler{writers: h.writers, level: h.level, attrs: h.attrs, groups: groups}
}

// writeAttr は属性を key=value 形式で書き出す。
func writeAttr(builder *strings.Builder, attr slog.Attr) {
	if attr.Key == "" {
		return
	}
	value := attr.Value.Resolve()
	builder.WriteString(" ")
	builder.WriteString(attr.Key)
	builder.WriteString("=")
	// 値に空白が含まれる場合は引用符で囲む
	text := fmt.Sprintf("%v", value.Any())
	if strings.ContainsAny(text, " \t\n") {
		builder.WriteString(fmt.Sprintf("%q", text))
		return
	}
	builder.WriteString(text)
}

// pythonLevelName はログレベルを Python 版と同じ名前で返す。
func pythonLevelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARNING"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// AccessLog はアクセスログを Python 版 (uvicorn) 互換の形式で出力する。
// 移植元: uvicorn.logging.AccessFormatter の
// '[%(asctime)s.%(msecs)03d] %(levelprefix)s %(client_addr)s - "%(request_line)s" %(status_code)s'
func AccessLog(writers []io.Writer, remoteAddr string, method string, target string, protocol string, statusCode int) {
	timestamp := time.Now().In(constants.JST)
	dateTime := timestamp.Format("2006/01/02 15:04:05")
	milliseconds := timestamp.Nanosecond() / 1000000
	statusText := fmt.Sprintf("%d %s", statusCode, statusReason(statusCode))
	line := fmt.Sprintf("[%s.%03d] %-9s %s - \"%s %s %s\" %s\n",
		dateTime, milliseconds, "INFO:", remoteAddr, method, target, protocol, statusText)
	for _, writer := range writers {
		_, _ = io.WriteString(writer, line)
	}
}

// statusReason は HTTP ステータスコードの理由句を返す。
func statusReason(statusCode int) string {
	switch statusCode {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 206:
		return "Partial Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 429:
		return "Too Many Requests"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return "Unknown"
	}
}
