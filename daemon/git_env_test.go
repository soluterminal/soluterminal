package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// gitArgvExempt lists the files allowed to build a git argv for another
// runner: the release publish steps run in the release pipeline's own
// worktree through a login shell, and their fetch/commit/push need the
// owner's identity and credentials (release-owner machines only).
var gitArgvExempt = map[string]string{"release_train_publish.go": "release publish steps"}

// Every daemon git must come from hardenedGitCommand or gitFetchCommand
// (git_env.go). This scans the package for exec.Command/CommandContext of a
// literal (or constant) "git" anywhere else, and for []string{"git", …} argv
// lists handed to another runner.
func TestDaemonGitCallsUseHardenedCommand(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var fileNames []string
	gitNames := map[string]bool{}
	isGitLiteral := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		v = strings.ToLower(filepath.Base(filepath.ToSlash(v)))
		return err == nil && (v == "git" || v == "git.exe")
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files, fileNames = append(files, file), append(fileNames, name)
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.ValueSpec); ok {
				for i, value := range spec.Values {
					if i < len(spec.Names) && isGitLiteral(value) {
						gitNames[spec.Names[i].Name] = true
					}
				}
			}
			return true
		})
	}
	isGit := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return isGitLiteral(e) || ok && gitNames[id.Name]
	}
	argvFiles := map[string]bool{}
	for i, file := range files {
		name := fileNames[i]
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok {
					if first, ok := gitRunnerArgIndex[id.Name]; ok {
						checkGitGlobalOptions(t, fset, n.Args, first)
					}
				}
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "exec" {
					return true
				}
				arg := 0
				switch sel.Sel.Name {
				case "Command":
				case "CommandContext":
					arg = 1
				default:
					return true
				}
				if len(n.Args) > arg && isGit(n.Args[arg]) && name != "git_env.go" {
					t.Errorf("%s starts git directly; use hardenedGitCommand (git_env.go)", fset.Position(n.Pos()))
				}
			case *ast.CompositeLit:
				if len(n.Elts) == 0 || !isGit(n.Elts[0]) {
					return true
				}
				if gitArgvExempt[name] == "" {
					t.Errorf("%s builds a git argv for another runner; use hardenedGitCommand (git_env.go)", fset.Position(n.Pos()))
				}
				argvFiles[name] = true
			}
			return true
		})
	}
	for name := range gitArgvExempt {
		if _, err := os.Stat(name); err == nil && !argvFiles[name] {
			t.Errorf("%s no longer builds a git argv; remove its exemption", name)
		}
	}
}

// gitRunnerArgIndex is where each git runner's git arguments start.
var gitRunnerArgIndex = map[string]int{"hardenedGitCommand": 2, "hardenedGitCommandWithExcludes": 2, "runGit": 1, "runGitResult": 1, "gitRaw": 1, "gitRawWithIndex": 2, "turnGit": 4, "turnGitLimited": 5, "workerGitRun": 2}

// checkGitGlobalOptions refuses a separate-form global option (--git-dir
// <path>) among the literal arguments before the subcommand: the =form keeps
// the value from being read as the subcommand.
func checkGitGlobalOptions(t *testing.T, fset *token.FileSet, args []ast.Expr, first int) {
	t.Helper()
	for i := first; i < len(args); i++ {
		lit, ok := args[i].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		v, _ := strconv.Unquote(lit.Value)
		switch {
		case v == "-c" || v == "-C":
			i++
		case gitSeparateValueOptions[v]:
			t.Errorf("%s passes %s with a separate value; use %s=<value>", fset.Position(lit.Pos()), v, v)
		case !strings.HasPrefix(v, "-"):
			return
		}
	}
}

