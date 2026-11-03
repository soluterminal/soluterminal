package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerNoticeIsQuietButReadableByTheLead(t *testing.T) {
	a, worker, _ := archiveFixture(t)
	a.asks.onCreated = a.queueAskPush
	a.flagWorkerLead("lead", worker.ID, "uncommitted work (new worktree: /repo/review)")
	notes := a.asks.workerAsks("lead")
	if len(notes) != 1 || askNeedsUser(notes[0]) || a.asks.openCounts()["lead"] != 0 {
		t.Fatalf("routine retention became a person ask: %+v", notes)
	}
	a.askPush.mu.Lock()
	queued, scheduled := len(a.askPush.pending), a.askPush.busy
	a.askPush.mu.Unlock()
	if queued != 0 || scheduled {
		t.Fatal("routine retention queued a push")
	}
	if _, _, pane := a.askPushText([]string{notes[0].ID}); pane != "" {
		t.Fatal("an old queued quiet notice could still produce a push")
	}
	text, failed := mcpResultText(t, a.mcpToolListPanes(nil, "lead"))
	var summaries []mcpPaneSummary
	if failed || json.Unmarshal([]byte(text), &summaries) != nil {
		t.Fatalf("list_panes: %s", text)
	}
	found := false
	for _, pane := range summaries {
		if pane.ID == "lead" {
			found = true
			if pane.OpenAsks != 0 || len(pane.WorkerNotices) != 1 || pane.WorkerNotices[0].ID != notes[0].ID || pane.WorkerNotices[0].WorkerPane != worker.ID {
				t.Fatalf("lead cannot read its quiet note: %+v", pane)
			}
		}
	}
	if !found {
		t.Fatal("lead missing from list_panes")
	}
	rec := httptest.NewRecorder()
	a.listPanes(rec, httptest.NewRequest("GET", "/api/panes", nil))
	var panes []struct {
		ID            string       `json:"id"`
		Asks          []paneAskDTO `json:"asks"`
		WorkerNotices []paneAskDTO `json:"workerNotices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &panes); err != nil {
		t.Fatal(err)
	}
	for _, pane := range panes {
		if pane.ID == "lead" && (len(pane.Asks) != 0 || len(pane.WorkerNotices) != 1) {
			t.Fatalf("pane DTO mixes lead notes with person asks: %+v", pane)
		}
	}
	rec = httptest.NewRecorder()
	a.listAsks(rec, httptest.NewRequest("GET", "/api/asks", nil))
	var personList struct {
		Asks []Ask `json:"asks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &personList); err != nil || len(personList.Asks) != 0 {
		t.Fatalf("person ask list contains housekeeping: %s (%v)", rec.Body.String(), err)
	}
}

func TestWorkerUnpublishedWorkEscalatesOnceAfterRestart(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "uncommitted work")
	id := a.asks.workerAsks("lead")[0].ID
	path := a.asks.path
	// Both the observation time and notice survive restart.
	a.asks = newAskStore(filepath.Dir(path), nil)
	a.asks.now = func() time.Time { return now }
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	a.asks.onCreated = a.queueAskPush
	// Hold the batch so the test controls delivery, without starting a timer.
	a.askPush.busy = true
	fired := 0
	a.askPush.fire = func(pane, title, summary string) bool {
		fired++
		if pane != "lead" || !strings.Contains(summary, "Decide how to preserve") {
			t.Errorf("escalation is not an actionable person ask: %q %q %q", pane, title, summary)
		}
		return true
	}
	now = now.Add(workerUnpublishedEscalation - time.Millisecond)
	a.observeWorkerPublication(worker.ID, "uncommitted work", now)
	a.reconcileWorkerRetentions(true)
	if a.asks.openCounts()["lead"] != 0 || len(a.askPush.pending) != 0 {
		t.Fatal("escalated before the observation threshold")
	}
	now = now.Add(time.Millisecond)
	a.observeWorkerPublication(worker.ID, "uncommitted work", now)
	a.reconcileWorkerRetentions(true)
	notes := a.asks.workerAsks("lead")
	if len(notes) != 1 || notes[0].ID != id || !askNeedsUser(notes[0]) || a.asks.openCounts()["lead"] != 1 || len(a.askPush.pending) != 1 {
		t.Fatalf("overdue unpublished work did not become one person ask: %+v", notes)
	}
	text, failed := mcpResultText(t, a.mcpToolListPanes(nil, "lead"))
	var summaries []mcpPaneSummary
	if failed || json.Unmarshal([]byte(text), &summaries) != nil {
		t.Fatalf("list_panes: %s", text)
	}
	for _, pane := range summaries {
		if pane.ID == "lead" && (pane.OpenAsks != 1 || len(pane.WorkerNotices) != 1 || !pane.WorkerNotices[0].NeedsUser) {
			t.Fatalf("lead lost the escalation context: %+v", pane)
		}
	}
	a.flushAskPush()
	a.reconcileWorkerRetentions(true)
	if fired != 1 || len(a.askPush.pending) != 0 {
		t.Fatal("unchanged escalation pushed more than once")
	}
	a.flagWorkerLead("lead", worker.ID, "final reply unread by lead")
	if a.asks.openCounts()["lead"] != 0 {
		t.Fatal("resolved publication risk still needs the person")
	}
}

