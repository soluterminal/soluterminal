//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type turnGitConsoleObservation struct {
	Window  uintptr
	Visible bool
	Args    []string
	Input   string
}

// Re-exec as git.exe so the real turnGit runner launches a console program
// that can report its own window. This runs before TestMain parses Git's args.
func init() {
	if os.Getenv("SOLU_TEST_TURN_GIT_CHILD") != "1" {
		return
	}
	self, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Base(self), "git.exe") {
		return
	}
	window, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	visible, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("IsWindowVisible").Call(window)
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if os.Args[len(os.Args)-1] == "console-test-error" {
		fmt.Fprintln(os.Stderr, "console-test stderr")
		os.Exit(7)
	}
	_ = json.NewEncoder(os.Stdout).Encode(turnGitConsoleObservation{
		Window: window, Visible: visible != 0, Args: os.Args[1:], Input: string(input),
	})
	os.Exit(0)
}

func TestTurnGitWindowsNoConsole(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SOLU_TEST_TURN_GIT_DRIVER") != "1" {
		// A desktop-launched daemon has a hidden console; an update successor
		// is detached. Testing only the former masks the missing child flags.
		for _, launch := range []string{"desktop", "successor"} {
			t.Run(launch, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, self, "-test.run=^TestTurnGitWindowsNoConsole$", "-test.parallel=8", "-test.v")
				cmd.Env = append(os.Environ(), "SOLU_TEST_TURN_GIT_DRIVER=1")
				if launch == "desktop" {
					applyNoWindow(cmd)
				} else {
					cmd.SysProcAttr = successorSysProcAttr()
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s daemon context: %v\n%s", launch, err, out)
				}
				t.Logf("%s daemon context:\n%s", launch, out)
			})
		}
		return
	}

	dir := t.TempDir()
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "git.exe"), bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOLU_TEST_TURN_GIT_CHILD", "1")
	for _, args := range [][]string{
		{"rev-parse", "--show-toplevel"},
		{"rev-parse", "--absolute-git-dir"},
		{"hash-object", "--stdin"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := turnGit(ctx, dir, nil, strings.NewReader("snapshot input"), args...)
			if err != nil {
				t.Fatal(err)
			}
			var got turnGitConsoleObservation
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("child output %q: %v", out, err)
			}
			if got.Window != 0 || got.Visible {
				t.Errorf("background Git acquired a console window: hwnd=%#x visible=%v", got.Window, got.Visible)
			}
			wantArgs := hardenedGitArgv(dir, args)
			if !reflect.DeepEqual(got.Args, wantArgs) || got.Input != "snapshot input" {
				t.Errorf("Git pipes/arguments changed: %+v", got)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := turnGit(ctx, dir, nil, nil, "console-test-error"); err == nil || !strings.Contains(err.Error(), "console-test stderr") {
		t.Fatalf("Git failure lost stderr: %v", err)
	}
}
