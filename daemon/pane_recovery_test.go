package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const recoveryTestThread = "019f0000-0000-7000-8000-000000000987"

func TestPaneRecoveryRestoreShellWithVerifiedCodexIdentity(t *testing.T) {
	req := createPaneReq{Cwd: "/work", Cmd: []string{"/bin/bash"}, Agent: agentCodex, CodexThreadID: recoveryTestThread, Restore: true}
	if err := preparePaneRestore(&req); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Cmd, []string{"codex", "resume", recoveryTestThread}) {
		t.Fatalf("restore recipe = %v", req.Cmd)
	}
}

func TestPaneRecoveryRefusesFreshOrAmbiguousConversations(t *testing.T) {
	for _, req := range []createPaneReq{
		{Cmd: []string{"bash"}, Agent: agentCodex},
		{Cmd: []string{"codex"}},
		{Cmd: []string{"claude"}},
		{Cmd: []string{"bash"}, Agent: agentCodex, CodexThreadID: recoveryTestThread, IdentityBlocked: true},
	} {
		if err := preparePaneRestore(&req); err == nil {
			t.Fatalf("unsafe restore accepted: %+v", req)
		}
	}
}

func TestPaneRecoveryRecipeKeepsProviderIdentityAndBlockedState(t *testing.T) {
	snap := PaneSnapshot{ID: "recovery", Cmd: []string{"bash"}, Agent: agentCodex, CodexThreadID: recoveryTestThread, ClaudeSessionID: "stale-claude"}
	saved := persistedFromSnapshot(snap)
	if !reflect.DeepEqual(saved.Cmd, []string{"codex", "resume", recoveryTestThread}) {
		t.Fatalf("recipe = %v", saved.Cmd)
	}
	snap.IdentityBlocked = true
	saved = persistedFromSnapshot(snap)
	if !saved.IdentityBlocked || !reflect.DeepEqual(saved.Cmd, snap.Cmd) {
		t.Fatalf("blocked recipe lost safety latch: %+v", saved)
	}
}

func TestPaneRecoveryFailedResumeSurvivesSecondRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := switchTestPane("recovery-failure", "", []string{"codex"}, "")
	old.Agent, old.CodexThreadID, old.Status = agentCodex, recoveryTestThread, PaneExited
	old.Flagged, old.FlagMemo = true, "keep this conversation"
	api, _, _ := resumeTestServer(t, old)
	api.panes = store
	api.spawnPane = func(SpawnOpts) (*Pane, error) {
		return nil, errors.New("working directory does not exist: /tmp/deleted")
	}
	if rec := postResume(api, old.ID); rec.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	for restart := 1; restart <= 2; restart++ {
		reloaded, err := NewPaneStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		records := reloaded.List()
		if len(records) != 1 {
			t.Fatalf("restart %d lost pane: %+v", restart, records)
		}
		mgr := NewManager(dir)
		p := mgr.SuspendExisting(&records[0])
		if p == nil {
			t.Fatal("missing recovery placeholder")
		}
		snap := p.Snapshot()
		if snap.CodexThreadID != recoveryTestThread || snap.RecoveryError == "" || !snap.Resumable || !snap.Flagged || snap.FlagMemo != old.FlagMemo {
			t.Fatalf("restart %d lost recovery state: %+v", restart, snap)
		}
		if err := reloaded.Put(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPaneRecoveryMissingIdentityPersistsWithoutStartingFresh(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	info := PersistedPane{ID: "unidentified", Cmd: []string{"bash"}, Agent: agentCodex, RecoveryError: "conversation ID missing", IdentityBlocked: true}
	p := NewManager(dir).SuspendExisting(&info)
	if p == nil {
		t.Fatal("unidentified pane was discarded")
	}
	if err := store.Put(p); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored := NewManager(dir).SuspendExisting(&reloaded.List()[0])
	if got := restored.Snapshot(); !got.IdentityBlocked || got.RecoveryError == "" || got.CodexThreadID != "" {
		t.Fatalf("unsafe recovery: %+v", got)
	}
}

func TestPaneRecoveryExplicitRetryOpensPickerAndClearsStaleIdentity(t *testing.T) {
	old := switchTestPane("picker", "", []string{"bash"}, "stale-claude")
	old.Agent, old.Status, old.RecoveryError = agentCodex, PaneExited, "conversation is unverified"
	old.CodexThreadID = recoveryTestThread
	old.codexIdentityBlocked.Store(true)
	api, opts, _ := resumeTestServer(t, old)
	if rec := postResume(api, old.ID); rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	got := (*opts)[0]
	if !reflect.DeepEqual(got.Cmd, []string{"codex", "--no-alt-screen", "resume", "--all"}) || got.CodexThreadID != "" || got.ClaudeSessionID != "" {
		t.Fatalf("picker retained stale identity: %+v", got)
	}
}

func TestPaneRecoveryRetryCanChooseExistingDirectory(t *testing.T) {
	old := switchTestPane("new-cwd", "", []string{"codex"}, "")
	old.Agent, old.CodexThreadID, old.Status, old.RecoveryError = agentCodex, recoveryTestThread, PaneExited, "directory deleted"
	api, opts, _ := resumeTestServer(t, old)
	req := httptest.NewRequest(http.MethodPost, "/api/panes/new-cwd/resume", strings.NewReader(`{"cwd":"/work/recovered","cols":100,"rows":35}`))
	req.SetPathValue("id", old.ID)
	rec := httptest.NewRecorder()
	api.handleResumePane(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if got := (*opts)[0]; got.Cwd != "/work/recovered" || got.CodexThreadID != recoveryTestThread || !got.RequireResumableConversation {
		t.Fatalf("changed conversation while changing folder: %+v", got)
	}
}

func TestPaneRecoveryShellClassificationDoesNotReplayAgentScripts(t *testing.T) {
	for _, cmd := range [][]string{{"bash"}, {"/bin/bash", "-i"}, {"pwsh.exe"}} {
		if !plainShellRecovery(PersistedPane{Cmd: cmd}) {
			t.Fatalf("shell rejected: %v", cmd)
		}
	}
	for _, info := range []PersistedPane{{Cmd: []string{"bash", "-lc", "codex"}}, {Cmd: []string{"bash"}, Agent: agentCodex}, {Cmd: []string{"codex"}}} {
		if plainShellRecovery(info) {
			t.Fatalf("agent treated as plain shell: %+v", info)
		}
	}
}

func TestPaneRecoverySweepRetriesExitedIdentityPersistence(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := switchTestPane("write-retry", "", []string{"bash"}, "")
	p.Status = PaneExited
	mgr.panes[p.ID] = p
	calls := 0
	mgr.onPromptsCommit = func(got *Pane) {
		if got != p {
			t.Fatal("wrong pane")
		}
		calls++
	}
	sweepCodexIdentities(mgr)
	if calls != 1 {
		t.Fatalf("exited pane persistence was never retried: %d", calls)
	}
}