func TestDismissedQuietWorkerNoticeDoesNotReturnWhenItAges(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "unpushed work")
	note := a.asks.workerAsks("lead")[0]
	if err := a.asks.dismiss(note.ID); err != nil {
		t.Fatal(err)
	}
	a.asks = newAskStore(filepath.Dir(a.asks.path), nil)
	now = now.Add(2 * workerUnpublishedEscalation)
	a.asks.now = func() time.Time { return now }
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	for i := 0; i < 2; i++ {
		a.observeWorkerPublication(worker.ID, "unpushed work", now)
		a.reconcileWorkerRetentions(true)
	}
	if len(a.asks.workerAsks("lead")) != 0 || a.asks.openCounts()["lead"] != 0 {
		t.Fatal("aging or restart reopened a dismissed notice")
	}
}

func TestAStoredWorkerReasonDoesNotEscalateWithoutFreshPublicationEvidence(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "uncommitted work; final reply unread by lead")
	path := a.asks.path
	now = now.Add(2 * workerUnpublishedEscalation)
	a.asks = newAskStore(filepath.Dir(path), nil)
	a.asks.now = func() time.Time { return now }
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	a.reconcileWorkerRetentions(true)
	if a.asks.openCounts()["lead"] != 0 {
		t.Fatal("a stale saved reason became a person ask without a fresh check")
	}
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now // old final reply, as after a daemon restart
	q.mu.Unlock()
	a.tryArchiveWorker(worker, now) // the fixture's real checkout is clean
	note, _ := a.retainedWorker(worker.ID)
	if note.Escalated || note.Since != 0 || note.Reason != "final reply unread by lead" || a.asks.openCounts()["lead"] != 0 {
		t.Fatalf("published work still needs the person: %+v", note)
	}
}

func TestWorkerRiskClockStartsWhenUnpublishedWorkAppears(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "final reply unread by lead")
	now = now.Add(2 * workerUnpublishedEscalation)
	a.flagWorkerLead("lead", worker.ID, "uncommitted work")
	if a.asks.openCounts()["lead"] != 0 {
		t.Fatal("old unread time was counted as unpublished risk")
	}
	note, _ := a.retainedWorker(worker.ID)
	if note.Since != now.UnixMilli() {
		t.Fatalf("risk observation started at %d", note.Since)
	}
}

func TestWorkerRetentionReasonsNeedPersonOnlyForProlongedUnpublishedWork(t *testing.T) {
	now := time.Now()
	for _, reason := range []string{
		"final reply unread by lead", "work has no pushed upstream", "worktree list cannot be verified",
		"worktree paths cannot be verified", "worktree index cannot be verified", "worktree status cannot be verified",
		"submodule work requires manual review", "archive layout persistence failed", "archive process shutdown failed",
		"archive pane-store cleanup failed", "unknown condition (new worktree: /unpushed work)",
	} {
		if workerRetentionNeedsUser(workerRetention{Reason: reason, Since: now.Add(-7 * 24 * time.Hour).UnixMilli()}, now) {
			t.Errorf("lead housekeeping escalated: %s", reason)
		}
	}
	for _, reason := range []string{"uncommitted work", "unpushed work; final reply unread by lead", "uncommitted work (new worktree: /repo/review)", "unpushed work (worker process cwd)"} {
		if !workerRetentionNeedsUser(workerRetention{Reason: reason, Since: now.Add(-workerUnpublishedEscalation).UnixMilli()}, now) {
			t.Errorf("prolonged unpublished work not escalated: %s", reason)
		}
		if workerRetentionNeedsUser(workerRetention{Reason: reason}, now) {
			t.Errorf("invented an age for legacy record: %s", reason)
		}
	}
}

