package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// watchPreFire ticks periodically and publishes a "schedule-firing-soon"
// uiEvent for each enabled schedule whose NextFire is within
// preFireLeadTime. Fire-once per occurrence — see preFireWarnedAt.
//
// 30s tick granularity means the warning can land anywhere in the window
// [lead - 30s, lead]. Close enough for "5 minutes before" UX; tighter
// would just burn CPU for no user-visible improvement.
func (s *ScheduleStore) watchPreFire(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.checkPreFire()
		}
	}
}

func (s *ScheduleStore) checkPreFire() {
	now := time.Now()
	horizon := now.Add(preFireLeadTime)

	type pending struct {
		id   string
		name string
		next time.Time
	}
	var toWarn []pending

	s.mu.Lock()
	for id, entryID := range s.entryIDs {
		e := s.cron.Entry(entryID)
		if e.ID == 0 {
			continue
		}
		sched, ok := s.items[id]
		if !ok || !sched.Enabled {
			continue
		}
		// A hidden agent-run trigger has no row anywhere in the Schedules UI,
		// so "your schedule fires in 5 minutes" would name a thing the user
		// cannot see, open, or snooze. The agent's own card is its surface.
		if sched.isAgentRun() {
			continue
		}
		// The built-in agents (fix, PR, investigate, triage) run every few
		// minutes around the clock; a "fires in 5 minutes" chime for each
		// was ~16 an hour of noise about infrastructure, not the user's work.
		if sched.Kind == "agent" {
			continue
		}
		next := e.Next
		if next.IsZero() || next.Before(now) || next.After(horizon) {
			continue
		}
		if last, ok := s.preFireWarnedAt[id]; ok && last.Equal(next) {
			continue
		}
		s.preFireWarnedAt[id] = next
		toWarn = append(toWarn, pending{id: id, name: sched.Name, next: next})
	}
	// Housekeeping: drop dedup entries for schedules that have been
	// removed. Prevents an unbounded map over a long daemon lifetime.
	for id := range s.preFireWarnedAt {
		if _, ok := s.entryIDs[id]; !ok {
			delete(s.preFireWarnedAt, id)
		}
	}
	s.mu.Unlock()

	if s.uiBus == nil {
		return
	}
	for _, p := range toWarn {
		s.uiBus.publish(uiEvent{
			Type: "schedule-firing-soon",
			Text: p.name,
		})
	}
}

// scheduleUnlocked wires a schedule into its trigger (cron entry or
// one-shot timer). Caller must hold s.mu.
func (s *ScheduleStore) scheduleUnlocked(sched *Schedule) error {
	// Capture the ID by value — the schedule itself might be mutated
	// by subsequent edits, but callbacks should always look up the
	// current version by ID.
	id := sched.ID
	if sched.oneTime() {
		d := time.Until(*sched.RunAt)
		if d < 0 {
			// Caller (load / create / update) is expected to have
			// filtered past-RunAt schedules already. If we ever hit
			// this, fire on the next tick so the user sees something
			// happen instead of silent nothing.
			d = 0
		}
		s.entryTimers[id] = time.AfterFunc(d, func() { s.fire(id) })
		return nil
	}
	entryID, err := s.cron.AddFunc(sched.Cron, func() { s.fire(id) })
	if err != nil {
		return err
	}
	s.entryIDs[id] = entryID
	return nil
}

func (s *ScheduleStore) unscheduleUnlocked(id string) {
	if entryID, ok := s.entryIDs[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entryIDs, id)
	}
	if t, ok := s.entryTimers[id]; ok {
		t.Stop()
		delete(s.entryTimers, id)
	}
}

