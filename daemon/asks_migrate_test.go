package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// At boot, agents' and the worker manager's flags move onto the asks list;
// the person's flags stay as bookmarks; a Warden flag is cleared.
func TestMigrateFlagsToAsks(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	for _, id := range []string{"mine", "old", "bm", "historical", "wm", "wd", "ghost"} {
		p := testPaneWithTail(t, id, id, "")
		p.done = make(chan struct{})
		close(p.done)
		a.mgr.panes[id] = p
	}
	set := func(id, memo, owner string) { a.mgr.panes[id].SetFlagAs(true, memo, owner) }
	set("mine", "Waiting on Benjamin: approve the notes", "agent:mine")
	set("old", "", "agent")
	set("ghost", "decide A or B", "agent:closed-pane")
	set("social", "check the release", "agent:lead") // the caller is another, open pane
	set("bm", "my bookmark", "user")
	set("historical", "older bookmark", "")
	a.mgr.panes["wd"].SetFlagAs(true, "NEEDS: x", "")
	a.mgr.panes["wd"].SetFlagAs(true, "NEEDS: x", "warden")
	set("wm", "Worker worker retained: final reply unread by lead", "worker-manager")

	a.migrateFlagsToAsks()
	a.migrateFlagsToAsks() // idempotent

	text := func(pane string) string {
		var out []string
		for _, x := range a.asks.openFor(pane) {
			out = append(out, x.Text)
		}
		return strings.Join(out, " | ")
	}
	if got := text("mine"); got != "Waiting on Benjamin: approve the notes" {
		t.Fatalf("own agent flag: %q", got)
	}
	if got := text("old"); got != "Come back to this pane" {
		t.Fatalf("legacy agent flag: %q", got)
	}
	if got := text("ghost"); got != "decide A or B" {
		t.Fatalf("flag whose caller is gone: %q", got)
	}
	if got := text("lead"); !strings.Contains(got, "On #3 Social loop") || !strings.Contains(got, "check the release") {
		t.Fatalf("flag set by another pane: %q", got)
	}
	if got := text("wm"); !strings.HasSuffix(got, "kept open: final reply unread by lead") {
		t.Fatalf("worker-manager flag: %q", got)
	}
	for _, id := range []string{"mine", "old", "ghost", "social", "wd", "wm"} {
		if a.mgr.panes[id].Snapshot().Flagged {
			t.Errorf("%s still flagged", id)
		}
	}
	if s := a.mgr.panes["bm"].Snapshot(); !s.Flagged || s.FlagMemo != "my bookmark" || s.FlagOwner != "user" {
		t.Fatalf("the person's bookmark changed: %+v", s)
	}
	if snap := a.mgr.panes["historical"].Snapshot(); !snap.Flagged || snap.FlagMemo != "older bookmark" || snap.FlagOwner != "" {
		t.Fatalf("historical bookmark changed: %+v", snap)
	}
	if n := len(a.asks.list()); n != 5 {
		t.Fatalf("asks = %d, want 5 (no duplicates on a second run)", n)
	}
}