func TestUnpublishedWorkerEscalatesAfterConfirmedLeadAbsence(t *testing.T) {
	for _, kind := range []string{"closed", "archived", "exited"} {
		t.Run(kind, func(t *testing.T) {
			a, worker, now := archiveFixture(t)
			a.asks.now = func() time.Time { return now }
			a.flagWorkerLead("lead", worker.ID, "unpushed work")
			id := a.asks.workerAsks("lead")[0].ID
			if kind == "exited" {
				a.mgr.panes["lead"].mu.Lock()
				a.mgr.panes["lead"].Status = PaneExited
				a.mgr.panes["lead"].mu.Unlock()
			} else {
				a.asksPaneClosed("lead")
				delete(a.mgr.panes, "lead")
			}
			// Startup must not discard a worker's notice with its old lead.
			a.asks.closeMissing(func(id string) bool { _, ok := a.mgr.Get(id); return ok })
			a.asks.onCreated = a.queueAskPush
			a.askPush.busy = true // control the batch without a timer
			a.observeWorkerPublication(worker.ID, "unpushed work", now)
			if len(a.asks.openCounts()) != 0 || len(a.askPush.pending) != 0 {
				t.Fatal("first absence observation created a person ask or push")
			}
			now = now.Add(5*time.Second - time.Millisecond)
			a.observeWorkerPublication(worker.ID, "unpushed work", now)
			if len(a.asks.openCounts()) != 0 || len(a.askPush.pending) != 0 {
				t.Fatal("absence escalated before its grace window")
			}
			now = now.Add(time.Millisecond)
			a.reconcileWorkerRetentions(true)
			if len(a.asks.openCounts()) != 0 {
				t.Fatal("aging alone escalated without a fresh worker observation")
			}
			a.observeWorkerPublication(worker.ID, "unpushed work", now)
			for i := 0; i < 2; i++ {
				a.reconcileWorkerRetentions(true)
			}
			notes := a.asks.workerAsks(worker.ID)
			if len(notes) != 1 || notes[0].ID != id || !notes[0].NeedsUser || !strings.Contains(notes[0].Text, "lead is no longer running") || a.asks.openCounts()[worker.ID] != 1 {
				t.Fatalf("orphaned work is not actionable on its worker: %+v", notes)
			}
			if len(a.askPush.pending) != 1 {
				t.Fatal("orphan escalation did not queue exactly one push")
			}
			a.clearWorkerRetention(worker.ID)
			if len(a.asks.workerAsks(worker.ID)) != 0 {
				t.Fatal("orphan ask survived cleared retention")
			}
		})
	}
}

func TestTransientLeadGapResetsItsAbsenceGrace(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "unpushed work")
	a.asks.onCreated = a.queueAskPush
	a.askPush.busy = true
	lead := a.mgr.panes["lead"]
	delete(a.mgr.panes, "lead")
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	// Reconciliation sees the lead return before another Git observation.
	a.mgr.panes["lead"] = lead
	a.reconcileWorkerRetentions(true)
	if note, _ := a.retainedWorker(worker.ID); note.LeadAbsentSince != 0 {
		t.Fatal("a returned lead retained its earlier absence clock")
	}
	now = now.Add(10 * workerLeadAbsenceGrace)
	delete(a.mgr.panes, "lead")
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	if len(a.asks.openCounts()) != 0 || len(a.askPush.pending) != 0 {
		t.Fatal("a later gap inherited the earlier absence's age")
	}
	now = now.Add(workerLeadAbsenceGrace)
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	if a.asks.openCounts()[worker.ID] != 1 || len(a.askPush.pending) != 1 {
		t.Fatal("genuine sustained absence did not escalate after its own grace")
	}
}