// fire is the actual run. Called from the cron goroutine or a
// one-shot timer; must be safe for concurrent fires across different
// schedules.
func (s *ScheduleStore) fire(id string) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	// End-condition gate: a recurring schedule whose Until has passed
	// should stop without firing. Record a skipped run so the user
	// sees *why* the daily 9am stopped after their deadline.
	if !sched.oneTime() && sched.Until != nil && !sched.Until.After(time.Now()) {
		s.unscheduleUnlocked(id)
		sched.Enabled = false
		s.mu.Unlock()
		s.recordRun(id, ScheduleRun{At: time.Now(), Status: "skipped", Detail: "past until — auto-disabled"})
		return
	}
	// Snooze gate: consume the marker and skip exactly this fire.
	// SnoozeUntil is set to (nextFire + 1s) by SnoozeNext, so any fire
	// at or before it is the "next" one the user asked to skip.
	if sched.SnoozeUntil != nil && !time.Now().After(*sched.SnoozeUntil) {
		sched.SnoozeUntil = nil
		_ = s.saveUnlocked()
		s.mu.Unlock()
		s.recordRun(id, ScheduleRun{At: time.Now(), Status: "skipped", Detail: "snoozed"})
		return
	}
	// Snapshot the fields we need so we can release the lock before
	// doing I/O (the PTY write / the agent or task spawn).
	kind, isKind := scheduleKinds[sched.Kind]
	kindSched := *sched
	isTask := sched.isTask()
	isAgentRun := sched.isAgentRun()
	agentID := sched.AgentID
	apply := sched.Apply
	paneID := sched.PaneID
	prompt := sched.Prompt
	cwd := sched.Cwd
	permission := sched.Permission
	model := sched.Model
	timeoutMin := sched.TimeoutMin
	s.mu.Unlock()

	if isAgentRun {
		s.fireAgentRun(id, agentID)
		return
	}
	if isKind && kind.fire != nil {
		kind.fire(s, id, kindSched, apply)
		return
	}
	if isTask {
		s.fireTask(id, cwd, prompt, permission, model, timeoutMin)
		return
	}

	run := ScheduleRun{At: time.Now(), PaneID: paneID}

	if s.mgr == nil {
		run.Status = "error"
		run.Detail = "pane manager unavailable"
		s.recordRun(id, run)
		return
	}
	pane, ok := s.mgr.Get(paneID)
	if !ok {
		// Pause rather than fail on every fire: a prompt schedule aimed at a
		// closed pane used to error (and page the phone) every 5 minutes or
		// every night for weeks. This run's error is the one notification;
		// the row then says why it stopped.
		s.pauseForReason(id, targetPaneGoneReason)
		run.Status = "error"
		run.Detail = "target pane no longer exists — paused. Pick a pane and turn it back on to resume."
		s.recordRun(id, run)
		return
	}
	snap := pane.Snapshot()
	if snap.Kind == "native" {
		// Focus panes have no terminal screen to scan. Their buffer contains raw
		// provider NDJSON and TotalStrippedChars is never advanced, so applying
		// computeState's terminal-position heuristics can manufacture a permanent
		// busy/choice state. Native turn + approval signals are authoritative.
		if snap.Status == PaneExited {
			run.Status = "skipped"
			run.Detail = "pane not ready (exited)"
			s.recordRun(id, run)
			return
		}
		if pane.hasNativePending() {
			run.Status = "skipped"
			run.Detail = "pane not ready (waiting on approval)"
			s.recordRun(id, run)
			return
		}
		if pane.nativeInTurnNow() {
			run.Status = "skipped"
			run.Detail = "pane not ready (agent mid-turn)"
			s.recordRun(id, run)
			return
		}
	} else {
		state := pane.computeState()
		if !isFireReadyState(state, snap.Status) {
			run.Status = "skipped"
			run.Detail = fmt.Sprintf("pane not ready (state=%s)", state)
			s.recordRun(id, run)
			return
		}
	}

	// Focus panes speak provider protocols, not terminal keystrokes; terminal
	// TUIs need body and Enter split across two PTY frames. Both live in
	// submitLineToPane (pty_submit.go) — this path is one of three copies that
	// re-implemented the same discipline. HasClaudeDescendant is a /proc walk,
	// so it is skipped for the native branch that never reads it.
	body := strings.TrimRight(prompt, "\r\n")
	hasClaude := snap.Kind != "native" && pane.HasClaudeDescendant()
	wrote, err := submitLineToPane(pane, body, hasClaude)
	if err != nil {
		detail := err.Error()
		if snap.Kind == "native" {
			detail = "native submit failed: " + detail
		}
		run.Status = "error"
		run.Detail = detail
		s.recordRun(id, run)
		return
	}

	run.Status = "fired"
	run.Detail = "delivered to " + displayTitle(pane)
	s.recordRun(id, run)
	log.Printf("scheduler: fired %q → %s (%d bytes)", sched.Name, displayTitle(pane), wrote)

	// Toast the UI so the user knows the fire happened, even if their
	// eyes are elsewhere. Also emit a dedicated "schedule-fired" event
	// separate from the toast, so the desktop voice layer can trigger
	// on it without pattern-matching arbitrary toast strings.
	if s.uiBus != nil {
		s.uiBus.publish(uiEvent{
			Type: "schedule-fired",
			Text: sched.Name,
		})
		s.uiBus.publish(uiEvent{
			Type: "toast",
			Text: fmt.Sprintf("⏰ %s → %s", sched.Name, displayTitle(pane)),
		})
	}
}

