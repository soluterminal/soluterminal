//go:build linux

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These run real processes laid out the way a Codex pane runs on Linux: the
// pane's shell, our solud pane shim named codex, and the npm vendor binary
// under @openai/codex-linux-x64. The binaries are copies of sleep (and, for
// the shim found by its build, of this test binary).

// TestAgentExeVersionSleeper is not a test: it is the body of the test-binary
// copy that stands in for an old solud shim, run with SOLU_AGENT_EXE_SLEEPER.
func TestAgentExeVersionSleeper(t *testing.T) {
	if os.Getenv("SOLU_AGENT_EXE_SLEEPER") != "1" {
		t.Skip("helper process body")
	}
	time.Sleep(60 * time.Second)
}

func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	if real, err := filepath.EvalSymlinks(from); err == nil {
		from = real
	}
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func sleepBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sleep")
	if err != nil {
		// Every Linux host has sleep; a skip here would hide that these
		// tests never ran.
		t.Fatalf("no sleep binary: %v", err)
	}
	return p
}

// codexInstall lays out an npm Codex install and returns its vendor binary.
func codexInstall(t *testing.T, vendorName, vendorVersion string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "lib", "node_modules", "@openai", "codex")
	vendor := filepath.Join(root, "node_modules", "@openai", "codex-linux-x64")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@openai/codex","version":"0.166.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "package.json"), []byte(`{"name":"`+vendorName+`","version":"`+vendorVersion+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendor, "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
	copyExecutable(t, sleepBinary(t), bin)
	return bin
}

// shimByPath is a sleep copy named codex at a path listed as an installed
// solud (ownSoludExes), like ~/.solu/codex-inline-bin/codex → solud.
func shimByPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	installed := filepath.Join(dir, "solud")
	copyExecutable(t, sleepBinary(t), installed)
	link := filepath.Join(dir, "shim", "codex")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(installed, link); err != nil {
		t.Fatal(err)
	}
	prev := ownSoludExes
	ownSoludExes = func() []string { return append(prev(), installed) }
	t.Cleanup(func() { ownSoludExes = prev })
	return link
}

// shimByBuild is a copy of this test binary named codex, at a path no list
// knows: found by its Go build (module and main package), like a parked
// solud.old.2 the update ring has since deleted.
func shimByBuild(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "old", "codex")
	copyExecutable(t, self, bin)
	return bin
}

// codexPane starts a pane root (sh) running each binary as a child named
// codex, and waits until all are in its process tree.
func codexPane(t *testing.T, agent string, bins ...string) *Pane {
	t.Helper()
	script := ""
	for i, b := range bins {
		arg := `"$` + string(rune('1'+i)) + `"`
		if filepath.Base(filepath.Dir(b)) == "old" {
			// The test-binary shim takes test flags, not a duration, and
			// sleeps in TestAgentExeVersionSleeper.
			script += arg + ` -test.run=^TestAgentExeVersionSleeper$ & `
		} else {
			script += arg + ` 60 & `
		}
	}
	script += "wait"
	cmd := exec.Command("sh", append([]string{"-c", script, "sh"}, bins...)...)
	cmd.Env = append(os.Environ(), "SOLU_AGENT_EXE_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, pid := range descendantPids(cmd.Process.Pid) {
			if pid != cmd.Process.Pid {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Kill()
				}
			}
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	p := testPaneWithTail(t, "codex-pane", "codex", "")
	p.Pid = cmd.Process.Pid
	p.Agent = agent
	waitForCodexChildren(t, p.Pid, len(bins))
	return p
}

