//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcStatusAllRoot(t *testing.T) {
	root := t.TempDir()
	write := func(pid, body string) {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("10", "Name:\tinit\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n")
	write("11", "Name:\tinit\nUid:\t1000\t1000\t1000\t1000\n")
	write("12", "Name:\tinit\nUid:\t1000\t0\t0\t0\n") // setuid: the real uid is still the user's
	write("13", "Name:\tinit\nUid:\t0\t0\n")
	write("14", "Name:\tinit\nUid:\tx\t0\t0\t0\n")
	write("15", "Name:\tinit\n")
	for pid, want := range map[int]bool{10: true, 11: false, 12: false, 13: false, 14: false, 15: false, 16: false} {
		if got := procStatusAllRoot(root, pid); got != want {
			t.Errorf("pid %d: procStatusAllRoot = %v, want %v", pid, got, want)
		}
	}
}

// A same-user program named "init" is not WSL's init, so it is not a relay
// proxy whose callers fall back to browser headers.
func TestPeerNamedInitIsNotRelayProxyUnlessRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a child would be root too")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	src, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	fake := filepath.Join(t.TempDir(), "init")
	if err := os.WriteFile(fake, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fake, "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for strings.TrimSpace(readComm(pid)) != "init" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.TrimSpace(readComm(pid)); got != "init" {
		t.Fatalf("child comm = %q, want init", got)
	}
	if peerIsRelayProxy(pid) {
		t.Fatalf("a same-user process named init (pid %d) was taken for WSL's relay", pid)
	}
}