func TestAnExitedWorkerWithNoRunningLeadStillGetsAPreservationAsk(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	putCapabilityFile(t, filepath.Join(worker.Cwd, "unpublished.txt"), "work to preserve")
	a.flagWorkerLead("lead", worker.ID, "uncommitted work")
	a.mgr.panes["lead"].mu.Lock()
	a.mgr.panes["lead"].Status = PaneExited
	a.mgr.panes["lead"].mu.Unlock()
	worker.mu.Lock()
	worker.Status = PaneExited
	worker.mu.Unlock()
	a.tryArchiveWorker(worker, now)
	if a.asks.openCounts()[worker.ID] != 0 {
		t.Fatal("an exited worker escalated on its first lead-absence observation")
	}
	now = now.Add(workerLeadAbsenceGrace)
	a.tryArchiveWorker(worker, now)
	if a.asks.openCounts()[worker.ID] != 1 {
		t.Fatal("an exited worker's unpublished work was lost behind its exited lead")
	}
}

func suspendWorkerNoticeLead(t *testing.T, a *apiServer) *Pane {
	t.Helper()
	previous := a.mgr.panes["lead"].Snapshot()
	delete(a.mgr.panes, "lead")
	lead := a.mgr.SuspendExisting(&PersistedPane{ID: "lead", ClaudeSessionID: "suspended-lead", Origin: previous.Origin})
	if lead == nil || lead.Snapshot().Status != PaneExited || !lead.Snapshot().Resumable {
		t.Fatal("restart did not materialize an exited, resumable lead")
	}
	return lead
}

func TestWorkerNoticeStaysQuietThroughRealLeadResumeAndRestart(t *testing.T) {
	for _, action := range []string{"resume", "restart"} {
		t.Run(action, func(t *testing.T) {
			a, worker, now := archiveFixture(t)
			a.asks.now = func() time.Time { return now }
			putCapabilityFile(t, filepath.Join(worker.Cwd, "unpublished.txt"), "work to preserve")
			lead := switchTestPane("lead", "native", nil, "lead-session")
			lead.Agent, lead.Cwd = agentClaude, worker.Cwd
			lead.ClaudeTranscriptPath = writeTempTranscript(t)
			if action == "resume" {
				lead.Status, lead.Resumable = PaneExited, true
			}
			a.mgr.panes["lead"] = lead
			a.flagWorkerLead("lead", worker.ID, "uncommitted work")
			id := a.asks.workerAsks("lead")[0].ID
			worker.mu.Lock()
			worker.Status = PaneExited
			worker.mu.Unlock()
			created, pushed := 0, 0
			a.askPush = askPushBatch{busy: true, fire: func(_, _, _ string) bool { pushed++; return true }}
			a.asks.onCreated = func(ask Ask) {
				if askNeedsUser(ask) {
					created++
				}
				a.queueAskPush(ask)
			}
			assertQuiet := func(stage string) {
				t.Helper()
				a.tryArchiveWorker(worker, now) // fresh Git check inside the real respawn gap
				a.reconcileWorkerRetentions(true)
				notes := a.asks.workerAsks("lead")
				if created != 0 || len(a.asks.openCounts()) != 0 || len(a.askPush.pending) != 0 || len(notes) != 1 || notes[0].ID != id || notes[0].NeedsUser {
					t.Fatalf("%s created or moved a person ask/push: %+v", stage, notes)
				}
				if note, _ := a.retainedWorker(worker.ID); note.LeadAbsentSince != 0 {
					t.Fatalf("%s started an absence clock during respawn", stage)
				}
			}
			killed, spawned := 0, 0
			a.killPane = func(id string) error {
				killed++
				a.mgr.mu.Lock()
				delete(a.mgr.panes, id)
				a.mgr.mu.Unlock()
				if !a.workerLeadPresent(id) {
					t.Fatal("respawning lead was treated as absent from mgr")
				}
				assertQuiet("after actual respawn removal")
				now = now.Add(5 * time.Minute)
				assertQuiet("respawn gap beyond the absence grace")
				return nil
			}
			a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
				spawned++
				if _, exists := a.mgr.Get(opts.ID); exists {
					t.Fatal("respawn did not free the lead id")
				}
				assertQuiet("before actual replacement spawn")
				next := switchTestPane(opts.ID, opts.Kind, opts.Cmd, opts.ClaudeSessionID)
				next.Agent, next.Cwd, next.Origin = opts.Agent, opts.Cwd, opts.Origin
				next.ClaudeTranscriptPath = opts.ClaudeTranscriptPath
				a.mgr.mu.Lock()
				a.mgr.panes[next.ID] = next
				a.mgr.mu.Unlock()
				return next, nil
			}
			req := httptest.NewRequest("POST", "/api/panes/lead/"+action, nil)
			req.SetPathValue("id", "lead")
			rec := httptest.NewRecorder()
			if action == "resume" {
				a.handleResumePane(rec, req)
			} else {
				a.handleRestartPane(rec, req)
			}
			if rec.Code != 200 || killed != 1 || spawned != 1 {
				t.Fatalf("real %s failed: %d %s kills=%d spawns=%d", action, rec.Code, rec.Body, killed, spawned)
			}
			if a.paneRespawnInProgress("lead") {
				t.Fatal("finished respawn kept its presence marker")
			}
			replacement, _ := a.mgr.Get("lead")
			if replacement == lead || replacement.Snapshot().Status != PaneRunning {
				t.Fatal("handler did not publish a running replacement")
			}
			assertQuiet("after real respawn")
			a.flushAskPush()
			if pushed != 0 {
				t.Fatal("real Resume/restart pushed a retention alert")
			}
		})
	}
}

