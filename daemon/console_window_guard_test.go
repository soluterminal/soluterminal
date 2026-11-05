package main

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// On Windows a console program started by a daemon with no console of its own
// gets a new console window, which shows on screen. These tests scan every
// Windows-buildable file in this module for child processes that could do that.

// consoleSafeCalls leave the command with no console window of its own.
var consoleSafeCalls = map[string]bool{
	"applyNoWindow":            true,
	"applyDetached":            true,
	"applyDetachedNoBreakaway": true,
	"runCaptureCmd":            true, // applies applyDetached itself
}

// consoleSafeAttrs are SysProcAttr builders that set DETACHED_PROCESS on Windows.
var consoleSafeAttrs = map[string]bool{
	"detachedSysProcAttr":       true,
	"successorSysProcAttr":      true,
	"shellInstallerSysProcAttr": true,
}

// consoleWindowAllowlist holds the spawns that may keep a console, keyed
// "file:function" or "file:function:variable". Prefer hiddenCommand; add an
// entry only for a child that needs the pane's console or cannot run on Windows.
// Top-level function values use their variable's name as the function.
var consoleWindowAllowlist = map[string]string{
	// Interactive children that must stay on the pane's console.
	"claude_team_shell.go:runClaudeTeamShim:c":            "interactive CLI on the pane's console",
	"codex_inline_shim.go:runCodexInlineShim:c":           "interactive CLI on the pane's console",
	"codex_inline_shim.go:runUnknownPaneShim:c":           "command typed in the pane, on the pane's console",
	"codex_live_supported.go:runCodexLiveTerminal:client": "interactive Codex terminal on the pane's console",
	"muse_driver.go:runMuseTurn:c":                        "stdout can be the pane's console, which a separate hidden console could not write to",

	// Children that never start on Windows.
	"background_service.go:serviceCommand":          "Linux only: every caller is behind a runtime.GOOS linux check",
	"caller_peer_lsof.go:runLsofTCPPort":            "macOS only: its one caller is in caller_peer_darwin.go",
	"claude_bootstrap.go:detectClaudeCLI":           "Unix path: returns on runtime.GOOS windows before these spawns",
	"codex_bootstrap.go:detectCodexCLI":             "Unix path: returns on runtime.GOOS windows before this spawn",
	"codex_bootstrap.go:detectCodexDirect":          "reached only from the Unix path of detectCodexCLI",
	"muse_bootstrap.go:detectMuseCLI":               "Unix path: returns on runtime.GOOS windows before these spawns",
	"login_shell.go:loginShellCommandOutput":        "called only by loginShellCommandPath, which returns on Windows first",
	"cmd/agent-fingerprint/main.go:fingerprint:cmd": "release-build tool; does not ship with the app",

	// Constructor: each daemonCommand call is checked as a spawn of its own.
	"daemon_command.go:daemonCommand:cmd": "constructor; its callers are checked instead",
}

type consoleFinding struct {
	pos     token.Position
	key     string // "file:function"
	varName string // "" when the command is not held in a variable
	problem string
}

func (f consoleFinding) allowlistKey() (string, bool) {
	if f.varName != "" {
		key := f.key + ":" + f.varName
		if _, ok := consoleWindowAllowlist[key]; ok {
			return key, true
		}
	}
	_, ok := consoleWindowAllowlist[f.key]
	return f.key, ok
}

// scanConsoleSpawns returns how many spawns a file makes and which of them, or
// which SysProcAttr assignments, can leave a console window showing.
func scanConsoleSpawns(fset *token.FileSet, rel string, file *ast.File) (spawns int, findings []consoleFinding) {
	execName := ""
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == "os/exec" {
			execName = "exec"
			if spec.Name != nil {
				execName = spec.Name.Name
			}
		}
	}
	report := func(stack []ast.Node, node ast.Node, varName, problem string) {
		findings = append(findings, consoleFinding{
			pos:     fset.Position(node.Pos()),
			key:     rel + ":" + consoleFunctionName(stack),
			varName: varName,
			problem: problem,
		})
	}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if !isExecSpawnSelector(node, execName) {
				return true
			}
			spawns++
			checkConsoleSpawn(stack, node, report)
		case *ast.Ident:
			parent, isCall := consoleParent(stack, 1).(*ast.CallExpr)
			if node.Name == "daemonCommand" && isCall && parent.Fun == node {
				spawns++
				checkConsoleSpawn(stack, node, report)
			}
		case *ast.AssignStmt:
			checkConsoleAttrAssign(stack, node, report)
		}
		return true
	})
	return spawns, findings
}

