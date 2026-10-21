package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// 実起動時の flag 宣言を検査し、依存先 URL を明示しない起動が Python を要求しないことを保護する。
func TestStandaloneProxyDefaults(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	noProxy, emptyBackend := false, false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "flag" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		name, _ := strconv.Unquote(literal.Value)
		if name == "no-proxy" && selector.Sel.Name == "Bool" {
			value, ok := call.Args[1].(*ast.Ident)
			noProxy = ok && value.Name == "true"
		}
		if name == "python-backend" && selector.Sel.Name == "String" {
			value, ok := call.Args[1].(*ast.BasicLit)
			if ok {
				text, _ := strconv.Unquote(value.Value)
				emptyBackend = text == ""
			}
		}
		return true
	})
	if !noProxy || !emptyBackend {
		t.Fatal("default startup must disable Python proxy with no configured Python URL")
	}
}