func TestSuspendedResumableLeadStillUses72HourEscalation(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	putCapabilityFile(t, filepath.Join(worker.Cwd, "unpublished.txt"), "work to preserve")
	a.flagWorkerLead("lead", worker.ID, "uncommitted work")
	suspendWorkerNoticeLead(t, a)
	a.asks.onCreated = a.queueAskPush
	a.askPush.busy = true
	now = now.Add(workerUnpublishedEscalation)
	a.tryArchiveWorker(worker, now)
	if a.asks.openCounts()["lead"] != 1 || a.asks.openCounts()[worker.ID] != 0 || len(a.askPush.pending) != 1 {
		t.Fatal("suspension bypassed the normal 72-hour escalation on the lead")
	}
}

func TestWSLRestartWithUnpublishedWorkersDoesNotCreatePersonAsksOrPush(t *testing.T) {
	a, dirtyWorker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	putCapabilityFile(t, filepath.Join(dirtyWorker.Cwd, "uncommitted.txt"), "preserve dirty work")
	a.flagWorkerLead("lead", dirtyWorker.ID, "uncommitted work")
	// Another worker owns a committed change that has not reached its upstream.
	info := persistedFromSnapshot(dirtyWorker.Snapshot())
	info.ID, info.Cwd = "pane_unpushed_worker", archiveRepo(t)
	putCapabilityFile(t, filepath.Join(info.Cwd, "unpublished.txt"), "preserve local commit")
	archiveGit(t, info.Cwd, "add", "unpublished.txt")
	archiveGit(t, info.Cwd, "commit", "-m", "not pushed")
	unpushedWorker := a.mgr.SuspendExisting(&info)
	if unpushedWorker == nil {
		t.Fatal("could not materialize second worker")
	}
	unpushedWorker.mu.Lock()
	unpushedWorker.Status, unpushedWorker.Resumable = PaneRunning, false
	unpushedWorker.mu.Unlock()
	a.flagWorkerLead("lead", unpushedWorker.ID, "unpushed work")
	ids := map[string]string{}
	for _, note := range a.asks.workerAsks("lead") {
		ids[note.WorkerPane] = note.ID
	}
	// Round-trip the panes' persisted representation, then lose all live panes,
	// read receipts and notice caches as on WSL shutdown. Disk records survive.
	saved, err := json.Marshal([]PersistedPane{
		persistedFromSnapshot(a.mgr.panes["lead"].Snapshot()),
		persistedFromSnapshot(dirtyWorker.Snapshot()),
		persistedFromSnapshot(unpushedWorker.Snapshot()),
	})
	if err != nil {
		t.Fatal(err)
	}
	var records []PersistedPane
	if err := json.Unmarshal(saved, &records); err != nil {
		t.Fatal(err)
	}
	records[0].ClaudeSessionID = "lead-before-reboot"
	a.mgr = NewManager(t.TempDir())
	for _, record := range records {
		p := a.mgr.SuspendExisting(&record)
		if p == nil || p.Snapshot().Status != PaneExited || !p.Snapshot().Resumable {
			t.Fatalf("pane %s did not restore suspended and resumable", record.ID)
		}
	}
	now = now.Add(time.Hour) // reboot alone must not bypass the 72-hour window
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now
	q.mu.Unlock()
	s := a.workerArchives()
	s.noticeMu.Lock()
	s.noticesLoaded, s.retained = false, nil
	s.noticeMu.Unlock()
	s.mu.Lock()
	s.loaded, s.reads, s.flagged, s.checked = false, nil, nil, nil
	s.mu.Unlock()
	a.asks = newAskStore(filepath.Dir(a.asks.path), nil)
	a.asks.now = func() time.Time { return now }
	created, pushed := 0, 0
	a.askPush = askPushBatch{busy: true, fire: func(_, _, _ string) bool { pushed++; return true }}
	a.asks.onCreated = func(ask Ask) {
		if askNeedsUser(ask) {
			created++
		}
		a.queueAskPush(ask)
	}
	// The production boot ordering: restored panes, persisted asks, migration,
	// then retention reconciliation and fresh publication checks of each worker.
	a.asks.closeMissing(func(id string) bool { _, ok := a.mgr.Get(id); return ok })
	a.migrateFlagsToAsks()
	for sweep := 0; sweep < 2; sweep++ {
		a.reconcileWorkerRetentions(true)
		for _, record := range records[1:] {
			worker, _ := a.mgr.Get(record.ID)
			a.tryArchiveWorker(worker, now)
		}
	}
	if created != 0 || len(a.asks.openCounts()) != 0 || len(a.askPush.pending) != 0 {
		t.Fatalf("reboot created person asks or queued pushes: created=%d counts=%v pending=%v", created, a.asks.openCounts(), a.askPush.pending)
	}
	a.flushAskPush()
	if pushed != 0 {
		t.Fatal("reboot pushed a worker-retention alert")
	}
	notes := a.asks.workerAsks("lead")
	if len(notes) != 2 {
		t.Fatalf("reboot lost the lead's quiet notes: %+v", notes)
	}
	for _, note := range notes {
		if note.NeedsUser || note.ID != ids[note.WorkerPane] {
			t.Fatalf("reboot replaced or escalated a quiet note: %+v", note)
		}
	}
}

