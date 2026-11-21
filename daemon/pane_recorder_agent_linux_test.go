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
// the shell default refuses every Claude pane. Since ST-317 (C1, amendment
// 2026-11-20 section 3.1) a live claude or codex process anywhere in a pane
// makes it an agent pane, so a shell where someone typed claude counts too.
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
	if v := recordingCheck(claude, "", dataDir); v.Hard != "" || len(v.Defaults) != 0 {
		t.Fatalf("a running Claude pane was refused: %+v", v)
	}

	// A shell with claude typed in it: its agent is still "" (what it was
	// started as), but the live claude child makes it an agent pane (C1).
	shell := facts(shellOpts)
	if shell.Agent != "" || !shell.AgentRunning {
		t.Fatalf("shell with a claude child read as %+v", shell)
	}
	if v := recordingCheck(shell, "", dataDir); len(v.Defaults) != 0 {
		t.Fatalf("a shell running claude was refused: %+v", v)
	}

	// A Codex pane stays "codex"; any live claude or codex counts (C1).
	codex := facts(codexOpts)
	if codex.Agent != agentCodex || !codex.AgentRunning {
		t.Fatalf("codex pane read as %+v", codex)
	}
}
