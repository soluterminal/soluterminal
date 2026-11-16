package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDataDirLockRefusesSecondOwnerAndNamesTheFirst(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDataDirLock(dir, dataDirLockHolder{Addr: "127.0.0.1:48887", Version: "0.0.1"})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	_, err = acquireDataDirLock(dir, dataDirLockHolder{Addr: "0.0.0.0:8787"})
	var owned *errDataDirOwned
	if !errors.As(err, &owned) {
		t.Fatalf("second acquire = %v, want errDataDirOwned", err)
	}
	if owned.Holder == nil || owned.Holder.PID != os.Getpid() || owned.Holder.Addr != "127.0.0.1:48887" {
		t.Fatalf("holder = %+v, want this pid on 127.0.0.1:48887", owned.Holder)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:48887") {
		t.Fatalf("error %q should name the owner's address", err)
	}
}

func TestDataDirLockFreesOnRelease(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	second, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.Release()
}

func TestDataDirLockSeparateDirectoriesDoNotConflict(t *testing.T) {
	a, err := acquireDataDirLock(t.TempDir(), dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := acquireDataDirLock(t.TempDir(), dataDirLockHolder{})
	if err != nil {
		t.Fatalf("a different data directory must not be blocked: %v", err)
	}
	b.Release()
}

// A stale lock FILE left by a crashed daemon is not a lock: only a live
// process holding the flock refuses a new owner.
func TestDataDirLockIgnoresStaleFileContents(t *testing.T) {
	dir := t.TempDir()
	stale := `{"pid":999999,"addr":"0.0.0.0:8787"}`
	if err := os.WriteFile(filepath.Join(dir, dataDirLockName), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireDataDirLock(dir, dataDirLockHolder{Addr: "0.0.0.0:8787"})
	if err != nil {
		t.Fatalf("stale file must not block: %v", err)
	}
	lock.Release()
}

// The successor waits for its predecessor's lock instead of exiting.
func TestDataDirLockPatientAcquireWaitsForPredecessor(t *testing.T) {
	dir := t.TempDir()
	predecessor, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		predecessor.Release()
	}()
	start := time.Now()
	lock, err := acquireDataDirLockPatiently(dir, dataDirLockHolder{}, 3*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("patient acquire: %v", err)
	}
	defer lock.Release()
	if time.Since(start) < 100*time.Millisecond {
		t.Fatalf("acquired before the predecessor released")
	}
}

func TestDataDirLockPatientAcquireGivesUpAtDeadline(t *testing.T) {
	dir := t.TempDir()
	holder, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	_, err = acquireDataDirLockPatiently(dir, dataDirLockHolder{}, 80*time.Millisecond, 10*time.Millisecond)
	var owned *errDataDirOwned
	if !errors.As(err, &owned) {
		t.Fatalf("err = %v, want errDataDirOwned after the deadline", err)
	}
}

// A child process (a pane agent, an update successor) must never inherit the
// lock: when the daemon dies the next daemon has to be able to start, even
// though the agents it spawned keep running.
func TestDataDirLockIsNotInheritedByChildren(t *testing.T) {
	if os.Getenv("SOLU_DATADIR_LOCK_SLEEPER") == "1" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	dir := t.TempDir()
	lock, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	// Release is idempotent; this covers a failed child start, which on
	// Windows would otherwise leave the file open and fail TempDir cleanup.
	defer lock.Release()
	child := exec.Command(os.Args[0], "-test.run=^TestDataDirLockIsNotInheritedByChildren$")
	child.Env = append(os.Environ(), "SOLU_DATADIR_LOCK_SLEEPER=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()
	lock.Release()
	next, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatalf("a live child kept the lock: %v", err)
	}
	next.Release()
}

func TestClaimDataDirForBootHonorsOptOut(t *testing.T) {
	dir := t.TempDir()
	holder, err := acquireDataDirLock(dir, dataDirLockHolder{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	// Successors are covered by datadir_lock_handoff_test.go: the check
	// depends on the parent process, so it runs in a child.
	t.Setenv("SOLU_SUCCESSOR_OF_UPDATE", "")
	t.Setenv("SOLU_DATA_DIR_LOCK", "off")
	if got := claimDataDirForBoot(dir, dataDirLockHolder{}); got != nil {
		t.Fatalf("SOLU_DATA_DIR_LOCK=off must skip the lock")
	}
}

// The refusal path exits with the status the WSL launcher treats as final.
func TestClaimDataDirForBootExitsWhenOwned(t *testing.T) {
	if os.Getenv("SOLU_DATADIR_LOCK_CHILD") == "1" {
		claimDataDirForBoot(os.Getenv("SOLU_DATADIR_LOCK_DIR"), dataDirLockHolder{})
		os.Exit(0)
	}
	dir := t.TempDir()
	holder, err := acquireDataDirLock(dir, dataDirLockHolder{Addr: "127.0.0.1:48887"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	cmd := exec.Command(os.Args[0], "-test.run=^TestClaimDataDirForBootExitsWhenOwned$")
	cmd.Env = append(os.Environ(),
		"SOLU_DATADIR_LOCK_CHILD=1",
		"SOLU_DATADIR_LOCK_DIR="+dir,
		"SOLU_SUCCESSOR_OF_UPDATE=",
		"SOLU_DATA_DIR_LOCK=",
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitDataDirOwned {
		t.Fatalf("child exit = %v, want status %d; output:\n%s", err, exitDataDirOwned, out)
	}
	if !strings.Contains(string(out), "127.0.0.1:48887") {
		t.Fatalf("refusal should name the owner; output:\n%s", out)
	}
}