func waitForCodexChildren(t *testing.T, root, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		count := 0
		for _, pid := range descendantPids(root) {
			if pid != root && readComm(pid) == agentCodex {
				count++
			}
		}
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d codex processes appeared under %d", count, n, root)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunningAgentVersionCodexOnLinux(t *testing.T) {
	t.Run("real layout: both shims and the vendor binary", func(t *testing.T) {
		p := codexPane(t, agentCodex, shimByPath(t), shimByBuild(t), codexInstall(t, "@openai/codex-linux-x64", "0.166.0-linux-x64"))
		if got := runningAgentVersion(p, agentCodex); got != "0.166.0" {
			t.Fatalf("version %q, want 0.166.0", got)
		}
		// ST-18 (#945): the recorded Codex band is selected through the real
		// helper. ST-11 (#947) compares the same value, "0.166.0", to pick
		// its recorded Codex keys.
		if b := p.promptEchoBandFor(true); b == nil || b.agent != agentCodex || b.version != "0.166.0" {
			t.Fatalf("band %+v, want codex 0.166.0", b)
		}
	})
	t.Run("shim only", func(t *testing.T) {
		p := codexPane(t, agentCodex, shimByPath(t))
		if got := runningAgentVersion(p, agentCodex); got != "" {
			t.Fatalf("version %q from the shim alone", got)
		}
	})
	t.Run("lookalike package name", func(t *testing.T) {
		p := codexPane(t, agentCodex, codexInstall(t, "@openai/codex-anything", "0.166.0"))
		if got := runningAgentVersion(p, agentCodex); got != "" {
			t.Fatalf("version %q from a lookalike package", got)
		}
	})
	t.Run("an unverified codex beside a real one", func(t *testing.T) {
		p := codexPane(t, agentCodex, codexInstall(t, "@openai/codex-linux-x64", "0.166.0"), codexInstall(t, "@openai/codex-anything", "0.166.0"))
		if got := runningAgentVersion(p, agentCodex); got != "" {
			t.Fatalf("version %q with an unverified codex in the pane", got)
		}
	})
	t.Run("deleted exe", func(t *testing.T) {
		bin := codexInstall(t, "@openai/codex-linux-x64", "0.166.0")
		p := codexPane(t, agentCodex, bin)
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		if got := runningAgentVersion(p, agentCodex); got != "" {
			t.Fatalf("version %q from a deleted binary", got)
		}
	})
	t.Run("a codex outside the pane's tree is ignored", func(t *testing.T) {
		outside := exec.Command(codexInstall(t, "@openai/codex-anything", "0.166.0"), "60")
		if err := outside.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = outside.Process.Kill(); _ = outside.Wait() })
		p := codexPane(t, agentCodex, codexInstall(t, "@openai/codex-linux-x64", "0.166.0"))
		if got := runningAgentVersion(p, agentCodex); got != "0.166.0" {
			t.Fatalf("version %q", got)
		}
	})
	t.Run("a pane recorded as claude is never codex", func(t *testing.T) {
		bin := codexInstall(t, "@openai/codex-linux-x64", "0.166.0")
		for _, agent := range []string{agentClaude, ""} {
			p := codexPane(t, agent, bin)
			if agent == "" {
				p.ClaudeSessionID = "session"
			}
			if got := runningAgentVersion(p, agentCodex); got != "" {
				t.Fatalf("agent %q: version %q", agent, got)
			}
		}
	})
	t.Run("exe owned by another user", func(t *testing.T) {
		prev := agentExeOwnedByDaemon
		agentExeOwnedByDaemon = func(string) bool { return false }
		t.Cleanup(func() { agentExeOwnedByDaemon = prev })
		p := codexPane(t, agentCodex, codexInstall(t, "@openai/codex-linux-x64", "0.166.0"))
		if got := runningAgentVersion(p, agentCodex); got != "" {
			t.Fatalf("version %q from another user's binary", got)
		}
	})
}

// A process keeps its pid and start time across exec, and an npm update
// deletes the binary it runs: the cached version is reused only while the
// process still runs the same file.
func TestAgentProcessVersionCacheFollowsTheExecutable(t *testing.T) {
	a := codexInstall(t, "@openai/codex-linux-x64", "0.166.0")
	b := codexInstall(t, "@openai/codex-linux-x64", "0.167.0-linux-x64")
	const fakePid = 1<<22 + 12345
	exe, id := a, exeFileID{dev: 1, ino: 1}
	prevID, prevStart := procExeIdentity, mcpProcessStartTime
	procExeIdentity = func(pid int) (string, exeFileID, error) {
		if pid != fakePid {
			return prevID(pid)
		}
		return exe, id, nil
	}
	mcpProcessStartTime = func(pid int) (uint64, bool) {
		if pid == fakePid {
			return 42, true
		}
		return prevStart(pid)
	}
	t.Cleanup(func() { procExeIdentity, mcpProcessStartTime = prevID, prevStart })
	check := func(step, want string) {
		t.Helper()
		if got, ours := agentProcessVersion(agentCodex, fakePid); got != want || ours {
			t.Fatalf("%s: version %q ours %t, want %q", step, got, ours, want)
		}
	}
	check("first read", "0.166.0")
	check("cached", "0.166.0")
	exe, id = b, exeFileID{dev: 1, ino: 2}
	check("after exec into another binary", "0.167.0")
	exe = b + " (deleted)"
	check("after the binary was deleted", "")
	vendorPkg := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(b)))), "package.json")
	if err := os.WriteFile(vendorPkg, []byte(`{"name":"@openai/codex-linux-x64","version":"0.168.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, id = b, exeFileID{dev: 1, ino: 3}
	check("after the same path was replaced", "0.168.0")
}