// targetPaneGoneReason is the PausedReason of a prompt schedule whose target
// pane closed.
const targetPaneGoneReason = "its target pane is gone"

// pauseForReason disables a schedule on the daemon's own initiative and
// records why, for the row to show.
func (s *ScheduleStore) pauseForReason(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.items[id]
	if !ok {
		return
	}
	s.unscheduleUnlocked(id)
	sched.Enabled = false
	sched.PausedReason = reason
	_ = s.saveUnlocked()
}

// fireAgentRun is the managed-agent counterpart of fireTask/fireAgent
// (docs/DESIGN-agents-surface.md §5.2). It owns none of the policy: the agent
// store decides whether the run may start (paused, globally paused, deleted,
// single-flight, the --disallowedTools probe) so there is exactly one place
// that answer lives, and stamps the verdict here as this schedule's run record
// so the fire is visible in the same history every other kind writes to.
//
// The paused case is doubly stopped: SetPaused also disables THIS schedule, so
// a paused agent normally never reaches here at all.
func (s *ScheduleStore) fireAgentRun(schedID, agentID string) {
	run := ScheduleRun{At: time.Now()}
	s.mu.Lock()
	runner := s.agentRunner
	s.mu.Unlock()
	if runner == nil {
		run.Status = "error"
		run.Detail = "agents are unavailable on this daemon"
		s.recordRun(schedID, run)
		return
	}
	status, detail := runner(agentID, agentTriggerCron)
	switch status {
	case "skipped":
		run.Status = "skipped"
	case "error":
		run.Status = "error"
	default:
		run.Status = "fired"
	}
	run.Detail = detail
	s.recordRun(schedID, run)
}

// isFireReadyState whitelists the pane states where prompt injection
// is safe. We intentionally skip `busy` (a tool-call in flight),
// `choice-prompt` (Claude awaiting numeric y/n — our prompt would be
// interpreted as the answer), and `exited`. Everything else (idle,
// recent, awaiting-input) is fair game.
func isFireReadyState(state PaneRuntimeState, status PaneStatus) bool {
	if status == PaneExited {
		return false
	}
	switch state {
	case StateBusy, StateChoicePrompt, StateExited:
		return false
	}
	return true
}

