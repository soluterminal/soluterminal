package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func workerDecision(a *apiServer, p *Pane) string {
	return a.workerArchiveDecisions()[p.ID].Reason
}

// Every check that keeps a finished worker open says why (ST-60).
func TestWorkerArchiveDecisionNamesWhyAWorkerStaysOpen(t *testing.T) {
	for _, tc := range []struct {
		kind, want string
	}{
		{"unread", "final reply unread by lead"},
		{"restart-keystroke", "a key was pressed in the pane since the restart"},
		{"restart-input", "the pane had input since the restart"},
		{"restart-draft", "the input box has a draft"},
		{"restart-unreadable", "the input box cannot be read"},
		{"read-clean", "archived"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			if tc.kind != "unread" {
				a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			}
			if strings.HasPrefix(tc.kind, "restart-") {
				restartWorkerDaemon(t, a, now)
				screen, readable := deliveryClaudeScreen(""), true
				switch tc.kind {
				case "restart-keystroke":
					p.noteHumanInput(now)
				case "restart-input":
					p.inputRevision.Add(1)
				case "restart-draft":
					screen = deliveryClaudeScreen("half a thought")
				case "restart-unreadable":
					readable = false
				}
				stubWorkerBox(t, screen, readable)
			}
			a.tryArchiveWorker(p, now)
			if got := workerDecision(a, p); !strings.Contains(got, tc.want) {
				t.Fatalf("decision=%q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestWorkerQueueSnapshotShowsArchiveDecision(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.tryArchiveWorker(p, now)
	for _, entry := range a.workerQueueSnapshot() {
		if entry["id"] == p.ID {
			d, ok := entry["archiveDecision"].(workerArchiveDecision)
			if !ok || !strings.Contains(d.Reason, "final reply unread by lead") {
				t.Fatalf("archiveDecision=%#v", entry["archiveDecision"])
			}
			return
		}
	}
	t.Fatal("worker missing from the queue snapshot")
}

func TestWorkerArchiveDecisionKeepsSinceWhileUnchanged(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerArchiveDecision(p.ID, "kept", now)
	a.noteWorkerArchiveDecision(p.ID, "kept", now.Add(time.Minute))
	d := a.workerArchiveDecisions()[p.ID]
	if !d.Since.Equal(now) || !d.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("decision=%+v", d)
	}
	a.noteWorkerArchiveDecision("pane_gone", "old", now.Add(-48*time.Hour))
	a.noteWorkerArchiveDecision(p.ID, "changed", now.Add(2*time.Minute))
	if _, kept := a.workerArchiveDecisions()["pane_gone"]; kept {
		t.Fatal("a day-old record was not pruned")
	}
}

func TestWorkerArchiveDecisionWhileAutoArchiveIsOff(t *testing.T) {
	a, p, now := archiveFixture(t)
	q := a.workers()
	q.mu.Lock()
	q.settings.WorkerAutoArchive = false
	q.archiveEnabled.Store(false)
	q.mu.Unlock()
	a.noteWorkerArchiveDecision(p.ID, "final reply unread by lead", now.Add(-time.Minute))
	a.archiveFinishedWorkers(now)
	if got := workerDecision(a, p); got != "auto-archive is off" {
		t.Fatalf("decision=%q", got)
	}
}

// "archived" is recorded only once the archive is complete: a failed
// pane-store cleanup after the commit says what failed.
func TestWorkerArchiveDecisionReportsLateCleanupFailure(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.panes = &failingWorkerCleanupStore{paneStore: a.panes, fail: true}
	a.tryArchiveWorker(p, now)
	if got := workerDecision(a, p); got == "archived" || !strings.Contains(got, "archive failed") {
		t.Fatalf("decision=%q", got)
	}
}

// A held worker at the top of the queue is due again 30 s after each check.
// Taking the first due worker let a few of them use every 5 s tick, so the
// workers below were never checked and showed no decision (ST-295). Every
// finished worker is checked once before any is checked twice.
func TestWorkerArchiveChecksEveryFinishedWorkerInTurn(t *testing.T) {
	a, held, start := archiveFixture(t)
	q := a.workers()
	ids := []string{held.ID}
	q.mu.Lock()
	// More finished workers than one 30 s cycle of 5 s ticks reaches. They
	// stay open too: these panes are no longer workers, a check needing no Git.
	for i := 0; i < 11; i++ {
		p := transactionTestPane(fmt.Sprintf("pane_queued_%02d", i))
		a.mgr.mu.Lock()
		a.mgr.panes[p.ID] = p
		a.mgr.mu.Unlock()
		q.requests = append(q.requests, &workerRequest{Info: workerInfo{ID: "w" + p.ID, State: "finished"}, PaneID: p.ID})
		ids = append(ids, p.ID)
	}
	q.mu.Unlock()
	s := a.workerArchives()
	// Let the background excused-worktree recheck finish before the temp dirs go.
	t.Cleanup(func() { s.excusedOp.Lock(); s.excusedOp.Unlock() })
	seen := map[string]int{}
	for tick := 0; tick < len(ids); tick++ {
		now := start.Add(time.Duration(tick) * 5 * time.Second)
		a.archiveFinishedWorkers(now)
		s.operation.Lock() // wait for this tick's check
		s.operation.Unlock()
		for id, d := range a.workerArchiveDecisions() {
			if d.At.Equal(now) {
				seen[id]++
			}
		}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("%s checked %d times in %d ticks, want once", id, seen[id], len(ids))
		}
	}
	if got := workerDecision(a, held); !strings.Contains(got, "final reply unread by lead") {
		t.Fatalf("held decision=%q", got)
	}
	if got := a.workerArchiveDecisions()["pane_queued_10"].Reason; got != "the pane is no longer a worker" {
		t.Fatalf("last decision=%q", got)
	}
}

func TestNextWorkerArchiveCheckTakesTheOldestDue(t *testing.T) {
	now := time.Now()
	checked := map[string]time.Time{"a": now.Add(-40 * time.Second), "b": now.Add(-90 * time.Second), "c": now.Add(-10 * time.Second)}
	if id, ok := nextWorkerArchiveCheck([]string{"a", "b", "c"}, checked, now); !ok || id != "b" {
		t.Fatalf("got %q %v, want b", id, ok)
	}
	if id, ok := nextWorkerArchiveCheck([]string{"a", "b", "d", "e"}, checked, now); !ok || id != "d" {
		t.Fatalf("got %q %v, want the first never-checked worker", id, ok)
	}
	if _, ok := nextWorkerArchiveCheck([]string{"c"}, checked, now); ok {
		t.Fatal("a worker checked 10 s ago is not due")
	}
}