func isExecSpawnSelector(sel *ast.SelectorExpr, execName string) bool {
	x, ok := sel.X.(*ast.Ident)
	return ok && execName != "" && x.Name == execName &&
		(sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext")
}

func checkConsoleSpawn(stack []ast.Node, fun ast.Expr, report func([]ast.Node, ast.Node, string, string)) {
	call, isCall := consoleParent(stack, 1).(*ast.CallExpr)
	if !isCall || call.Fun != fun {
		report(stack, fun, "", "spawn function used as a value; call hiddenCommand instead")
		return
	}
	varName := consoleAssignedVar(consoleParent(stack, 2), call)
	if varName == "" {
		report(stack, fun, "", "command is not hidden; use hiddenCommand")
		return
	}
	if !consoleVarIsSafe(consoleBody(stack), varName) {
		report(stack, fun, varName, "command is not hidden; use hiddenCommand or applyNoWindow("+varName+")")
	}
}

// checkConsoleAttrAssign catches a SysProcAttr set from a builder that does not
// detach, with no later applyNoWindow: the assignment discards CREATE_NO_WINDOW.
func checkConsoleAttrAssign(stack []ast.Node, assign *ast.AssignStmt, report func([]ast.Node, ast.Node, string, string)) {
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return
	}
	sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "SysProcAttr" {
		return
	}
	target, ok := sel.X.(*ast.Ident)
	call, isCall := assign.Rhs[0].(*ast.CallExpr)
	if !ok || !isCall {
		return
	}
	if name, ok := call.Fun.(*ast.Ident); ok && consoleSafeAttrs[name.Name] {
		return
	}
	hiddenAfter := false
	ast.Inspect(consoleBody(stack), func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && c.Pos() > assign.End() && isCallOnVar(c, "applyNoWindow", target.Name) {
			hiddenAfter = true
		}
		return true
	})
	if !hiddenAfter {
		report(stack, assign, target.Name, "SysProcAttr set with no applyNoWindow("+target.Name+") after it")
	}
}

func consoleVarIsSafe(body ast.Node, name string) bool {
	safe := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			for helper := range consoleSafeCalls {
				if isCallOnVar(node, helper, name) {
					safe = true
				}
			}
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				return true
			}
			sel, ok := node.Lhs[0].(*ast.SelectorExpr)
			call, isCall := node.Rhs[0].(*ast.CallExpr)
			if !ok || !isCall || sel.Sel.Name != "SysProcAttr" {
				return true
			}
			target, isIdent := sel.X.(*ast.Ident)
			builder, isBuilder := call.Fun.(*ast.Ident)
			if isIdent && isBuilder && target.Name == name && consoleSafeAttrs[builder.Name] {
				safe = true
			}
		}
		return true
	})
	return safe
}

func isCallOnVar(call *ast.CallExpr, fun, name string) bool {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok || ident.Name != fun {
		return false
	}
	for _, arg := range call.Args {
		if v, ok := arg.(*ast.Ident); ok && v.Name == name {
			return true
		}
	}
	return false
}

func consoleParent(stack []ast.Node, up int) ast.Node {
	if len(stack) <= up {
		return nil
	}
	return stack[len(stack)-1-up]
}

func consoleAssignedVar(parent ast.Node, call *ast.CallExpr) string {
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) == len(assign.Rhs) {
		for i, rhs := range assign.Rhs {
			if ident, isIdent := assign.Lhs[i].(*ast.Ident); isIdent && rhs == call {
				return ident.Name
			}
		}
	}
	return ""
}

// consoleBody is the innermost function around the top of stack.
func consoleBody(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch fn := stack[i].(type) {
		case *ast.FuncLit:
			return fn.Body
		case *ast.FuncDecl:
			return fn.Body
		}
	}
	return stack[0]
}

// consoleFunctionName is the named function, or package-level variable, around
// the top of stack. Function literals take the name of whatever holds them.
func consoleFunctionName(stack []ast.Node) string {
	for _, n := range stack {
		switch node := n.(type) {
		case *ast.FuncDecl:
			return node.Name.Name
		case *ast.ValueSpec:
			return node.Names[0].Name
		}
	}
	return "?"
}

func parseConsoleFixture(t *testing.T, body string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", body, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return fset, file
}