// An agent can't see, close, overwrite or be crowded out by the worker
// manager's asks on its own pane.
func TestWorkerAsksAreSeparateFromTheAgents(t *testing.T) {
	s, _ := newTestAskStore(t)
	for i := 0; i < askOpenPerPane+2; i++ {
		if _, _, err := s.post(askPost{Pane: "lead", Kind: askKindWorker, Key: workerAskKey(string(rune('a' + i))), Text: "kept open", WorkerPane: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	// Seven worker notices leave the agent its whole budget.
	for i := 0; i < askOpenPerPane; i++ {
		mustPost(t, s, "lead", "mine-"+string(rune('a'+i)), "decide "+string(rune('a'+i)))
	}
	if err := s.resolve("lead", workerAskKey("a")); err != errAskNotFound {
		t.Fatalf("agent resolved a worker notice: %v", err)
	}
	s2, _ := newTestAskStore(t)
	if _, _, err := s2.post(askPost{Pane: "lead", Kind: askKindWorker, Key: "worker-x", Text: "Worker x kept open: unpushed work", WorkerPane: "x"}); err != nil {
		t.Fatal(err)
	}
	mustPost(t, s2, "lead", "worker-x", "nothing to see here")
	notices := s2.workerAsks("lead")
	if len(notices) != 1 || notices[0].Text != "Worker x kept open: unpushed work" {
		t.Fatalf("agent overwrote the worker notice: %+v", notices)
	}
	if mine := s2.openAgentAsks("lead"); len(mine) != 1 || mine[0].Kind != askKindAgent {
		t.Fatalf("agent's own asks = %+v", mine)
	}
}

// A flag is cleared only once its replacement ask is saved; a failed save
// leaves it for the next start.
func TestMigrateFlagsKeepsTheFlagWhenTheAskIsNotSaved(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	good := a.asks.path
	a.asks.path = filepath.Join(t.TempDir(), "missing", "dir", "asks.json")
	a.mgr.panes["social"].SetFlagAs(true, "approve the notes", "agent:social")
	a.mgr.panes["lead"].SetFlagAs(true, "Worker worker retained: unpushed work", "worker-manager")
	a.migrateFlagsToAsks()
	for _, id := range []string{"social", "lead"} {
		if !a.mgr.panes[id].Snapshot().Flagged {
			t.Fatalf("%s lost its flag although the ask was not saved", id)
		}
	}
	a.asks.path = good
	a.migrateFlagsToAsks()
	for _, id := range []string{"social", "lead"} {
		if a.mgr.panes[id].Snapshot().Flagged {
			t.Fatalf("%s still flagged after a successful retry", id)
		}
	}
	if len(a.asks.openAgentAsks("social")) != 1 || len(a.asks.workerAsks("lead")) != 1 {
		t.Fatalf("asks after retry: %+v", a.asks.list())
	}
}

// A worker notice whose ask could not be saved is retried by the next
// reconcile, also after a restart, instead of being lost.
func TestWorkerNoticeRetriesAFailedSave(t *testing.T) {
	a, p, _ := archiveFixture(t)
	good := a.asks.path
	a.asks.path = filepath.Join(t.TempDir(), "missing", "dir", "asks.json")
	a.flagWorkerLead("lead", p.ID, "unpushed work")
	if leadNoticed(a, "lead") {
		t.Fatal("notice saved to an unwritable store")
	}
	if note, ok := a.retainedWorker(p.ID); !ok || !note.Pending {
		t.Fatalf("retention = %+v %v, want pending", note, ok)
	}
	a.reconcileWorkerRetentions(true) // still failing: still pending
	if note, _ := a.retainedWorker(p.ID); !note.Pending {
		t.Fatal("pending mark lost on a failed retry")
	}
	a.asks.path = good
	// A restart reloads the record from disk with its pending mark.
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	a.reconcileWorkerRetentions(true)
	if !strings.Contains(leadNoticeText(a, "lead"), "kept open: unpushed work") {
		t.Fatalf("notice not retried: %q", leadNoticeText(a, "lead"))
	}
	if note, _ := a.retainedWorker(p.ID); note.Pending {
		t.Fatal("still pending after a successful retry")
	}
}

// A failed update for one worker must not turn another worker's saved ask
// into a pending retry that brings it back after the person dismisses it.
func TestWorkerNoticeDismissalSurvivesAnotherWorkersSaveFailure(t *testing.T) {
	a, p, _ := archiveFixture(t)
	other := testPaneWithTail(t, "other-worker", "Other worker", "")
	other.Origin = p.Snapshot().Origin.clone()
	a.mgr.panes[other.ID] = other
	a.flagWorkerLead("lead", p.ID, "unpushed work")
	a.flagWorkerLead("lead", other.ID, "unread reply")

	good := a.asks.path
	a.asks.path = filepath.Join(t.TempDir(), "missing", "dir", "asks.json")
	a.flagWorkerLead("lead", other.ID, "changed retention reason") // fails to save
	a.flagWorkerLead("lead", p.ID, "unpushed work")                // already saved, unchanged
	a.reconcileWorkerRetentions(true)                              // other worker still fails
	if note, _ := a.retainedWorker(other.ID); !note.Pending {
		t.Fatal("lost the genuinely unsaved worker notice")
	}
	a.asks.path = good
	dismissed := false
	for _, ask := range a.asks.workerAsks("lead") {
		if ask.WorkerPane == p.ID {
			if err := a.asks.dismiss(ask.ID); err != nil {
				t.Fatal(err)
			}
			dismissed = true
		}
	}
	if !dismissed {
		t.Fatal("the saved notice was not available to dismiss")
	}

	// Reload both stores so an in-memory correction alone cannot pass.
	a.asks = newAskStore(filepath.Dir(good), nil)
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	for i := 0; i < 2; i++ {
		a.reconcileWorkerRetentions(true)
		for _, ask := range a.asks.workerAsks("lead") {
			if ask.WorkerPane == p.ID {
				t.Fatalf("dismissed notice came back after another worker's save failed: %+v", ask)
			}
		}
	}
	if note, _ := a.retainedWorker(p.ID); note.Pending {
		t.Fatal("the saved, dismissed notice is still pending")
	}
	if note, _ := a.retainedWorker(other.ID); note.Pending {
		t.Fatal("the unsaved worker notice was not retried after recovery")
	}

	// A new retention event may ask again; ordinary reconciliation may not.
	a.flagWorkerLead("lead", p.ID, "new uncommitted work")
	for _, ask := range a.asks.workerAsks("lead") {
		if ask.WorkerPane == p.ID {
			return
		}
	}
	t.Fatal("a new retention event did not create a new notice")
}

// A refresh based on an earlier snapshot may change an open notice, but may
// neither reopen a dismissed one nor overwrite a newer retention event.
func TestWorkerAskRefreshRespectsDismissalAndReplacement(t *testing.T) {
	s, _ := newTestAskStore(t)
	in := askPost{Pane: "lead", Kind: askKindWorker, Key: "worker-x", Text: "unread reply", WorkerPane: "x"}
	first, _, err := s.post(in)
	if err != nil {
		t.Fatal(err)
	}
	in.UpdateID, in.Text = first.ID, "unpushed work"
	if updated, created, err := s.post(in); err != nil || created || updated.ID != first.ID || updated.Text != in.Text {
		t.Fatalf("refresh open notice: %+v created=%v err=%v", updated, created, err)
	}
	if err := s.dismiss(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(in); err != errAskNotFound {
		t.Fatalf("refresh dismissed notice: %v", err)
	}
	if asks := s.workerAsks("lead"); len(asks) != 0 {
		t.Fatalf("refresh reopened the dismissed notice: %+v", asks)
	}
	fresh := in
	fresh.UpdateID, fresh.Text = "", "new uncommitted work"
	second, _, err := s.post(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(in); err != errAskNotFound {
		t.Fatalf("stale refresh overwrote a replacement notice: %v", err)
	}
	if asks := s.workerAsks("lead"); len(asks) != 1 || asks[0].ID != second.ID || asks[0].Text != fresh.Text {
		t.Fatalf("replacement notice changed: %+v", asks)
	}
}
