package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLoginShellWrapArgvResolvesBareToolsViaLoginShell pins the fix for preview
// acceptance degrading to `unavailable: preview build failed: exec: "npm":
// executable file not found in $PATH` on every task: a bare {"npm","run",…}
// argv resolved against the DAEMON's PATH, which carries no nvm. The lookup
// belongs inside a login-shell child, exactly as verify rows and panes do it.
//
// The assertions are machine-independent — no nvm or Go install is needed — so
// a revert to bare exec fails everywhere, not just on this machine.
func TestLoginShellWrapArgvResolvesBareToolsViaLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no bash to wrap with on windows; bare exec is the status quo")
	}
	argv := loginShellWrapArgv([]string{"npm", "run", "build", "--", "--base=./"})
	if len(argv) != 3 || argv[0] != "bash" || argv[1] != "-lc" {
		t.Fatalf("wrapped argv = %v, want a bash -lc wrap", argv)
	}
	if !strings.HasPrefix(argv[2], loginShellInit()+"; ") {
		t.Fatalf("script missing login-shell prelude: %q", argv[2])
	}
	if want := `; exec 'npm' 'run' 'build' '--' '--base=./'`; !strings.HasSuffix(argv[2], want) {
		t.Fatalf("script = %q, want suffix %q", argv[2], want)
	}
}

// A wrap is only useful when argv[0] would consult PATH. Everything else must
// pass through untouched — an explicit path already names the binary, and
// rewriting it would break callers (including tests) that spawn helper
// processes by absolute path.
func TestLoginShellWrapArgvPassesThroughWhenNothingToResolve(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{},
		{"/usr/bin/env", "node"},
		{"./node_modules/.bin/vite", "build"},
	} {
		got := loginShellWrapArgv(argv)
		if len(got) != len(argv) {
			t.Fatalf("loginShellWrapArgv(%v) = %v, want unchanged", argv, got)
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Fatalf("loginShellWrapArgv(%v) = %v, want unchanged", argv, got)
			}
		}
	}
}

func TestLoginShellFallbackResolvesNPMBesideNode(t *testing.T) {
	binDir := t.TempDir()
	nodeBin := filepath.Join(binDir, "node")
	npmUnixBin := filepath.Join(binDir, "npm")
	npmBin := filepath.Join(binDir, "npm.cmd")
	for _, path := range []string{nodeBin, npmUnixBin, npmBin} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	originalGOOS := loginShellGOOS
	originalLookPath := loginShellLookPath
	originalResolve := loginShellResolveNodeBin
	t.Cleanup(func() {
		loginShellGOOS = originalGOOS
		loginShellLookPath = originalLookPath
		loginShellResolveNodeBin = originalResolve
	})
	loginShellResolveNodeBin = func() (string, error) { return nodeBin, nil }
	for _, test := range []struct {
		name string
		goos string
		want string
	}{
		{name: "windows", goos: "windows", want: npmBin},
		{name: "bash missing", goos: "linux", want: npmUnixBin},
	} {
		t.Run(test.name, func(t *testing.T) {
			loginShellGOOS = test.goos
			loginShellLookPath = func(string) (string, error) { return "", errors.New("bash missing") }
			got := loginShellWrapArgv([]string{"npm", "run", "build"})
			if len(got) != 3 || got[0] != test.want {
				t.Fatalf("fallback argv=%v, want npm beside node at %s", got, test.want)
			}
		})
	}
}

