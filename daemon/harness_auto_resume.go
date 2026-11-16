package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

// Live-exit auto-resume for managed terminal harnesses (DeepSeek, Pi).
//
// Claude/Codex/Muse pane commands end with `…; exec bash -i`, so when the
// provider process exits the pane drops to a live shell instead of a dead-end.
// A managed DeepSeek/Pi pane runs the harness as the pane command, so a client
// exit marks the pane exited+resumable and parks the "Resume session" card.
//
// When the user has opted into restart recovery "auto", this closes that parity
// gap: a managed harness pane that exits on its own while the daemon is up is
// resumed in place. It is deliberately conservative:
//
//   - only while the recovery setting is "auto";
//   - only a managed terminal harness with a recorded durable session;
//   - only a non-clean exit — a `/exit` or idle Ctrl+C reports code 0 and is
//     the user's deliberate close, so it is never resurrected;
//   - bounded by harnessAutoResumeMaxAttempts within harnessAutoResumeWindow,
//     so a client that crashes on startup cannot respawn forever. On exhaustion
//     the Resume card is left for the user, exactly as before.
const (
	harnessAutoResumeWindow      = 10 * time.Minute
	harnessAutoResumeMaxAttempts = 3
)

// harnessAutoResumeBudget is the rolling per-pane attempt counter that keeps an
// auto-resume from becoming a crash-loop. Keyed by pane id, which is stable
// across an in-place resume (the conversation that keeps crashing is the thing
// being bounded).
type harnessAutoResumeBudget struct {
	mu       sync.Mutex
	window   time.Duration
	max      int
	attempts map[string][]time.Time
}

func newHarnessAutoResumeBudget(window time.Duration, max int) *harnessAutoResumeBudget {
	return &harnessAutoResumeBudget{window: window, max: max, attempts: map[string][]time.Time{}}
}

// allow reports whether paneID may make another automatic resume attempt now.
func (b *harnessAutoResumeBudget) allow(paneID string, now time.Time) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pruneLocked(paneID, now)) < b.max
}

// record notes one automatic resume attempt for paneID.
func (b *harnessAutoResumeBudget) record(paneID string, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	recent := b.pruneLocked(paneID, now)
	b.attempts[paneID] = append(recent, now)
}

// pruneLocked drops attempts outside the rolling window. In-place filter; the
// caller holds b.mu.
func (b *harnessAutoResumeBudget) pruneLocked(paneID string, now time.Time) []time.Time {
	cutoff := now.Add(-b.window)
	src := b.attempts[paneID]
	recent := src[:0]
	for _, at := range src {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	if len(recent) == 0 {
		delete(b.attempts, paneID)
	} else {
		b.attempts[paneID] = recent
	}
	return recent
}

// The process-wide budget the pane-exit hook consults. Tests build their own.
var harnessAutoResumeLimiter = newHarnessAutoResumeBudget(harnessAutoResumeWindow, harnessAutoResumeMaxAttempts)

// shouldAutoResumeHarnessOnExit is the pure policy for the live-exit path. It
// takes a snapshot so the decision sees a coherent pane state.
func shouldAutoResumeHarnessOnExit(snap PaneSnapshot, autoResume bool) bool {
	if !autoResume {
		return false
	}
	if snap.Status != PaneExited || !snap.Resumable {
		return false
	}
	// Managed terminal harness only. A plain shell that happens to carry a
	// harness sidecar is not relaunchable unattended.
	managed := strings.TrimSpace(snap.HarnessProvider) != "" || cmdLaunchesManagedHarness(snap.Cmd)
	if !managed {
		return false
	}
	// Require the durable conversation. Without one, a "resume" would start a
	// fresh session and could mask a broken client behind an endless new-session
	// loop; leave the Resume card instead.
	if strings.TrimSpace(snap.HarnessSessionID) == "" {
		return false
	}
	// A clean exit (code 0) is a deliberate `/exit` or idle Ctrl+C: the user
	// chose to close the harness. Never resurrect it.
	if snap.ExitCode != nil && *snap.ExitCode == 0 {
		return false
	}
	return true
}

// autoResumeHarnessPane resumes a managed terminal harness pane that exited on
// its own. It runs off the pane-exit hook (goroutine), mirrors handleResumePane
// but with no live client viewport (the next attach re-measures), and consults
// the rolling budget.
func (a *apiServer) autoResumeHarnessPane(p *Pane) {
	if a == nil || a.mgr == nil || p == nil {
		return
	}
	autoResume := a.recoverySettings != nil && a.recoverySettings.AutoResumeAfterRestart()
	if !autoResume {
		return
	}
	snap := p.Snapshot()
	// The process poll adopts the client's sidecar while it lives; read it
	// directly here so a client that wrote its session and then crashed before
	// the next poll is still resumable. Pure read — it must not re-persist the
	// pane the base exit hook just removed.
	if strings.TrimSpace(snap.HarnessSessionID) == "" {
		if provider, sessionID, ok := readHarnessSessionRecord(harnessSessionDirs(a.dataDir), snap.ID); ok {
			snap.HarnessProvider, snap.HarnessSessionID = provider, sessionID
		}
	}
	if !shouldAutoResumeHarnessOnExit(snap, true) {
		return
	}
	now := time.Now()
	if !harnessAutoResumeLimiter.allow(snap.ID, now) {
		log.Printf("auto-resume pane %s: attempt budget exhausted (%d in %s); leaving the Resume card",
			snap.ID, harnessAutoResumeMaxAttempts, harnessAutoResumeWindow)
		return
	}
	// Single-flight shared with resume/restart/switch-mode: a user clicking
	// Resume (or a pane restart) wins the race and this becomes a no-op.
	if !p.tryAcquireRespawn() {
		return
	}
	defer p.releaseRespawn()
	// Re-snapshot after the acquire; the pane may have come back meanwhile.
	snap = p.Snapshot()
	if snap.Status != PaneExited {
		return
	}
	harnessAutoResumeLimiter.record(snap.ID, now)
	newPane, err := a.resumeExitedPane(snap, 0, 0)
	if err != nil {
		log.Printf("auto-resume harness pane %s failed: %v", snap.ID, err)
		return
	}
	log.Printf("auto-resumed harness pane %s → %s (session %s)", newPane.ID, newPane.Title, shortSessionID(snap.HarnessSessionID))
}
