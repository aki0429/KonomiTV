package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 全 Python route が Go 登録/dispatcher と分類台帳に存在することを検査する。
// 登録検査は、全正常系の実機受入れに代わるものではない。
func TestAllPythonRoutesHaveNativeRegistrationAndClassification(t *testing.T) {
	prefixPattern := regexp.MustCompile(`prefix\s*=\s*['"]([^'"]+)['"]`)
	routePattern := regexp.MustCompile(`@router\.(get|post|put|delete|patch|options|head)\(\s*['"]([^'"]*)['"]`)
	pythonFiles, err := filepath.Glob("../../../server/app/routers/*Router.py")
	if err != nil || len(pythonFiles) == 0 {
		t.Fatal("Python source inventory unavailable")
	}
	expected := map[string]bool{}
	for _, name := range pythonFiles {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		prefix := prefixPattern.FindSubmatch(source)
		if len(prefix) == 0 {
			continue
		}
		for _, match := range routePattern.FindAllSubmatch(source, -1) {
			expected[strings.ToUpper(string(match[1]))+" "+string(prefix[1])+string(match[2])] = true
		}
	}
	if len(expected) != 115 {
		t.Fatalf("Python inventory changed: %d, update audit", len(expected))
	}
	registrations := map[string]string{}
	functions := map[string]*ast.FuncDecl{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range source.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok {
				functions[function.Name.Name] = function
			}
		}
		ast.Inspect(source, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "HandleFunc" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			route, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			handler, ok := call.Args[1].(*ast.SelectorExpr)
			if ok {
				registrations[route] = handler.Sel.Name
			}
			return true
		})
	}
	audit, err := os.ReadFile("../../API_IMPLEMENTATION_AUDIT.md")
	if err != nil {
		t.Fatal(err)
	}
	classified := map[string]bool{}
	for _, line := range strings.Split(string(audit), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 5 || strings.TrimSpace(parts[3]) == "" {
			t.Fatalf("unclassified route: %s", line)
		}
		classified[strings.Trim(strings.TrimSpace(parts[1]), "`")] = true
	}
	if len(classified) != 115 {
		t.Fatalf("audit route count=%d", len(classified))
	}
	legacyAllowed := map[string]bool{"POST /api/programs/search": true, "POST /api/maintenance/update-database": true, "GET /api/streams/video/{video_id}/{quality}/offline-stream": true}
	categoryCount := map[string]int{}
	for route := range expected {
		if !classified[route] {
			t.Errorf("unclassified Python route %s", route)
		}
		registration := route
		if strings.Contains(route, "/api/bluesky/") && strings.Contains(route, "/{post_id:path}/") {
			registration = strings.Split(route, "/{post_id:path}/")[0] + "/{rest...}"
		}
		handler := registrations[registration]
		if strings.Contains(route, "/api/data-broadcasting/request/") {
			handler = "handleDataBroadcastingProxy"
		}
		if handler == "" || functions[handler] == nil {
			t.Errorf("missing native handler %s (%s)", route, registration)
			continue
		}
		category := "Go/local"
		switch {
		case strings.Contains(route, "/api/twitter/") && !strings.Contains(route, "video-proxy") && route != "DELETE /api/twitter/accounts/{screen_name}":
			category = "Go/CDP"
		case strings.Contains(route, "/api/streams/"):
			category = "Go/encoder-backend"
		case strings.Contains(route, "/api/recording/") || route == "POST /api/programs/search":
			category = "Go/EDCB"
		case strings.Contains(route, "/api/bluesky/") || strings.Contains(route, "/api/niconico/") || strings.Contains(route, "/api/data-broadcasting/") || strings.Contains(route, "/api/iptv/") || strings.Contains(route, "jikkyo") || strings.Contains(route, "video-proxy"):
			category = "Go/external-HTTP"
		}
		categoryCount[category]++
		// 登録 handler から Server メソッドを辿り、Python 転送の残存を明示する。
		seen := map[string]bool{}
		var hasProxy func(string) bool
		hasProxy = func(name string) bool {
			if seen[name] {
				return false
			}
			seen[name] = true
			function := functions[name]
			if function == nil {
				return false
			}
			found := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if selector.Sel.Name == "ServeHTTP" {
					if field, ok := selector.X.(*ast.SelectorExpr); ok && field.Sel.Name == "proxy" {
						found = true
					}
				}
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "s" {
					if hasProxy(selector.Sel.Name) {
						found = true
					}
				}
				return true
			})
			return found
		}
		forwards := hasProxy(handler)
		if forwards != legacyAllowed[route] {
			t.Errorf("Python forwarding boundary changed: %s handler=%s forwards=%v", route, handler, forwards)
		}
	}
	keys := []string{}
	for category := range categoryCount {
		keys = append(keys, category)
	}
	sort.Strings(keys)
	for _, category := range keys {
		t.Logf("%s: %d routes", category, categoryCount[category])
	}
	t.Log("115/115 classified and statically registered; three explicit legacy-proxy branches remain, all have native no-proxy implementations")
}
