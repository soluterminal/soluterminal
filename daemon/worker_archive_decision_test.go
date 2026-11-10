package main

import (
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
