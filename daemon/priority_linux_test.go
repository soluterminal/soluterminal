//go:build linux

package main

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLowerProcessPriorityAppliesBestEffortIOClass(t *testing.T) {
	// One process, no shell: a shell forking sleep while the priorities are
	// applied leaves a member the group-wide ioprio_set walk never sees.
	cmd := exec.Command("sleep", "30")
	applyDetached(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = killChildTree(cmd.Process)
		_ = cmd.Wait()
	}()
	lowerProcessPriority(cmd.Process)
	value, _, errno := syscall.Syscall(unix.SYS_IOPRIO_GET, 2, uintptr(cmd.Process.Pid), 0)
	if errno != 0 {
		t.Fatalf("ioprio_get process group %d: %v", cmd.Process.Pid, errno)
	}
	const want = uintptr(2<<13 | 7)
	if value != want {
		t.Fatalf("ioprio=%#x want best-effort/7 (%#x)", value, want)
	}
}
