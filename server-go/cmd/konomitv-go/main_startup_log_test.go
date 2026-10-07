package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/aki0429/KonomiTV/server-go/internal/logging"
)

// 実 main.go の起動ログ属性を解釈し、実ハンドラーの出力を検証する。
// main()、設定ロード、DB、ネットワークは起動せず、入力はすべて合成する。
func TestMainStartupLogProxyPrivacy(t *testing.T) {
	startup := mainStartupInfoCall(t)
	cases := []struct {
		name        string
		backend     string
		wantEnabled bool
		forbidden   []string
	}{
		{name: "disabled"},
		{
			name: "enabled", backend: "http://synthetic-backend.example.invalid:7010/synthetic-enabled-path", wantEnabled: true,
			forbidden: []string{"synthetic-enabled-path"},
		},
		{
			name: "userinfo", backend: "http://synthetic-startup-user@synthetic-backend.example.invalid:7010/", wantEnabled: true,
			forbidden: []string{"synthetic-startup-user"},
		},
		{
			name: "password", backend: "http://synthetic-startup-user:synthetic-startup-password@synthetic-backend.example.invalid:7010/", wantEnabled: true,
			forbidden: []string{"synthetic-startup-user", "synthetic-startup-password"},
		},
		{
			name: "query_token", backend: "http://synthetic-backend.example.invalid:7010/?token=synthetic-startup-token", wantEnabled: true,
			forbidden: []string{"token=", "synthetic-startup-token"},
		},
		{
			name: "path", backend: "http://synthetic-backend.example.invalid:7010/synthetic-startup-private-path", wantEnabled: true,
			forbidden: []string{"synthetic-startup-private-path"},
		},
		{
			name: "fragment", backend: "https://synthetic-backend.example.invalid:7010/#synthetic-startup-fragment", wantEnabled: true,
			forbidden: []string{"synthetic-startup-fragment"},
		},
		{
			name: "encoded_credentials",
			backend: "https://synthetic%2Dencoded%2Duser:synthetic%2Dencoded%2Dpassword@synthetic-backend.example.invalid:7010/" +
				"synthetic%2Dencoded%2Dpath?token=synthetic%2Dencoded%2Dtoken#synthetic%2Dencoded%2Dfragment",
			wantEnabled: true,
			forbidden: []string{
				"synthetic%2Dencoded%2Duser", "synthetic%2Dencoded%2Dpassword", "synthetic%2Dencoded%2Dpath",
				"synthetic%2Dencoded%2Dtoken", "synthetic%2Dencoded%2Dfragment",
				"synthetic-encoded-user", "synthetic-encoded-password", "synthetic-encoded-path",
				"synthetic-encoded-token", "synthetic-encoded-fragment",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := mainStartupString(t, startup.Args[0], tc.backend)
			var attrs []slog.Attr
			for _, expr := range startup.Args[1:] {
				attrs = append(attrs, mainStartupAttr(t, expr, tc.backend))
			}
			var stdout, serverLog bytes.Buffer
			logger := slog.New(logging.NewPythonHandler([]io.Writer{&stdout, &serverLog}, slog.LevelInfo))
			logger.LogAttrs(context.Background(), slog.LevelInfo, message, attrs...)
			if stdout.Len() == 0 || stdout.String() != serverLog.String() {
				t.Fatal("startup log must reach both synthetic output writers identically")
			}

			forbidden := append([]string{}, tc.forbidden...)
			if tc.backend != "" {
				forbidden = append(forbidden, tc.backend, "http://", "https://", "synthetic-backend.example.invalid")
			}
			for _, value := range forbidden {
				if strings.Contains(stdout.String(), value) {
					t.Errorf("startup log exposed synthetic backend component %q", value)
				}
			}

			proxyAttrs := 0
			for _, attr := range attrs {
				switch attr.Key {
				case "proxy_enabled":
					proxyAttrs++
					if attr.Value.Kind() != slog.KindBool || attr.Value.Bool() != tc.wantEnabled {
						t.Errorf("proxy_enabled must be a boolean matching enabled=%v, got %v", tc.wantEnabled, attr.Value)
					}
				case "version", "listen", "server_dir", "backend_type", "encoder":
					// 起動ログの既存非 URL 属性のみ許可する。
				default:
					t.Errorf("unexpected startup attribute %q; backend URL details must not be logged", attr.Key)
				}
			}
			if proxyAttrs != 1 {
				t.Errorf("startup must log exactly one proxy_enabled attribute, got %d", proxyAttrs)
			}
			state := "proxy_enabled=" + strconv.FormatBool(tc.wantEnabled)
			if !strings.Contains(stdout.String(), " "+state+" ") {
				t.Errorf("real startup handler output is missing %q", state)
			}
		})
	}
}

func mainStartupInfoCall(t *testing.T) *ast.CallExpr {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var matches []*ast.CallExpr
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Info" {
				return true
			}
			logger, ok := selector.X.(*ast.Ident)
			if !ok || logger.Name != "logger" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			message, err := strconv.Unquote(literal.Value)
			if err == nil && message == "KonomiTV server (Go) started" {
				matches = append(matches, call)
			}
			return true
		})
	}
	if len(matches) != 1 {
		t.Fatalf("expected one real main startup logger.Info call, got %d", len(matches))
	}
	return matches[0]
}

// 未対応の式は秘匿済みと仮定せず失敗させる。期待ログの生成はしない。
func mainStartupAttr(t *testing.T, expr ast.Expr, backend string) slog.Attr {
	t.Helper()
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		t.Fatalf("unsupported startup attribute expression %T", expr)
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		t.Fatal("startup attribute must be a slog constructor")
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "slog" {
		t.Fatal("startup attribute must use slog")
	}
	key := mainStartupString(t, call.Args[0], backend)
	switch selector.Sel.Name {
	case "String":
		return slog.String(key, mainStartupString(t, call.Args[1], backend))
	case "Bool":
		comparison, ok := call.Args[1].(*ast.BinaryExpr)
		if !ok || (comparison.Op != token.NEQ && comparison.Op != token.EQL) {
			t.Fatal("unsupported startup boolean expression")
		}
		equal := mainStartupString(t, comparison.X, backend) == mainStartupString(t, comparison.Y, backend)
		if comparison.Op == token.NEQ {
			equal = !equal
		}
		return slog.Bool(key, equal)
	default:
		t.Fatalf("unsupported startup slog constructor %q", selector.Sel.Name)
		return slog.Attr{}
	}
}

func mainStartupString(t *testing.T, expr ast.Expr, backend string) string {
	t.Helper()
	if literal, ok := expr.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if ident, ok := expr.(*ast.Ident); ok && ident.Name == "backendURL" {
		return backend
	}
	// URL 以外の main 変数は公開の合成値で置き換え、実設定は読まない。
	var parts []string
	for {
		switch node := expr.(type) {
		case *ast.SelectorExpr:
			parts = append([]string{node.Sel.Name}, parts...)
			expr = node.X
		case *ast.Ident:
			parts = append([]string{node.Name}, parts...)
			values := map[string]string{
				"constants.Version":   "synthetic-version",
				"httpServer.Addr":     "127.0.0.1:7002",
				"paths.ServerDir":     "synthetic-server-dir",
				"cfg.General.Backend": "synthetic-backend-type",
				"cfg.General.Encoder": "synthetic-encoder",
			}
			if value, ok := values[strings.Join(parts, ".")]; ok {
				return value
			}
			t.Fatalf("unsupported startup value %q", strings.Join(parts, "."))
			return ""
		default:
			t.Fatalf("unsupported startup string expression %T", expr)
			return ""
		}
	}
}