func TestLeadLossDoesNotReopenADismissedWorkerNotice(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.flagWorkerLead("lead", worker.ID, "unpushed work")
	if err := a.asks.dismiss(a.asks.workerAsks("lead")[0].ID); err != nil {
		t.Fatal(err)
	}
	delete(a.mgr.panes, "lead")
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	now = now.Add(workerLeadAbsenceGrace)
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	a.reconcileWorkerRetentions(true)
	if len(a.asks.workerAsks(worker.ID)) != 0 || len(a.asks.workerAsks("lead")) != 0 {
		t.Fatal("lead loss reopened a dismissed notice")
	}
}

func TestMovingALeadPreservesNoticeOwnership(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	putCapabilityFile(t, filepath.Join(worker.Cwd, "unpublished.txt"), "work to preserve")
	a.tryArchiveWorker(worker, now)
	newLead := testPaneWithTail(t, "replacement-lead", "Replacement lead", "")
	a.mgr.panes[newLead.ID] = newLead
	a.movePaneAsks("lead", newLead.ID, "Replacement lead")
	a.tryArchiveWorker(worker, now) // old and replacement panes briefly coexist
	if len(a.asks.workerAsks(newLead.ID)) != 1 {
		t.Fatal("the old live pane stole the migrated notification recipient")
	}
	delete(a.mgr.panes, "lead")
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now // old final, as after migration/restart
	q.mu.Unlock()
	a.tryArchiveWorker(worker, now)
	a.reconcileWorkerRetentions(true)
	notes := a.asks.workerAsks(newLead.ID)
	if a.asks.openCounts()[worker.ID] != 0 || len(notes) != 1 || notes[0].NeedsUser {
		t.Fatal("a migrated lead was treated as an abandoned worker")
	}
	if err := a.asks.dismiss(notes[0].ID); err != nil {
		t.Fatal(err)
	}
	a.tryArchiveWorker(worker, now)
	a.reconcileWorkerRetentions(true)
	if len(a.asks.workerAsks(newLead.ID)) != 0 || len(a.asks.workerAsks(worker.ID)) != 0 {
		t.Fatal("old spawn-time lead reopened a dismissed migrated notice")
	}
}

