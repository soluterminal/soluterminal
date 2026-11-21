//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// C1 and C2 (amendment 2026-11-20 section 3, Addendum B 4) on real processes:
// fixture programs named claude, helper and node, never a real agent.

func recFixtureExecutable(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// eventually re-reads the pane until want holds; the process tree is cached
// for up to a second.
func eventually(t *testing.T, p *Pane, want func(recordingPaneFacts) bool) recordingPaneFacts {
	t.Helper()
	var f recordingPaneFacts
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if f = liveRecordingFacts(p); want(f) {
			return f
		}
	}
	t.Fatalf("facts never matched: %+v", f)
	return f
}

// startFixtureShell starts the tree a pane has in production: a helper
// process (standing in for `solud agent`, the pane's Pid) in its own folder,
// whose direct child is the pane's shell, running script in dir.
func startFixtureShell(t *testing.T, dir string, script string, args ...string) *exec.Cmd {
	t.Helper()
	return startFixturePane(t, t.TempDir(), dir, script, args...)
}

func startFixturePane(t *testing.T, helperDir, dir string, script string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", `( cd "$PANE_DIR" && exec /bin/sh -c "$PANE_SCRIPT" sh "$@" ) & wait`, "helper"}, args...)...)
	cmd.Dir = helperDir
	cmd.Env = append(os.Environ(), "PANE_DIR="+dir, "PANE_SCRIPT="+script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = killChildTree(cmd.Process); _ = cmd.Wait() })
	return cmd
}

func TestRecordingFactsReadTheShellAndItsAgentOnly(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	elsewhere := filepath.Join(dir, "config-like")
	for _, d := range []string{work, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(dir, "claude")
	helper := filepath.Join(dir, "helper")
	recFixtureExecutable(t, sleep, claude)
	recFixtureExecutable(t, sleep, helper)
	// The pane's shell runs a fixture claude in its own folder and a helper
	// child in another folder.
	sh := startFixtureShell(t, work, `"$1" 60 & (cd "$2" && exec "$3" 60) & wait`, claude, elsewhere, helper)
	p := &Pane{ID: "pane_c1", Pid: sh.Process.Pid, Status: PaneRunning}

	f := eventually(t, p, func(f recordingPaneFacts) bool { return f.AgentRunning && len(f.Cwds) == 2 })
	if f.CwdUnknown || slices.Contains(f.Cwds, elsewhere) || f.Cwds[0] != work || f.Cwds[1] != work {
		t.Fatalf("C2: read beyond the shell and agent: %+v", f)
	}
	// A descendant in a refused folder doesn't refuse the pane.
	if v := recordingCheck(f, dir, filepath.Join(dir, "data")); v.Hard != "" || len(v.Defaults) != 0 {
		t.Fatalf("verdict %+v", v)
	}

	// The agent exits: a plain shell again.
	killed := false
	for _, pid := range scanProcessTree().descendantPids(sh.Process.Pid) {
		if readComm(pid) == "claude" {
			killed = syscall.Kill(pid, syscall.SIGKILL) == nil
		}
	}
	if !killed {
		t.Fatal("fixture claude not found")
	}
	f = eventually(t, p, func(f recordingPaneFacts) bool { return !f.AgentRunning })
	if v := recordingCheck(f, dir, filepath.Join(dir, "data")); !slices.Contains(v.Defaults, recordingRuleShell) {
		t.Fatalf("after exit %+v", v)
	}
}

// Codex installed through npm runs as node with argv[1] .../codex.
func TestRecordingFactsSeeCodexUnderNode(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	recFixtureExecutable(t, "/bin/sh", node)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "server.js"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("sleep 60\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	other := startFixtureShell(t, dir, `"$1" "$2" & wait`, node, filepath.Join(bin, "server.js"))
	codex := startFixtureShell(t, dir, `"$1" "$2" & wait`, node, filepath.Join(bin, "codex"))
	eventually(t, &Pane{ID: "pane_codex", Pid: codex.Process.Pid}, func(f recordingPaneFacts) bool { return f.AgentRunning })
	time.Sleep(1100 * time.Millisecond) // past the tree cache
	if f := liveRecordingFacts(&Pane{ID: "pane_node", Pid: other.Process.Pid}); f.AgentRunning {
		t.Fatalf("node running another script counts as an agent: %+v", f)
	}
}

// A pane whose own process is gone, or has no pid, has no readable folder:
// a hard refusal.
func TestRecordingFactsWithoutAProcessAreUnknown(t *testing.T) {
	if f := liveRecordingFacts(&Pane{ID: "pane_nopid"}); !f.CwdUnknown {
		t.Fatalf("%+v", f)
	}
	if f := liveRecordingFacts(&Pane{ID: "pane_gone", Pid: 1 << 30}); !f.CwdUnknown {
		t.Fatalf("%+v", f)
	}
}

// Two agents in one pane: the folders of both are read (C2 reads every
// claude or codex process, and nothing else).
func TestRecordingFactsReadEveryAgentsFolder(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	keys := filepath.Join(dir, "keys")
	for _, d := range []string{work, keys} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(dir, "claude")
	recFixtureExecutable(t, sleep, claude)
	sh := startFixtureShell(t, work, `"$1" 60 & (cd "$2" && exec "$1" 60) & wait`, claude, keys)
	p := &Pane{ID: "pane_two_agents", Pid: sh.Process.Pid, Status: PaneRunning}
	f := eventually(t, p, func(f recordingPaneFacts) bool { return len(f.Cwds) == 3 })
	if !f.AgentRunning || !slices.Contains(f.Cwds, keys) {
		t.Fatalf("second agent's folder not read: %+v", f)
	}
}

// The pane's Pid is its helper; the helper's own folder is never read, the
// shell's (its direct child) always is: a shell in a hard folder is refused
// whatever the helper's folder, and a helper in one refuses nothing.
func TestRecordingFactsReadTheShellNotTheHelper(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	work := filepath.Join(home, "work")
	for _, d := range []string{ssh, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(home, "data")
	inSSH := startFixturePane(t, work, ssh, `sleep 60 & wait`)
	f := eventually(t, &Pane{ID: "pane_shell_ssh", Pid: inSSH.Process.Pid}, func(f recordingPaneFacts) bool { return len(f.Cwds) > 0 })
	if v := recordingCheck(f, home, data); v.Hard != recordingRuleHardFolder {
		t.Fatalf("a shell in ~/.ssh: %+v (%+v)", v, f)
	}
	helperInSSH := startFixturePane(t, ssh, work, `sleep 60 & wait`)
	f = eventually(t, &Pane{ID: "pane_helper_ssh", Pid: helperInSSH.Process.Pid}, func(f recordingPaneFacts) bool { return len(f.Cwds) > 0 })
	if slices.Contains(f.Cwds, ssh) {
		t.Fatalf("the helper's folder was read: %+v", f)
	}
	if v := recordingCheck(f, home, data); v.Hard != "" {
		t.Fatalf("a helper started in ~/.ssh refused the pane: %+v", v)
	}
}