func TestHardenedGitArgvGuardsDiffingCommands(t *testing.T) {
	guard := []string{"--no-ext-diff", "--no-textconv"}
	for _, tc := range []struct{ args, want []string }{
		{[]string{"log", "-1"}, slices.Concat([]string{"log"}, guard, []string{"-1"})},
		{[]string{"show"}, slices.Concat([]string{"show"}, guard)},
		{[]string{"--git-dir=/v", "--work-tree=/w", "diff", "HEAD"}, slices.Concat([]string{"--git-dir=/v", "--work-tree=/w", "diff"}, guard, []string{"HEAD"})},
		{[]string{"-c", "x.y=1", "diff-tree", "-r"}, slices.Concat([]string{"-c", "x.y=1", "diff-tree"}, guard, []string{"-r"})},
		{[]string{"rev-parse", "diff"}, []string{"rev-parse", "diff"}},
		{[]string{"--git-dir", "log", "status"}, []string{"--git-dir", "log", "status"}},
		{[]string{"status"}, []string{"status"}},
	} {
		want := slices.Concat([]string{"-C", "/repo"}, hardenedGitOptions, tc.want)
		if got := hardenedGitArgv("/repo", tc.args); !slices.Equal(got, want) {
			t.Errorf("hardenedGitArgv(%q) = %q, want %q", tc.args, got, want)
		}
	}
	fetch := gitFetchCommand(context.Background(), "/repo", "origin", "main")
	if slices.Contains(fetch.Args, "protocol.allow=never") || !slices.Contains(fetch.Args, "core.fsmonitor=false") ||
		!slices.Equal(fetch.Args[len(fetch.Args)-7:], []string{"fetch", "-q", "--no-write-fetch-head", "--no-recurse-submodules", "--", "origin", "main"}) {
		t.Errorf("fetch argv = %q", fetch.Args)
	}
}

