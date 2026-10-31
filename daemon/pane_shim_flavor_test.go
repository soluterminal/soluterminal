package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUnknownPaneShimMainHelper(t *testing.T) {
	if os.Getenv("SOLU_TEST_UNKNOWN_SHIM") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Getenv("SOLU_TEST_SHIM_ARGV0")}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing argument separator")
}

func TestDaemonInvocationNames(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		daemon bool
	}{
		{"solud", true},
		{"solud.exe", true},
		{"solud-0.0.1231-candidate", true},
		{"solud-0.0.1231-candidate.exe", true},
		{"solud-verify", true},
		{"solud-verify.exe", true},
		{"claude", false},
		{"codex", false},
		{"solud.old", false},
		{"soludish", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestUnknownPaneShimMainHelper$", "--", "--version")
			root := t.TempDir()
			// No real CLI or inherited shim environment: a shim must refuse
			// here rather than answering the daemon's --version command.
			cmd.Env = append(filterOutEnvKeys(os.Environ(), []string{"PATH", codexInlineShimEnvKey, "HOME"}),
				"PATH="+root, "HOME="+root, "SOLU_TEST_UNKNOWN_SHIM=1", "SOLU_TEST_SHIM_ARGV0="+filepath.Join(root, tc.name))
			out, err := cmd.CombinedOutput()
			if tc.daemon {
				if err != nil || strings.TrimSpace(string(out)) != version {
					t.Fatalf("daemon invocation %s: %v: %s", tc.name, err, out)
				}
			} else {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 127 || !strings.HasPrefix(string(out), tc.name+":") {
					t.Fatalf("shim invocation %s: %v: %s", tc.name, err, out)
				}
			}
		})
	}
}

func TestUnknownPaneShimNeverBoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixtures")
	}
	for _, tc := range []struct {
		label, name string
		real, env   bool
		args        []string
	}{
		{"args", "unknown-command", true, true, []string{"one", "two words"}},
		{"no-env", "unknown-command", true, false, []string{"--version"}},
		{"missing", "unknown-command", false, true, []string{"--addr", "127.0.0.1:0"}},
		{"public-claude", "claude", true, true, []string{"--version"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if _, ok := paneShimCommand(tc.name); ok {
				t.Skip("registered in this build")
			}
			root := t.TempDir()
			shim := filepath.Join(root, codexInlineShimDirName)
			real := filepath.Join(root, "real")
			alias := filepath.Join(root, "alias")
			for _, dir := range []string{shim, real, alias} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{shim, alias} {
				if err := os.Symlink(self, filepath.Join(dir, tc.name)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.real {
				if err := os.WriteFile(filepath.Join(real, tc.name), []byte("#!/bin/sh\nprintf 'real:%s\\n' \"$PATH\" \"$@\"\nexit 23\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestUnknownPaneShimMainHelper$", "--"}, tc.args...)
			cmd := exec.CommandContext(ctx, self, args...)
			cmd.Env = append(filterOutEnvKeys(os.Environ(), []string{"PATH", codexInlineShimEnvKey, "HOME"}), "PATH="+strings.Join([]string{shim, alias, real}, string(os.PathListSeparator)), "SOLU_TEST_UNKNOWN_SHIM=1", "SOLU_TEST_SHIM_ARGV0="+filepath.Join(shim, tc.name), "HOME="+root)
			if tc.env {
				cmd.Env = append(cmd.Env, codexInlineShimEnvKey+"="+shim)
			}
			out, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected exit: %v %s", err, out)
			}
			want := 127
			if tc.real {
				want = 23
			}
			if exit.ExitCode() != want {
				t.Fatalf("exit=%d want=%d: %s", exit.ExitCode(), want, out)
			}
			if tc.real {
				if !strings.HasPrefix(string(out), "real:") || strings.Contains(string(out), shim) {
					t.Fatalf("unexpected CLI output/PATH: %s", out)
				}
				for _, arg := range tc.args {
					if !strings.Contains(string(out), "real:"+arg+"\n") {
						t.Fatalf("argument lost: %q: %s", arg, out)
					}
				}
			} else if !strings.Contains(string(out), "not available in this build") {
				t.Fatalf("unclear missing-command error: %s", out)
			}
			if _, err := os.Stat(filepath.Join(root, ".solu")); !os.IsNotExist(err) {
				t.Fatalf("daemon wrote state: %v", err)
			}
		})
	}
}

func TestEnsureCodexInlineShimRemovesUnregisteredLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	root := t.TempDir()
	dir := filepath.Join(root, codexInlineShimDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(dir, ".codex-in-flight")
	if err := os.Symlink("/new/solud", temporary); err != nil {
		t.Fatal(err)
	}
	names := []string{"obsolete-command", "claude"}
	for _, name := range names {
		if err := os.Symlink("/old/solud", filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keep-file"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCodexInlineShim(root); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(temporary); err != nil || target != "/new/solud" {
		t.Fatalf("in-flight shim link changed: %q %v", target, err)
	}
	self, _ := os.Executable()
	for _, name := range names {
		target, err := os.Readlink(filepath.Join(dir, name))
		_, registered := paneShimCommand(name)
		if registered {
			if err != nil || target != self {
				t.Fatalf("registered shim %s: %q %v", name, target, err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("stale shim %s remains: %v", name, err)
		}
	}
	for _, shim := range paneShims {
		if target, err := os.Readlink(filepath.Join(dir, shim.name)); err != nil || target != self {
			t.Fatalf("shim %s: %q %v", shim.name, target, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "keep-file")); err != nil || string(data) != "user data" {
		t.Fatalf("ordinary file changed: %s %v", data, err)
	}
}
