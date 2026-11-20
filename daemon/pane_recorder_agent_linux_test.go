//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A Claude pane is stored with Agent "" (healPaneIdentity's legacy zero
// value), not "claude". The recorder must still read it as a Claude pane, or
// (f)3 refuses every Claude pane as "not an agent pane". A plain shell where
// someone typed claude stays refused.
func TestRecordingFactsReadAHealedClaudePaneAsClaude(t *testing.T) {
	claudeOpts, _ := healSpawnOpts(SpawnOpts{Agent: agentClaude, Cmd: []string{"claude", "--resume", "5cd5f757-46de-43e1-abb8-363ab9ef5079"}})
	if claudeOpts.Agent != "" {
		t.Fatalf("healed Claude pane agent %q; this test pins the legacy zero value", claudeOpts.Agent)
	}
	shellOpts, _ := healSpawnOpts(SpawnOpts{Cmd: []string{"bash"}})
	codexOpts, _ := healSpawnOpts(SpawnOpts{Agent: agentCodex, Cmd: []string{"codex"}})

	// A process tree like a real pane's: the pane's process with a running
	// child named claude (the shell keeps it a child: it is not the last
	// command, so sh does not exec it).
	dir := t.TempDir()
	claudeBin := filepath.Join(dir, "claude")
	raw, err := os.ReadFile(standInSleeper(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeBin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	tree := exec.Command("sh", "-c", `"$0" 60; :`, claudeBin)
	tree.Dir = dir
	applyDetached(tree) // its own process group, so the cleanup kills both
	if err := tree.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killChildTree(tree.Process)
		_ = tree.Wait()
	})

	facts := func(o SpawnOpts) recordingPaneFacts {
		p := &Pane{ID: "pane_rec_agent", Status: PaneRunning, Agent: o.Agent, Cmd: o.Cmd, Cwd: dir, Pid: tree.Process.Pid}
		return liveRecordingFacts(p)
	}
	dataDir := t.TempDir()
	var claude recordingPaneFacts
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if claude = facts(claudeOpts); claude.AgentRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claude child was never seen: %+v", claude)
		}
	}
	if claude.Agent != agentClaude {
		t.Fatalf("healed Claude pane read as agent %q", claude.Agent)
	}
	if reason := recordingRefusal(claude, "", dataDir); reason != "" {
		t.Fatalf("a running Claude pane was refused: %s", reason)
	}

	shell := facts(shellOpts)
	if shell.Agent != "" || shell.AgentRunning {
		t.Fatalf("plain shell read as %+v", shell)
	}
	if reason := recordingRefusal(shell, "", dataDir); reason != recordingStopShell {
		t.Fatalf("a shell with claude typed in it: refusal %q, want %q", reason, recordingStopShell)
	}

	// A Codex pane is stored as "codex"; its claude child is not its agent.
	codex := facts(codexOpts)
	if codex.Agent != agentCodex || codex.AgentRunning {
		t.Fatalf("codex pane read as %+v", codex)
	}
}