func TestLoginShellCommandPath(t *testing.T) {
	originalGOOS := loginShellGOOS
	originalLookPath := loginShellLookPath
	originalOutput := loginShellCommandOutput
	t.Cleanup(func() {
		loginShellGOOS = originalGOOS
		loginShellLookPath = originalLookPath
		loginShellCommandOutput = originalOutput
	})
	loginShellGOOS = "linux"
	loginShellLookPath = func(string) (string, error) { return "/bin/bash", nil }

	realBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(realBin, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	var gotArgv []string
	loginShellCommandOutput = func(ctx context.Context, argv []string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("login-shell resolver context has no deadline")
		}
		gotArgv = append([]string(nil), argv...)
		return []byte("nvm startup banner\n" + realBin + "\n\n"), nil
	}
	got, err := loginShellCommandPath("claude")
	if err != nil || got != realBin {
		t.Fatalf("loginShellCommandPath = %q, %v; want %q", got, err, realBin)
	}
	if len(gotArgv) != 3 || gotArgv[0] != "bash" || gotArgv[1] != "-lc" {
		t.Fatalf("runner argv = %v, want bash -lc script", gotArgv)
	}
	if !strings.HasPrefix(gotArgv[2], loginShellInit()+"; ") {
		t.Fatalf("resolver script missing shared login-shell prelude: %q", gotArgv[2])
	}
	if !strings.Contains(gotArgv[2], "command -v -- 'claude'") {
		t.Fatalf("resolver script missing quoted command lookup: %q", gotArgv[2])
	}

	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "missing path", output: filepath.Join(t.TempDir(), "missing") + "\n"},
		{name: "relative path", output: "bin/claude\n"},
		{name: "empty output", output: "\n \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loginShellCommandOutput = func(context.Context, []string) ([]byte, error) {
				return []byte(tc.output), nil
			}
			if got, err := loginShellCommandPath("claude"); err == nil {
				t.Fatalf("loginShellCommandPath = %q, want error", got)
			}
		})
	}

	for _, tc := range []struct {
		name string
		prep func()
	}{
		{name: "windows", prep: func() { loginShellGOOS = "windows" }},
		{name: "bash missing", prep: func() {
			loginShellGOOS = "linux"
			loginShellLookPath = func(string) (string, error) { return "", errors.New("bash missing") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			loginShellGOOS = "linux"
			loginShellLookPath = func(string) (string, error) { return "/bin/bash", nil }
			loginShellCommandOutput = func(context.Context, []string) ([]byte, error) {
				called = true
				return nil, nil
			}
			tc.prep()
			if _, err := loginShellCommandPath("claude"); err == nil {
				t.Fatal("loginShellCommandPath returned nil error")
			}
			if called {
				t.Fatal("runner invoked despite pre-spawn guard")
			}
		})
	}
}

// A CommandContext timeout does not by itself bound Output: a background
// grandchild can keep the stdout pipe open after bash exits. The production
// runner must close those pipes after its short post-cancel grace period.
func TestLoginShellCommandOutputBoundsInheritedPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash inherited-pipe fixture is Unix-only")
	}
	runner := loginShellCommandOutput
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _ = runner(ctx, []string{"bash", "-c", "sleep 10 & echo ready"})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("login-shell output waited %s for an inherited pipe, want a bounded return", elapsed)
	}
}

func TestLoginShellCommandPathQuotesToolAndUsesLastCandidate(t *testing.T) {
	originalGOOS := loginShellGOOS
	originalLookPath := loginShellLookPath
	originalOutput := loginShellCommandOutput
	t.Cleanup(func() {
		loginShellGOOS = originalGOOS
		loginShellLookPath = originalLookPath
		loginShellCommandOutput = originalOutput
	})
	loginShellGOOS = "linux"
	loginShellLookPath = func(string) (string, error) { return "/bin/bash", nil }

	bannerPath := filepath.Join(t.TempDir(), "banner-looking-path")
	realBin := filepath.Join(t.TempDir(), "real tool")
	for _, path := range []string{bannerPath, realBin} {
		if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	loginShellCommandOutput = func(_ context.Context, argv []string) ([]byte, error) {
		if want := "command -v -- 'a'\\''b tool'"; !strings.Contains(argv[2], want) {
			t.Fatalf("resolver script = %q, want safely quoted %q", argv[2], want)
		}
		return []byte(bannerPath + "\nstartup complete\n" + realBin + "\n"), nil
	}
	got, err := loginShellCommandPath("a'b tool")
	if err != nil || got != realBin {
		t.Fatalf("loginShellCommandPath = %q, %v; want final candidate %q", got, err, realBin)
	}
}
