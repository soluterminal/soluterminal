package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soluterminal/migrationfence"
)

func TestMigrationFenceProtectsClaimsAndPairingBeforeUse(t *testing.T) {
	for _, marker := range []string{migrationfence.InProgress, migrationfence.Retired} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			old := []byte("fixture: must not parse or replace this claim")
			if err := os.WriteFile(filepath.Join(dir, "claim.json"), old, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, marker), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadClaim(dir); !errors.Is(err, migrationfence.ErrFenced) {
				t.Fatalf("LoadClaim=%v", err)
			}
			if err := SaveClaim(dir, &Claim{Token: "fixture-bearer"}); !errors.Is(err, migrationfence.ErrFenced) {
				t.Fatalf("SaveClaim=%v", err)
			}
			if err := UpdateClaimToken(dir, "fixture-next"); !errors.Is(err, migrationfence.ErrFenced) {
				t.Fatalf("rotation=%v", err)
			}
			// An invalid relay would fail URL validation if the guard did not run first.
			if _, err := RunClaimFlow(context.Background(), "not-a-relay", dir); !errors.Is(err, migrationfence.ErrFenced) {
				t.Fatalf("pairing=%v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, "claim.json"))
			if err != nil || string(after) != string(old) {
				t.Fatalf("claim changed: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, marker)); err != nil {
				t.Fatalf("fence changed: %v", err)
			}
		})
	}
}

func TestMigrationFenceMainOrderingKeepsHelpersAndAgentsStateless(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	parse := strings.Index(text, "\tflag.Parse()")
	guard := strings.Index(text, "checkStartupMigrationFence(*dataDir)")
	for _, dispatch := range []string{`os.Args[1] == helperCommand`, `os.Args[1] == "agent"`} {
		if at := strings.Index(text, dispatch); at < 0 || parse < 0 || at >= parse {
			t.Fatalf("%s must precede parsing", dispatch)
		}
	}
	if guard < parse {
		t.Fatal("actual data must be parsed before fence")
	}
	for _, action := range []string{"\tattachDurableLog()", "\tmaybeRollbackAfterFailedUpdate()", "\tmaybeReexecCleanEnv()", "\trole := bootDaemonRole"} {
		if at := strings.Index(text, action); at < 0 || guard >= at {
			t.Fatalf("fence must precede %s", action)
		}
	}
}

func TestMigrationFenceMainProcessRefusesBeforeAnyNormalBoot(t *testing.T) {
	if os.Getenv("SOLU_TEST_FENCED_MAIN_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("solud", flag.ExitOnError)
		os.Args = []string{"solud", "-data", os.Getenv("SOLU_TEST_FENCE_DIR"), "-addr", "127.0.0.1:0"}
		main()
		os.Exit(99)
	}
	for _, marker := range []string{migrationfence.InProgress, migrationfence.Retired} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, marker), nil, 0600); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "-test.run=^TestMigrationFenceMainProcessRefusesBeforeAnyNormalBoot$")
			command.Dir = root
			// No personal environment or home/profile path is forwarded to the child.
			command.Env = []string{"PATH=" + root, "SOLU_TEST_FENCED_MAIN_CHILD=1", "SOLU_TEST_FENCE_DIR=" + root, "SOLU_DURABLE_LOG_DIR=off", "SOLU_SKIP_MCP_REGISTER=1"}
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != migrationfence.ExitCode || !strings.Contains(string(output), migrationfence.Message) {
				t.Fatalf("exit=%v output=%s", err, output)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("stateful boot wrote files: entries=%d error=%v", len(entries), err)
			}
		})
	}
}

func TestMigrationFenceHomeUsesPasswdFallbackWithoutChangingEnvironment(t *testing.T) {
	root := t.TempDir()
	missing := func() (string, error) { return "", errors.New("fixture unavailable") }
	for _, environment := range []func() (string, error){missing, func() (string, error) { return "relative", nil }} {
		home, err := migrationLinuxHome(environment, func() (string, error) { return root, nil })
		if err != nil || home != root {
			t.Fatalf("home=%q error=%v", home, err)
		}
	}
	if home, err := migrationLinuxHome(func() (string, error) { return root, nil }, func() (string, error) { t.Fatal("unnecessary passwd lookup"); return "", nil }); err != nil || home != root {
		t.Fatalf("home=%q error=%v", home, err)
	}
	if _, err := migrationLinuxHome(missing, missing); !errors.Is(err, migrationfence.ErrUnavailable) || strings.Contains(err.Error(), migrationfence.Message) {
		t.Fatalf("uncertainty=%v", err)
	}
}