// hostileGitRepo is a checkout whose own config names a program for every
// hook git offers on the commands the daemon runs: fsmonitor, external diff,
// textconv, clean/smudge filters, gpg (on a signed HEAD), pager and hooks. Its
// tracked file f is modified. Each program touches a file in the returned
// directory. HOME points at a global config the daemon must not read.
func hostileGitRepo(t *testing.T) (repo, ran string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ran, repo, home := t.TempDir(), t.TempDir(), t.TempDir()
	tools := t.TempDir()
	tool := func(name string) string {
		path := filepath.Join(tools, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(ran, name)+"'\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = global-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetGitUserConfig(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	for name, body := range map[string]string{"f": "one\n", ".gitattributes": "f diff=evil filter=evil\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	header, message, ok := strings.Cut(git("cat-file", "commit", "HEAD"), "\n\n")
	if !ok {
		t.Fatal("unexpected commit object")
	}
	object := filepath.Join(t.TempDir(), "signed")
	signed := header + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n -----END PGP SIGNATURE-----\n\n" + message + "\n"
	if err := os.WriteFile(object, []byte(signed), 0o600); err != nil {
		t.Fatal(err)
	}
	git("reset", "-q", "--soft", git("hash-object", "-t", "commit", "-w", object))
	for key, value := range map[string]string{
		"core.fsmonitor":     tool("fsmonitor"),
		"core.pager":         tool("pager"),
		"diff.external":      tool("ext-diff"),
		"diff.evil.textconv": tool("textconv"),
		"filter.evil.clean":  tool("clean"),
		"filter.evil.smudge": tool("smudge"),
		"log.showSignature":  "true",
		"gpg.program":        tool("gpg"),
	} {
		git("config", key, value)
	}
	for _, hook := range []string{"reference-transaction", "post-index-change", "post-checkout"} {
		if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", hook), []byte("#!/bin/sh\ntouch '"+filepath.Join(ran, hook)+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Control: plain git does run each of them and reads the global config.
	for _, args := range [][]string{
		{"log", "-1", "--format=%ct"}, {"diff"}, {"diff", "--no-ext-diff"}, {"diff", "--name-only", "HEAD"}, {"status"},
		{"update-ref", "refs/heads/control", "HEAD"},
	} {
		_ = exec.Command("git", append([]string{"-C", repo}, args...)...).Run()
	}
	for _, name := range []string{"fsmonitor", "ext-diff", "textconv", "clean", "gpg", "reference-transaction"} {
		if _, err := os.Stat(filepath.Join(ran, name)); err != nil {
			t.Fatalf("fixture: plain git did not run %s", name)
		}
	}
	if git("config", "--get", "user.name") != "global-leak" {
		t.Fatal("fixture: global config not visible to plain git")
	}
	clearDir(t, ran)
	// Inherited GIT_* variables are dropped too.
	t.Setenv("GIT_EXTERNAL_DIFF", tool("env-ext-diff"))
	t.Setenv("GIT_PAGER", tool("env-pager"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor="+tool("env-fsmonitor")+"'")
	return repo, ran
}

func clearDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNothingRan(t *testing.T, ran string) {
	t.Helper()
	entries, err := os.ReadDir(ran)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("daemon git ran the repository's %s", entry.Name())
	}
}

func TestHardenedGitNeverRunsRepositoryPrograms(t *testing.T) {
	repo, ran := hostileGitRepo(t)
	ctx := context.Background()
	for _, args := range [][]string{
		{"log", "-1", "--format=%H%x00%s%x00%cI", "--name-only"},
		{"log", "-p", "-1"},
		{"show", "HEAD"},
		{"diff", "--cached", "--stat"},
		{"rev-list", "--count", "HEAD"},
		{"cat-file", "-e", "HEAD^{commit}"},
	} {
		if _, err := runGit(repo, args...); err != nil {
			t.Errorf("runGit %v: %v", args, err)
		}
	}
	if out, err := runGit(repo, "config", "--get", "user.name"); err == nil {
		t.Errorf("runGit read the global config: user.name=%q", out)
	}
	if _, err := turnGit(ctx, repo, nil, nil, "log", "-1", "--oneline"); err != nil {
		t.Errorf("turnGit log: %v", err)
	}
	if _, err := hardenedGitStatus(repo, true, 5); err != nil {
		t.Errorf("git_status tool: %v", err)
	}
	if _, why := workerGitInspect(ctx, repo, false); why != "uncommitted work" {
		t.Errorf("worker inspect = %q, want uncommitted work", why)
	}
	if _, err := workerGitRun(ctx, repo, "log", "-1", "--format=%ct"); err != nil {
		t.Errorf("worker git log: %v", err)
	}
	if files, err := hardenedGitTrackedChanges(ctx, repo); err != nil || !slices.Equal(files, []string{"f"}) {
		t.Errorf("tracked changes = %q, %v; want [f]", files, err)
	}
	_ = gitOriginURL(ctx, repo)
	_ = canonicalRepoKey(repo)
	assertNothingRan(t, ran)
}

// The status view ignores submodules, so a checkout with one is refused
// instead of reported clean.
func TestHardenedGitTrackedChangesRefusesSubmodules(t *testing.T) {
	repo := testGitRepo(t)
	head, err := runGit(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add gitlink: %v: %s", err, out)
	}
	if files, err := hardenedGitTrackedChanges(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("tracked changes with a submodule = %q, %v; want refusal", files, err)
	}
}

func resetGitUserConfig(t *testing.T) {
	t.Helper()
	reset := func() {
		gitUserConfigCache.Lock()
		gitUserConfigCache.at = time.Time{}
		gitUserConfigCache.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// The status view's private git dir lives under the daemon's data dir, never
// the temp dir sandboxed agents can write.
func TestHardenedGitStatusViewIsDaemonPrivate(t *testing.T) {
	repo := testGitRepo(t)
	data := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	previous := gitViewDir.Load()
	setGitViewDir(data)
	t.Cleanup(func() { gitViewDir.Store(previous); gitViewCreatedForTest = nil })
	resetGitUserConfig(t)
	var views []string
	gitViewCreatedForTest = func(view string) { views = append(views, view) }
	if _, err := hardenedGitTrackedChanges(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %q", views)
	}
	root := filepath.Join(data, "git-views")
	if rel, err := filepath.Rel(os.TempDir(), views[0]); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("view %s is under the temp dir %s", views[0], os.TempDir())
	}
	if filepath.Dir(views[0]) != root {
		t.Errorf("view %s is not in %s", views[0], root)
	}
	if info, err := os.Stat(root); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("view root %s: %v %v", root, info, err)
	}
}

func TestGitUserConfigKeepsSafeDirectoryOrderAndIgnoresRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n\tdirectory = /a\n\tdirectory =\n\tdirectory = /b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := testGitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "config", "safe.directory", "/repo-local").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Chdir(repo)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'safe.directory=/from-env'")
	resetGitUserConfig(t)
	got := gitUserConfig().safeDirectories
	if len(got) < 3 || !slices.Equal(got[len(got)-3:], []string{"/a", "", "/b"}) || slices.Contains(got, "/repo-local") || slices.Contains(got, "/from-env") {
		t.Fatalf("safe directories = %q, want global /a, (reset), /b last and no repository or environment value", got)
	}
	file, err := os.ReadFile(gitUserConfig().globalConfig)
	if err != nil || !strings.HasSuffix(string(file), "\tdirectory = \"/a\"\n\tdirectory = \"\"\n\tdirectory = \"/b\"\n") {
		t.Fatalf("global config file = %q, %v", file, err)
	}
}

func TestGitUserConfigFailedReadPassesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script git shim")
	}
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	resetGitUserConfig(t)
	if got := gitUserConfig(); len(got.safeDirectories) != 0 || got.excludesFile != "" {
		t.Fatalf("failed read passed %+v", got)
	}
	for _, path := range []string{gitUserConfig().globalConfig, gitUserConfig().turnGlobalConfig} {
		if file, err := os.ReadFile(path); err != nil || string(file) != gitGlobalConfigHeader {
			t.Fatalf("global config file %s after a failed read = %q, %v", path, file, err)
		}
	}
	reason := gitFailureReason("fatal: detected dubious ownership in repository at '/x'\nTo add an exception...\n")
	if reason != "repository owned by another user (git's safe.directory check refused it)" {
		t.Fatalf("dubious ownership reason = %q", reason)
	}
}