func TestWindowsChildProcessesHideConsole(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "windows", "amd64", false

	totalSpawns := 0
	used := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "node_modules" || name == "webdist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		match, err := ctx.MatchFile(filepath.Dir(path), name)
		if err != nil || !match {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		spawns, findings := scanConsoleSpawns(fset, filepath.ToSlash(path), file)
		totalSpawns += spawns
		for _, finding := range findings {
			if key, ok := finding.allowlistKey(); ok {
				used[key] = true
				continue
			}
			t.Errorf("%s: %s (%s). A console child of the daemon shows a window on Windows; see consoleWindowAllowlist if it must keep one", finding.pos, finding.problem, finding.key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if totalSpawns < 10 {
		t.Fatalf("scan found only %d spawns; the walk is not seeing the daemon sources", totalSpawns)
	}
	for key, reason := range consoleWindowAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("allowlist entry %s has no reason", key)
		}
		if used[key] {
			continue
		}
		// The public export leaves some of these files out, so only an entry
		// whose file is present can be stale.
		file, _, _ := strings.Cut(key, ":")
		if _, statErr := os.Stat(filepath.FromSlash(file)); statErr == nil {
			t.Errorf("allowlist entry %s matches no spawn; remove it", key)
		}
	}
}

func TestConsoleGuardClassifiesSpawns(t *testing.T) {
	const (
		execImport = "package p\nimport \"os/exec\"\n"
		ctxArg     = "func f(ctx any) {\n"
	)
	tests := []struct {
		name, src string
		want      int
	}{
		{"plain spawn", execImport + ctxArg + `cmd := exec.Command("x"); cmd.Run() }`, 1},
		{"chained spawn", execImport + ctxArg + `exec.CommandContext(ctx, "x").Run() }`, 1},
		{"renamed import", "package p\nimport osexec \"os/exec\"\n" + ctxArg + `cmd := osexec.Command("x"); cmd.Run() }`, 1},
		{"function value", execImport + ctxArg + `run := exec.Command; _ = run }`, 1},
		{"package-level value", execImport + `var run = func() { exec.Command("x").Run() }`, 1},
		{"another variable hidden", execImport + ctxArg + `a := exec.Command("x"); b := exec.Command("y"); applyNoWindow(b); a.Run(); b.Run() }`, 1},
		{"daemon command", "package p\n" + ctxArg + `c := daemonCommand("x"); c.Run() }`, 1},
		{"hidden then overwritten", "package p\n" + ctxArg + `cmd := hiddenCommand(ctx, "x"); cmd.SysProcAttr = pipeChildSysProcAttr() }`, 1},
		{"applied before overwrite", execImport + ctxArg + `cmd := exec.Command("x"); applyNoWindow(cmd); cmd.SysProcAttr = pipeChildSysProcAttr() }`, 1},

		{"hidden constructor", "package p\n" + ctxArg + `hiddenCommand(ctx, "x").Run() }`, 0},
		{"applyNoWindow", execImport + ctxArg + `cmd := exec.Command("x"); applyNoWindow(cmd) }`, 0},
		{"applied after attrs", execImport + ctxArg + `cmd := exec.Command("x"); cmd.SysProcAttr = pipeChildSysProcAttr(); applyNoWindow(cmd) }`, 0},
		{"detached helper", execImport + ctxArg + `cmd := exec.Command("x"); applyDetached(cmd) }`, 0},
		{"detached daemon command", "package p\n" + ctxArg + `c := daemonCommand("x"); applyDetached(c) }`, 0},
		{"detached attrs", execImport + ctxArg + `cmd := exec.Command("x"); cmd.SysProcAttr = shellInstallerSysProcAttr() }`, 0},
		{"capture helper", execImport + ctxArg + `cmd := exec.Command("x"); runCaptureCmd(ctx, cmd) }`, 0},
		{"inside a closure", execImport + ctxArg + `run := func() { cmd := exec.Command("x"); applyDetached(cmd) }; run() }`, 0},
		{"other package Command", "package p\nimport exec \"example.com/other\"\n" + ctxArg + `exec.Command("x").Run() }`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseConsoleFixture(t, tt.src)
			_, findings := scanConsoleSpawns(fset, "fixture.go", file)
			if len(findings) != tt.want {
				t.Fatalf("got %d findings, want %d: %+v", len(findings), tt.want, findings)
			}
		})
	}
}

// Removing a real fix must fail the guard, not only a fixture's.
func TestConsoleGuardFailsWhenAFixIsRemoved(t *testing.T) {
	source, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	const fix = "applyNoWindow(c)"
	if !strings.Contains(string(source), fix) {
		t.Fatalf("agent.go no longer contains %q; pick another fix for this test", fix)
	}
	fset, file := parseConsoleFixture(t, string(source))
	if _, findings := scanConsoleSpawns(fset, "agent.go", file); len(findings) != 0 {
		t.Fatalf("unmutated agent.go has findings: %+v", findings)
	}
	fset, file = parseConsoleFixture(t, strings.Replace(string(source), fix, "", 1))
	if _, findings := scanConsoleSpawns(fset, "agent.go", file); len(findings) == 0 {
		t.Fatal("guard did not notice applyNoWindow being removed from agent.go")
	}
}