func (s *ScheduleStore) recordRun(id string, run ScheduleRun) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	// Snapshot the push decision inputs while we hold the lock; the POST
	// itself happens off-lock (and off this goroutine) below.
	pusher := s.pusher
	// Hidden agent-run triggers push nothing: a "schedule-error" phone
	// notification naming a schedule the user cannot open is a dead end. The
	// run still lands in this schedule's history and on the agent's card,
	// which is the surface the user actually has.
	if sched.isAgentRun() {
		pusher = nil
	}
	metrics := s.metrics
	name := sched.Name
	// An IMPORTED schedule's outcomes are also the owning agent's run history
	// (docs/DESIGN-agents-surface.md §8.3). recordRun is the choke point every
	// run of every kind passes through, so mirroring HERE means the card shows
	// the runs that actually happened — including the skips and the spawn
	// failures — rather than a second tally kept somewhere else.
	ownedBy := sched.OwnedByAgentID
	mirror := s.ownedRunMirror
	notifyOnRun := sched.NotifyOnRun
	wasOneTime := sched.oneTime()
	kind := sched.kindLabel()
	var deliverChat *ScheduleChatRef
	if sched.DeliverChat != nil && sched.isTask() {
		ref := *sched.DeliverChat
		deliverChat = &ref
	}
	// Prepend (newest first), cap length.
	sched.Runs = append([]ScheduleRun{run}, sched.Runs...)
	if len(sched.Runs) > maxRunHistory {
		sched.Runs = sched.Runs[:maxRunHistory]
	}
	// Lifetime counters — survive beyond the 20-run cap.
	switch run.Status {
	case "fired":
		sched.FireCount++
		at := run.At
		sched.LastFiredAt = &at
	case "skipped":
		sched.SkipCount++
	case "error":
		sched.ErrorCount++
	}
	// Terminal-condition check: one-time schedules disable after the
	// single attempt regardless of outcome; recurring disables when
	// MaxFires is reached (counted by actual fires, not skips/errors).
	if sched.oneTime() {
		s.unscheduleUnlocked(id)
		sched.Enabled = false
	} else if sched.MaxFires > 0 && sched.FireCount >= sched.MaxFires && run.Status == "fired" {
		s.unscheduleUnlocked(id)
		sched.Enabled = false
	}
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.notifyScheduleChanged(id)
	metrics.IncLabelled("solud_schedule_runs_total", map[string]string{"kind": kind, "status": run.Status})

	// Off the lock, on purpose: the mirror calls into the ManagedAgentStore, which
	// calls back into this store (SetPaused → SetEnabled).
	if ownedBy != "" && mirror != nil {
		mirror(ownedBy, run)
	}

	// Phone push, three cases:
	//   error                 → always ("schedule-error"): a job failing
	//                           while nobody watches is what push is FOR.
	//   one-time skip         → also "schedule-error": the reminder's one
	//                           chance is spent (one-timers disable after
	//                           any attempt) — a silently missed reminder
	//                           is a failure from the user's seat.
	//   successful run        → "schedule-fired", per-schedule opt-in.
	// Recurring skips (pane busy, snoozed) stay quiet — the next fire is
	// minutes away and the counters already tell the story.
	if pusher != nil {
		switch {
		case run.Status == "error" || (run.Status == "skipped" && wasOneTime):
			// A failed task run's Summary says why ("failed: <last error
			// line>"); lead with it and keep the detail for exit and time.
			body := run.Detail
			if run.Status == "error" && run.Summary != "" {
				body = run.Summary + " (" + run.Detail + ")"
			}
			go pusher.FireSchedule(id, name, "schedule-error", body)
		case run.Status == "fired" && notifyOnRun:
			// Task runs carry the report's opening lines in Summary — push
			// THAT, so the notification is the digest, not just "it ran".
			body := run.Detail
			if run.Summary != "" {
				body = run.Summary
			}
			go pusher.FireSchedule(id, name, "schedule-fired", body)
		}
	}

	// Chat delivery (an optional feature) — the report goes where the user
	// already talks. Failures stamp the run AND push.
	if run.Status == "fired" && deliverChat != nil {
		if scheduleChatDelivery != nil {
			go scheduleChatDelivery(s, id, run, name, *deliverChat)
		} else {
			// Loud, like any failed delivery: the user picked chat delivery
			// because they aren't watching the app.
			go s.stampRunDetail(id, run.At, "chat ✗ delivery isn't available in this build")
			if pusher != nil {
				go pusher.FireSchedule(id, name, "schedule-error", "report couldn't be delivered: chat delivery isn't available in this build")
			}
		}
	}
}

// stampRunDetail appends a note to an existing run record (matched by
// timestamp) — how async delivery outcomes reach the reports reader.
func (s *ScheduleStore) stampRunDetail(id string, at time.Time, note string) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if ok {
		for i := range sched.Runs {
			if sched.Runs[i].At.Equal(at) {
				if sched.Runs[i].Detail != "" {
					sched.Runs[i].Detail += " · "
				}
				sched.Runs[i].Detail += note
				break
			}
		}
		_ = s.saveUnlocked()
	}
	s.mu.Unlock()
	if ok {
		s.notifyScheduleChanged(id)
	}
}
