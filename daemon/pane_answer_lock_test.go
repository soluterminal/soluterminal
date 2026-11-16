package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnswerLockGridDetectorAndAriaPaths(t *testing.T) {
	for _, path := range []string{"grid-direct", "aria-observation", "aria-send-input"} {
		t.Run(path, func(t *testing.T) {
			a, p, frames, _ := newDirectAutoRepeatHarness(t, "locked-"+path)
			assertDirectAutoPromptReady(t, p, "live permission menu")
			p.setAutomaticAnswerSettings("", true)
			switch path {
			case "grid-direct":
				a.directAutoFire(context.Background(), p.ID, p.Title)
			case "aria-observation":
				a.evaluateObservation(context.Background(), p.ID, p.Title, "")
			case "aria-send-input":
				result := a.toolSendInput(context.Background(), "locked-answer", p.ID, "1\n", 0, 0, false, func(map[string]any) {})
				if !strings.Contains(result, "locked") {
					t.Fatalf("aria result: %s", result)
				}
			}
			expectNoFrame(t, frames, "locked "+path)
		})
	}
}

func TestAnswerLockCrossPaneAnswerPromptUsesFinalGate(t *testing.T) {
	a, p, frames, _ := newDirectAutoRepeatHarness(t, "locked-cross-pane")
	p.setAutomaticAnswerSettings("", true)
	server := &apiServer{mgr: a.mgr, uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}
	// Real MCP dispatch in a unit fixture represents a delegated caller on a
	// DIFFERENT pane. It must not use the target's manual input privilege.
	params, err := json.Marshal(toolCallParams{Name: "answer_prompt", Arguments: map[string]any{"pane_id": p.ID, "option": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpResultText(t, server.mcpCallTool(params, nil, "other-pane"))
	if !isErr || !strings.Contains(text, "locked") {
		t.Fatalf("cross-pane answer bypassed final lock: %q %v", text, isErr)
	}
	expectNoFrame(t, frames, "cross-pane answer")
	text, isErr = mcpResultText(t, server.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "1"}, "other-pane"))
	if !isErr || !strings.Contains(text, "locked") {
		t.Fatalf("raw menu answer bypassed lock: %q %v", text, isErr)
	}
	expectNoFrame(t, frames, "cross-pane raw menu answer")
	// This exercises the shared writer with a freshly captured, otherwise valid
	// policy, so a lock cannot depend on mode/generation checks upstream.
	_, err = p.writeWithAutoModePolicy([]byte("\r"), false, p.autoModePolicy(), time.Now().Add(time.Second))
	if !errors.Is(err, errAutoModePolicyChanged) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("final writer: %v", err)
	}
	expectNoFrame(t, frames, "shared answer transport")
	// Actual human keystrokes remain usable.
	if _, err := p.WriteNoPromptRecord([]byte("3")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "human menu answer", "3")
}

func TestAnswerLockJevCautiousUsesFinalGate(t *testing.T) {
	a, p, frames := newJevFullHarness(t, jevNovelToolMenu)
	p.SetAutoMode("cautious")
	tail := ariaJevPermissionTail(p, true)
	_, pos, fp, ok := currentChoicePromptIdentity(p, tail)
	if !ok {
		t.Fatal("missing cautious prompt")
	}
	proof := &jevAmbiguousProof{dataDir: a.dataDir, mode: "cautious", full: true, classified: true, pos: pos, fingerprint: fp, tailDigest: sha256.Sum256([]byte(ariaJevMenuEvidence(tail, true)))}
	bindJevTestProof(t, a, p, proof)
	// Model a legacy restored policy with its mode still set. All evidence and
	// decision checks stay valid; the final answer transport must enforce the lock.
	p.mu.Lock()
	p.NeverAutoAnswer = true
	p.mu.Unlock()
	_, err := a.writeJevEnter(context.Background(), p, proof)
	if !errors.Is(err, errAutoModePolicyChanged) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("Jev final gate: %v", err)
	}
	expectNoFrame(t, frames, "locked Jev cautious Enter")
	p.mu.Lock()
	p.NeverAutoAnswer = false
	p.mu.Unlock()
	if _, err := a.writeJevEnter(context.Background(), p, proof); err != nil {
		t.Fatalf("fixture proof was not valid: %v", err)
	}
	expectFrame(t, frames, "unlocked valid Jev proof", "\r")
}

func TestAnswerLockMarkerStartsPaneLockedAndDeletionCannotClear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd, data := t.TempDir(), t.TempDir()
	marker := filepath.Join(cwd, paneNoAutoAnswerMarker)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(data)
	p, err := mgr.Spawn(SpawnOpts{Cwd: cwd, Cmd: []string{"/bin/sh"}, AutoMode: "always"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Kill(p.ID) })
	assertAutoSettings(t, p, true)
	store, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	info := reloaded.List()[0]
	info.RecoveryError = "fixture shell"
	restored := NewManager(t.TempDir()).SuspendExisting(&info)
	assertAutoSettings(t, restored, true)
	api, _, _ := resumeTestServer(t, restored)
	api.panes = reloaded
	server := autoSettingsServer(t, api)
	autoSettingsPost(t, server, "/api/panes/"+restored.ID+"/never-auto-answer", `{"neverAutoAnswer":false}`, 200)
	assertAutoSettings(t, restored, false)
	unlocked, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if unlocked.List()[0].NeverAutoAnswer {
		t.Fatal("human unlock did not persist")
	}
}

func TestAnswerLockMarkerTightensPersistedBootRecordDurably(t *testing.T) {
	cwd, data := t.TempDir(), t.TempDir()
	store, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	p := switchTestPane("marker-boot", "", []string{"bash"}, "")
	p.Cwd = cwd
	p.SetAutoMode("always")
	t.Cleanup(p.shutdownGrid)
	if err := store.Put(p); err != nil {
		t.Fatal(err)
	}
	// A marker introduced between daemon runs tightens a previously unlocked
	// recipe and is written back before boot recovery publishes any pane.
	marker := filepath.Join(cwd, paneNoAutoAnswerMarker)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	boot, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := boot.List()[0]; !got.NeverAutoAnswer || got.AutoMode != "" {
		t.Fatalf("marker ignored: %+v", got)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	again, err := NewPaneStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List()[0]; !got.NeverAutoAnswer || got.AutoMode != "" {
		t.Fatal("marker removal cleared the persisted lock")
	}
	// No marker and no persisted lock leaves ordinary panes unchanged.
	mode, locked := tightenPaneAnswerSettings(cwd, "always", false)
	if mode != "always" || locked {
		t.Fatal("unmarked pane was unexpectedly locked")
	}
}
