package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Child modes for the handoff tests. The successor check depends on the
// parent process, so each case runs claimDataDirForBoot in a child of this
// test process.
func init() {
	switch os.Getenv("SOLU_HANDOFF_TEST_CHILD") {
	case "boot":
		claimDataDirForBoot(os.Getenv("SOLU_HANDOFF_TEST_DIR"), dataDirLockHolder{})
		os.Stdout.WriteString("BOOTED\n")
		os.Exit(0)
	case "successor-hold":
		got := claimDataDirForBoot(os.Getenv("SOLU_HANDOFF_TEST_DIR"), dataDirLockHolder{})
		if got != nil {
			os.Stdout.WriteString("TOOK-LIFETIME-LOCK\n")
			os.Exit(0)
		}
		if dataDirHandoffHeld == nil {
			os.Stdout.WriteString("NO-HANDOFF\n")
			os.Exit(0)
		}
		os.Stdout.WriteString("HOLDING\n")
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
}

func handoffChild(t *testing.T, mode, dir string, successor bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	succ := ""
	if successor {
		succ = "1"
	}
	cmd.Env = append(os.Environ(),
		"SOLU_HANDOFF_TEST_CHILD="+mode,
		"SOLU_HANDOFF_TEST_DIR="+dir,
		"SOLU_SUCCESSOR_OF_UPDATE="+succ,
		"SOLU_DATA_DIR_LOCK=",
	)
	return cmd
}

func wantExitOwned(t *testing.T, cmd *exec.Cmd, mention string) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitDataDirOwned {
		t.Fatalf("child exit = %v, want status %d; output:\n%s", err, exitDataDirOwned, out)
	}
	if mention != "" && !strings.Contains(string(out), mention) {
		t.Fatalf("output should mention %q:\n%s", mention, out)
	}
}

// The real successor: its parent holds the lifetime lock. It must not take
// the lifetime lock at boot, and it must hold the handoff lock.
func TestSuccessorOfLockHolderHoldsHandoff(t *testing.T) {
	dir := t.TempDir()
	parent, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Release()

	child := handoffChild(t, "successor-hold", dir, true)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "HOLDING" {
		t.Fatalf("successor child said %q, want HOLDING", line)
	}
	if !handoffInProgress(dir) {
		t.Fatalf("the successor's handoff lock should be visible")
	}

	// The window the handoff lock closes: the predecessor is gone, the
	// successor doesn't hold the lifetime lock yet, and an ordinary daemon
	// starts. It must leave before loading anything.
	parent.Release()
	wantExitOwned(t, handoffChild(t, "boot", dir, false), "update successor is taking over")
	// A second process with the successor flag, started in the same window,
	// finds the lifetime lock free but must not take it either.
	wantExitOwned(t, handoffChild(t, "boot", dir, true), "this process is not it")
}

// A process with the successor flag whose parent is not the holder (a forged
// or stray flag) must not skip the lock.
func TestSuccessorFlagWithoutHolderParentIsRefused(t *testing.T) {
	dir := t.TempDir()
	other, err := acquireDataDirLock(dir, dataDirLockHolder{PID: 999999, Addr: "127.0.0.1:48887"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	wantExitOwned(t, handoffChild(t, "boot", dir, true), "does not hold the data directory")
}

// A successor whose predecessor never held the lock (lock unusable or
// disabled there) takes it at boot instead of waiting for nobody.
func TestSuccessorTakesFreeLockAtBoot(t *testing.T) {
	dir := t.TempDir()
	out, err := handoffChild(t, "successor-hold", dir, true).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "TOOK-LIFETIME-LOCK") {
		t.Fatalf("successor with a free lock: err=%v output:\n%s", err, out)
	}
}

// Once the successor holds the lifetime lock, the handoff lock is released.
func TestClaimAfterHandoffReleasesHandoffLock(t *testing.T) {
	dir := t.TempDir()
	h, held, err := tryDataDirHandoffLock(dir, true)
	if err != nil || !held {
		t.Fatalf("handoff lock: held=%v err=%v", held, err)
	}
	dataDirHandoffHeld = h
	t.Setenv("SOLU_SUCCESSOR_OF_UPDATE", "1")
	t.Setenv("SOLU_DATA_DIR_LOCK", "")
	prev := dataDirLockHeld
	dataDirLockHeld = nil
	defer func() {
		dataDirLockHeld.Release()
		dataDirLockHeld = prev
	}()
	claimDataDirAfterHandoff(dir, dataDirLockHolder{})
	if dataDirLockHeld == nil {
		t.Fatalf("successor did not take the lifetime lock")
	}
	if dataDirHandoffHeld != nil || handoffInProgress(dir) {
		t.Fatalf("handoff lock still held after the lifetime lock was taken")
	}
}

// Checking for a handoff must not create the handoff file: an ordinary boot
// (and the update-only boot, update_only_boot_test.go) writes nothing new.
func TestHandoffCheckCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if handoffInProgress(dir) {
		t.Fatalf("no successor, yet a handoff was reported")
	}
	if _, err := os.Stat(filepath.Join(dir, dataDirHandoffLockName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check created %s (stat err=%v)", dataDirHandoffLockName, err)
	}
}
