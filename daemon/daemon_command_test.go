package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenamedDaemonCommandHelper(t *testing.T) {
	if os.Getenv("SOLU_TEST_RENAMED_DAEMON") != "1" {
		return
	}
	// Exercise production dispatch with the argv0 actually received by exec.
	os.Args = []string{os.Args[0], "--version"}
	main()
}

func TestRenamedDaemonInternalCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix running-image rename")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(t.TempDir(), "solud.old")
	// A hard link is a real alternate executable name, not a script that resets argv0.
	if err := os.Link(self, renamed); err != nil {
		t.Fatal(err)
	}
	for _, shell := range []bool{false, true} {
		name := "direct"
		if shell {
			name = "shell"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"-test.run=^TestRenamedDaemonCommandHelper$"}
			cmd := daemonCommand(renamed, args...)
			if shell {
				argv := daemonCommandArgv(renamed, args...)
				cmd = exec.Command(argv[0], argv[1:]...)
			}
			cmd.Env = append(os.Environ(), "SOLU_TEST_RENAMED_DAEMON=1")
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != version {
				t.Fatalf("renamed daemon %s: %v: %s", name, err, out)
			}
		})
	}
}