func TestWorkerNoticeRelocationCannotChangeAnotherWorker(t *testing.T) {
	s, _ := newTestAskStore(t)
	first, _, err := s.post(askPost{Pane: "lead", Kind: askKindWorker, Key: "worker-x", Text: "quiet", WorkerPane: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(askPost{Pane: "x", Kind: askKindWorker, Key: "worker-x", Text: "preserve", WorkerPane: "other", UpdateID: first.ID}); err != errAskNotFound {
		t.Fatalf("relocated a different worker: %v", err)
	}
	if err := s.dismiss(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(askPost{Pane: "x", Kind: askKindWorker, Key: "worker-x", Text: "preserve", WorkerPane: "x", UpdateID: first.ID}); err != errAskNotFound {
		t.Fatalf("relocation reopened a dismissed record: %v", err)
	}
}

func TestNeverPushedBranchEscalatesWithFreshCommitEvidence(t *testing.T) {
	for _, leadGone := range []bool{false, true} {
		t.Run(fmt.Sprint("leadGone=", leadGone), func(t *testing.T) {
			a, worker, now := archiveFixture(t)
			a.asks.now = func() time.Time { return now }
			archiveGit(t, worker.Cwd, "checkout", "-b", "never-pushed")
			putCapabilityFile(t, filepath.Join(worker.Cwd, "new.txt"), "preserve this commit")
			archiveGit(t, worker.Cwd, "add", "new.txt")
			archiveGit(t, worker.Cwd, "commit", "-m", "unpublished")
			a.tryArchiveWorker(worker, now)
			if a.asks.openCounts()["lead"] != 0 {
				t.Fatal("new unpublished work escalated with a running lead")
			}
			if leadGone {
				delete(a.mgr.panes, "lead")
				a.tryArchiveWorker(worker, now)
				now = now.Add(workerLeadAbsenceGrace)
			} else {
				now = now.Add(workerUnpublishedEscalation)
			}
			a.tryArchiveWorker(worker, now)
			target := "lead"
			if leadGone {
				target = worker.ID
			}
			if a.asks.openCounts()[target] != 1 {
				t.Fatal("verified never-pushed commit did not escalate")
			}
		})
	}
	// A missing repository is uncertainty, not evidence of unpublished commits.
	if _, reason := workerGitCleanPushed(context.Background(), t.TempDir()); reason != "work has no pushed upstream" {
		t.Fatalf("unverified repository became unpublished work: %q", reason)
	}
}

func TestGitUncertaintyPreservesButDoesNotPromoteWorkerRisk(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.asks.now = func() time.Time { return now }
	a.asks.onCreated = a.queueAskPush
	a.askPush.busy = true
	a.flagWorkerLead("lead", worker.ID, "unpushed work")
	first, _ := a.retainedWorker(worker.ID)
	now = now.Add(workerUnpublishedEscalation)
	a.observeWorkerPublication(worker.ID, "worktree status cannot be verified", now)
	a.flagWorkerLead("lead", worker.ID, "worktree status cannot be verified")
	if note, _ := a.retainedWorker(worker.ID); note.Since != first.Since || note.Escalated || a.asks.openCounts()["lead"] != 0 {
		t.Fatal("uncertainty reset or promoted unconfirmed risk")
	}
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	if a.asks.openCounts()["lead"] != 1 || len(a.askPush.pending) != 1 {
		t.Fatal("fresh proof did not use the original risk clock")
	}
	a.askPush.pending = nil
	a.observeWorkerPublication(worker.ID, "worktree status cannot be verified", now)
	a.flagWorkerLead("lead", worker.ID, "worktree status cannot be verified")
	a.observeWorkerPublication(worker.ID, "unpushed work", now)
	if a.asks.openCounts()["lead"] != 1 || len(a.askPush.pending) != 0 {
		t.Fatal("transient Git error retracted or repushed an existing escalation")
	}
	a.observeWorkerPublication(worker.ID, "", now)
	if a.asks.openCounts()["lead"] != 0 {
		t.Fatal("fresh publication proof did not retract escalation")
	}
}

func TestExitedWorkerClearsPublishedWorkNotice(t *testing.T) {
	a, worker, now := archiveFixture(t)
	a.flagWorkerLead("lead", worker.ID, "uncommitted work")
	worker.mu.Lock()
	worker.Status = PaneExited
	worker.mu.Unlock()
	a.tryArchiveWorker(worker, now) // the fixture checkout is clean and published
	if _, kept := a.retainedWorker(worker.ID); kept || len(a.asks.workerAsks("lead")) != 0 {
		t.Fatal("an exited worker kept a stale unpublished-work note")
	}
}