func TestGitFailureReasonIsBoundedAndPrintable(t *testing.T) {
	got := gitFailureReason("fatal: bad \x1b[31mname\x07\n" + strings.Repeat("é", 4000))
	if len(got) > gitReasonMaxBytes || strings.ContainsAny(got, "\x1b\x07\n") || !strings.HasPrefix(got, "fatal: bad [31mname ") || !utf8.ValidString(got) {
		t.Fatalf("reason = %q (%d bytes)", got[:min(len(got), 80)], len(got))
	}
}

// The person's global excludes file applies to turn git and git_status only;
// the worker check stays strict and release git carries no excludes option.
func TestGitUserExcludesFileScope(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	excludes := filepath.Join(home, "global-ignore")
	if err := os.WriteFile(excludes, []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = "+excludes+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetGitUserConfig(t)
	repo := testGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := gitUserConfig()
	if file, err := os.ReadFile(config.turnGlobalConfig); err != nil || !strings.HasSuffix(string(file), "[core]\n\texcludesFile = "+gitConfigQuote(excludes)+"\n") {
		t.Fatalf("turn global config file = %q, %v", file, err)
	}
	if file, err := os.ReadFile(config.globalConfig); err != nil || strings.Contains(string(file), "excludesFile") {
		t.Fatalf("global config file = %q, %v", file, err)
	}
	if got, err := turnGit(context.Background(), repo, nil, nil, "config", "--get", "core.excludesFile"); err != nil || strings.TrimSpace(got) != excludes {
		t.Errorf("turn git excludes file = %q, %v", got, err)
	}
	if got, err := runGit(repo, "config", "--get", "core.excludesFile"); err == nil {
		t.Errorf("runGit sees an excludes file: %q", got)
	}
	if env := hardenedGitCommand(context.Background(), repo, "status").Env; !slices.Contains(env, "GIT_CONFIG_GLOBAL="+config.globalConfig) {
		t.Errorf("hardened git env = %q, want the safe.directory-only global config", env)
	}
	out, err := hardenedGitStatus(repo, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := out["status"].([]string); slices.Contains(status, "?? ignored.txt") {
		t.Errorf("git_status listed a globally ignored file: %q", status)
	}
	if _, why := workerGitInspect(context.Background(), repo, false); why != "uncommitted work" {
		t.Errorf("worker check = %q, want the ignored file still counted", why)
	}
	if argv := hardenedGitArgv(repo, []string{"log", "-1"}); slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "core.excludesFile") }) {
		t.Errorf("release/runGit argv carries the excludes file: %q", argv)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	resetGitUserConfig(t)
	if got := gitUserConfig().excludesFile; got != filepath.Join(home, "git", "ignore") {
		t.Errorf("default excludes file = %q, want the XDG default", got)
	}
}

