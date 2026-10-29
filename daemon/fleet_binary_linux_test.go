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

func fleetTestCopyExecutable(t *testing.T, source, target string) {
	t.Helper()
	b, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFleetBinaryProcessCacheInvalidatesAfterExecChangesInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "exec-helper.go")
	helperSource := `package main
import (
  "os"
  "syscall"
  "time"
)
func main() {
  ready, gate, target := os.Args[1], os.Args[2], os.Args[3]
  _ = os.WriteFile(ready, []byte("ready"), 0600)
  for { if _, err := os.Stat(gate); err == nil { break }; time.Sleep(time.Millisecond) }
  _ = syscall.Exec(target, []string{target, "30"}, os.Environ())
}`
	if err := os.WriteFile(source, []byte(helperSource), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "exec-helper")
	if output, err := exec.Command("go", "build", "-o", helper, source).CombinedOutput(); err != nil {
		t.Fatalf("build exec helper: %v\n%s", err, output)
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	gate := filepath.Join(dir, "gate")
	cmd := exec.Command(helper, ready, gate, sleepPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exec helper did not become ready")
		}
		time.Sleep(time.Millisecond)
	}

	platform := newFleetBinaryPlatform().(*linuxFleetBinaryPlatform)
	before, err := platform.Process(cmd.Process.Pid, "before")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	var after fleetProcessBinary
	for {
		after, err = platform.Process(cmd.Process.Pid, "after")
		if err == nil && after.Identity != before.Identity {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process identity did not change after exec: before=%+v after=%+v err=%v", before, after, err)
		}
		time.Sleep(time.Millisecond)
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.cache) != 1 {
		t.Fatalf("cache retained stale process identities: %+v", platform.cache)
	}
	for key := range platform.cache {
		if key.PID != cmd.Process.Pid || key.Device != after.Identity.Device || key.Inode != after.Identity.Inode {
			t.Fatalf("cache key = %+v, want successor identity %+v", key, after.Identity)
		}
	}
}

func TestFleetBinarySameAndDeletedInodeClassification(t *testing.T) {
	dir := t.TempDir()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dir, "solud")
	fleetTestCopyExecutable(t, sleepPath, installed)
	cmd := exec.Command(installed, "30")
	// Multi-call coreutils (uutils, BusyBox) pick the applet from argv[0], and
	// the copy is named solud: keep argv[0] as sleep so the helper stays alive.
	cmd.Args[0] = filepath.Base(sleepPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	platform := newFleetBinaryPlatform()
	target, err := platform.Target(installed)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := platform.Process(cmd.Process.Pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if stale := classifyFleetComponent("daemon", "", "Daemon", proc, target, 0); stale.Stale || stale.BinaryDiffers {
		t.Fatalf("same inode classified stale: %+v", stale)
	}
	if proc.Version != "" {
		t.Fatalf("non-Go helper version = %q, want unknown", proc.Version)
	}

	replacement := filepath.Join(dir, "replacement")
	fleetTestCopyExecutable(t, sleepPath, replacement)
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatal(err)
	}
	newTarget, err := platform.Target(installed)
	if err != nil {
		t.Fatal(err)
	}
	oldProc, err := platform.Process(cmd.Process.Pid, "")
	if err != nil {
		t.Fatal(err)
	}
	component := classifyFleetComponent("daemon", "", "Daemon", oldProc, newTarget, 0)
	if !component.Stale || !component.BinaryDiffers || !oldProc.Deleted || !strings.Contains(component.Executable, "(deleted)") {
		t.Fatalf("unlinked process not conclusively stale: proc=%+v component=%+v", oldProc, component)
	}
	if component.Version != "" {
		t.Fatalf("unknown stale version = %q, want empty", component.Version)
	}
}

func TestFleetBinaryEmbeddedVersionMarkerFallbackAndDisagreement(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\nvar version = \"dev\"\nfunc main(){ println(version) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "solud")
	cmd := exec.Command("go", "build", "-ldflags=-X=main.version=9.8.7", "-o", binary, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build versioned helper: %v\n%s", err, output)
	}
	version, err := fleetReadBinaryVersion(binary, false)
	if err != nil || version != "9.8.7" {
		t.Fatalf("embedded version = %q, %v; want 9.8.7", version, err)
	}
	if err := os.WriteFile(binary+".version", []byte("9.8.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := fleetReadBinaryVersion(binary, true); err != nil || got != "9.8.7" {
		t.Fatalf("matching marker/embedded version = %q, %v; want 9.8.7", got, err)
	}

	markerOnly := filepath.Join(dir, "marker-only")
	if err := os.WriteFile(markerOnly, []byte("not an ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerOnly+".version", []byte("4.5.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := fleetReadBinaryVersion(markerOnly, true); err != nil || got != "4.5.6" {
		t.Fatalf("marker fallback = %q, %v; want 4.5.6", got, err)
	}
	if err := os.WriteFile(binary+".version", []byte("9.8.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetReadBinaryVersion(binary, true); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("marker/embedded disagreement = %v, want fail-closed error", err)
	}
}
