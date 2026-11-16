package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readDaemonPolicyFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func requirePolicyText(t *testing.T, doc string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(doc, want) {
			t.Errorf("daemon-test-memory.md is missing %q", want)
		}
	}
}

func firstStatementIsParallel(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	expr, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Parallel" {
		return false
	}
	receiver, ok := sel.X.(*ast.Ident)
	return ok && receiver.Name == "t"
}

func requireSerialTests(t *testing.T, file string, names ...string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(names))
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok {
			found[fn.Name.Name] = firstStatementIsParallel(fn)
		}
	}
	for _, name := range names {
		parallel, exists := found[name]
		if !exists {
			t.Errorf("%s: test %s not found", file, name)
		} else if parallel {
			t.Errorf("%s: timing-sensitive test %s starts with t.Parallel()", file, name)
		}
	}
}

func TestParallelTestsDoNotMutateProcessEnvironment(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Test") || !firstStatementIsParallel(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				mutatesEnv := receiver.Name == "t" && sel.Sel.Name == "Setenv" ||
					receiver.Name == "os" && (sel.Sel.Name == "Setenv" || sel.Sel.Name == "Unsetenv" || sel.Sel.Name == "Chdir")
				if mutatesEnv {
					t.Errorf("%s: parallel test %s calls %s.%s", file, fn.Name.Name, receiver.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}

func TestTestsInjectRestartReservationGate(t *testing.T) {
	t.Parallel()
	restartReservationIdent := "selfRestart" + "InProgress"
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if ok && ident.Name == restartReservationIdent {
				t.Errorf("%s: tests must inject a per-test reservation via Updater.restartGate or memGuardRestartDeps.gate instead of touching process-global %s", file, restartReservationIdent)
			}
			return true
		})
	}
}

func requireParallelTests(t *testing.T, file string, names ...string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(names))
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok {
			found[fn.Name.Name] = firstStatementIsParallel(fn)
		}
	}
	for _, name := range names {
		parallel, exists := found[name]
		if !exists {
			t.Errorf("%s: test %s not found", file, name)
		} else if !parallel {
			t.Errorf("%s: %s does not start with t.Parallel()", file, name)
		}
	}
}