func TestGitGlobalConfigContents(t *testing.T) {
	values := gitUserConfigValues{safeDirectories: []string{"/a", "", ` /b "q" \x;#`, "*"}, excludesFile: "/home/u/ignore"}
	safe := "[safe]\n\tdirectory = \"/a\"\n\tdirectory = \"\"\n\tdirectory = \" /b \\\"q\\\" \\\\x;#\"\n\tdirectory = \"*\"\n"
	if got := gitGlobalConfigContents(values, false); got != gitGlobalConfigHeader+safe {
		t.Errorf("global config = %q", got)
	}
	if got := gitGlobalConfigContents(values, true); got != gitGlobalConfigHeader+safe+"[core]\n\texcludesFile = \"/home/u/ignore\"\n" {
		t.Errorf("turn global config = %q", got)
	}
	// A value carrying a line break or control character fails closed: the
	// whole safe.directory list (one dropped reset would widen trust).
	for _, bad := range []string{"/a\n[include]\n\tpath = /x", "/a\x00", "/a\x1b[31m", "/a\r", "/a\u0085"} {
		refused := gitUserConfigValues{safeDirectories: []string{"/ok", bad}, excludesFile: bad}
		if got := gitGlobalConfigContents(refused, true); got != gitGlobalConfigHeader {
			t.Errorf("config with %q = %q, want nothing passed", bad, got)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// git reads the values back exactly.
	file := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(file, []byte(gitGlobalConfigContents(values, true)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "config", "--file", file, "-z", "--get-all", "safe.directory").Output()
	if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); err != nil || !slices.Equal(got, values.safeDirectories) {
		t.Errorf("git read back %q, %v; want %q", got, err, values.safeDirectories)
	}
}

// A repository owned by another user is trusted through the daemon-written
// global config (git honours -c safe.directory only on newer versions).
func TestHardenedGitTrustsSafeDirectoryThroughGlobalFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := testGitRepo(t)
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	revParse := func() error {
		cmd := hardenedGitCommand(context.Background(), repo, "rev-parse", "HEAD")
		// git's own switch: treat the repository as another user's.
		cmd.Env = append(cmd.Env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
		_, err := cmd.Output()
		return err
	}
	resetGitUserConfig(t)
	if !slices.Contains(gitUserConfig().safeDirectories, "*") {
		if err := revParse(); err == nil || gitErrorText(err) != "repository owned by another user (git's safe.directory check refused it)" {
			t.Fatalf("untrusted repository = %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n\tdirectory = "+gitConfigQuote(real)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetGitUserConfig(t)
	if err := revParse(); err != nil {
		t.Fatalf("repository trusted in the user's config = %v", gitErrorText(err))
	}
	root, err := gitViewRoot()
	if err != nil {
		t.Fatal(err)
	}
	config := gitUserConfig()
	for _, path := range []string{config.globalConfig, config.turnGlobalConfig} {
		info, err := os.Lstat(path)
		if err != nil || filepath.Dir(path) != root || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("global config file %s: %v %v", path, info, err)
		}
	}
	if argv := hardenedGitArgv(repo, []string{"status"}); slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, "safe.directory") }) {
		t.Errorf("argv carries safe.directory: %q", argv)
	}
}

// The status view copies the checkout's change-detection settings as
// normalised literals and nothing else: no include is followed and no
// planted filter or fsmonitor reaches it.
func TestHardenedGitStatusViewCopiesOnlyRepositoryBooleans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit and shell-script fixture")
	}
	repo := testGitRepo(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "crlf.txt"), "a\r\nb\r\n", 0o644)
	write(filepath.Join(repo, "run.sh"), "#!/bin/sh\n", 0o644)
	git("config", "core.autocrlf", "true")
	git("add", ".")
	git("commit", "-q", "-m", "files")
	git("config", "core.filemode", "false")
	if err := os.Chmod(filepath.Join(repo, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A changed mtime makes git compare crlf.txt's content, not its stat.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(repo, "crlf.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	ran, tools := t.TempDir(), t.TempDir()
	tool := func(name string) string {
		path := filepath.Join(tools, name)
		write(path, "#!/bin/sh\ntouch '"+filepath.Join(ran, name)+"'\n", 0o700)
		return path
	}
	include := filepath.Join(t.TempDir(), "included")
	write(include, "[core]\n\tfilemode = true\n\tfsmonitor = "+tool("include-fsmonitor")+"\n[filter \"evil\"]\n\tclean = "+tool("include-clean")+"\n", 0o600)
	git("config", "core.fsmonitor", tool("fsmonitor"))
	git("config", "filter.evil.clean", tool("clean"))
	git("config", "include.path", include)
	write(filepath.Join(repo, ".gitattributes"), "* filter=evil\n", 0o644)
	var config string
	gitViewCreatedForTest = func(view string) {
		b, _ := os.ReadFile(filepath.Join(view, "config"))
		config = string(b)
	}
	t.Cleanup(func() { gitViewCreatedForTest = nil })
	ctx := context.Background()
	if files, err := hardenedGitTrackedChanges(ctx, repo); err != nil || len(files) != 0 {
		t.Fatalf("tracked changes with filemode=false and autocrlf = %q, %v; want clean", files, err)
	}
	if want := "[core]\n\tfilemode = false\n\tautocrlf = true\n"; config != want {
		t.Errorf("view config = %q, want %q", config, want)
	}
	assertNothingRan(t, ran)
	// Control: without those settings the same checkout reads as changed.
	git("config", "core.filemode", "true")
	git("config", "core.autocrlf", "false")
	if files, err := hardenedGitTrackedChanges(ctx, repo); err != nil || !slices.Equal(files, []string{"crlf.txt", "run.sh"}) {
		t.Fatalf("tracked changes with filemode=true = %q, %v", files, err)
	}
	// A value outside the allowed literals fails closed.
	git("config", "core.eol", "bogus")
	if files, err := hardenedGitTrackedChanges(ctx, repo); err == nil {
		t.Fatalf("tracked changes with core.eol=bogus = %q, want an error", files)
	}
	git("config", "--unset", "core.eol")
	// (git itself reads core.precomposeunicode only on macOS.)
	git("config", "core.precomposeunicode", "maybe")
	if files, err := hardenedGitTrackedChanges(ctx, repo); err == nil {
		t.Fatalf("tracked changes with core.precomposeunicode=maybe = %q, want an error", files)
	}
	assertNothingRan(t, ran)
}
