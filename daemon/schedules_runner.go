package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
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
	retry := time.NewTicker(paneWaitPoll)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick()
		case <-retry.C:
			s.retryPaneWaits()
		}
	}
}

// tick is the 30-second pass: pre-fire warnings, one-time and recurring runs
// set again from the wall clock, and prompt runs waiting for a busy pane.
func (s *ScheduleStore) tick() {
	s.checkPreFire()
	s.resyncOneTimes()
	s.wakeCron()
	s.retryPaneWaits()
}

// wakeCron makes the cron loop read the wall clock again. The loop sleeps on
// one Go timer until its next entry is due, and reads the clock only when it
// wakes: for a run, or when an entry is added or removed. That timer has the
// same fault as a one-time timer (armOneTimeUnlocked): it stops while the
// machine sleeps and can run behind the wall clock, so a recurring run was
// late by a share of the gap since the loop last woke (5% on one WSL PC: up
// to 39 s with many automations, minutes with one daily automation). No
// entry has id 0, so removing it changes nothing; the loop stops its timer,
// reads the clock, and sets the timer again for what is left. An entry that
// is already due then runs at once. Not under s.mu: the loop takes the
// hand-off itself, and a stopped cron just returns.
func (s *ScheduleStore) wakeCron() {
	s.cron.Remove(0)
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
		if !ok || !sched.Enabled || !scheduleAvailable(sched) {
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
	if !scheduleAvailable(sched) {
		return nil
	}
	// Capture the ID by value — the schedule itself might be mutated
	// by subsequent edits, but callbacks should always look up the
	// current version by ID.
	id := sched.ID
	if sched.oneTime() {
		// Caller (load / create / update) is expected to have filtered
		// past-RunAt schedules already. If we ever hit one, it fires at
		// once so the user sees something happen instead of silent nothing.
		s.armOneTimeUnlocked(id, *sched.RunAt)
		return nil
	}
	entryID, err := s.cron.AddFunc(sched.Cron, func() { s.fire(id) })
	if err != nil {
		return err
	}
	s.entryIDs[id] = entryID
	return nil
}

// armOneTimeUnlocked sets the timer of a one-time schedule due at `at`,
// replacing any earlier one. A Go timer counts a clock that stops while the
// machine sleeps and, on some machines, runs slower than the wall clock (one
// WSL PC lost 5%, so a reminder set an hour ahead ran three minutes late and
// one set days ahead ran 55 minutes late). So the timer is only a wake-up:
// the wall clock decides. tick sets every waiting timer again from the wall
// clock (resyncOneTimes), and a timer that ends before the wall clock has
// reached `at` waits out the rest. Caller holds s.mu.
func (s *ScheduleStore) armOneTimeUnlocked(id string, at time.Time) {
	if old, ok := s.entryTimers[id]; ok {
		old.Stop()
	}
	at = at.Round(0) // wall-clock reading only, so the comparisons below use it
	d := at.Sub(s.nowLocked())
	if d < 0 {
		d = 0
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		s.mu.Lock()
		if s.entryTimers[id] != t {
			// Set again, cancelled, or already run.
			s.mu.Unlock()
			return
		}
		if at.After(s.nowLocked()) {
			s.armOneTimeUnlocked(id, at)
			s.mu.Unlock()
			return
		}
		// Taken out before the run, so a later pass cannot run it twice.
		delete(s.entryTimers, id)
		s.mu.Unlock()
		s.fire(id)
	})
	s.entryTimers[id] = t
}

// resyncOneTimes sets every waiting one-time timer again from the wall
// clock. One that is already due runs now.
func (s *ScheduleStore) resyncOneTimes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.entryTimers {
		if sched, ok := s.items[id]; ok && sched.RunAt != nil {
			s.armOneTimeUnlocked(id, *sched.RunAt)
		}
	}
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
	// Paused, edited, deleted or finished: a run waiting for its pane ends.
	s.dropPaneWaitLocked(id)
}

// dropPaneWaitLocked ends an automation's wait for its pane, in memory and
// in its saved record, and reports whether there was one. Caller holds s.mu
// and saves.
func (s *ScheduleStore) dropPaneWaitLocked(id string) bool {
	_, waiting := s.paneWaits[id]
	delete(s.paneWaits, id)
	if sched, ok := s.items[id]; ok && sched.WaitingSince != nil {
		sched.WaitingSince, sched.WaitingPane = nil, ""
		waiting = true
	}
	return waiting
}

// fire is the actual run. Called from the cron goroutine or a
// one-shot timer; must be safe for concurrent fires across different
// schedules.
func (s *ScheduleStore) fire(id string) { s.fireAs(id, fireScheduled) }

// fireMode says who started a run.
type fireMode int

const (
	// fireScheduled: the timetable (a cron entry, a one-time timer, catch-up).
	fireScheduled fireMode = iota
	// firePerson: the person's Run now. It is answered at once, so it never
	// waits for a busy pane.
	firePerson
	// fireRetry: a scheduled prompt that waited for its busy pane. It was
	// due and admitted already, so the until and snooze gates are not asked
	// again (asking would spend a snooze meant for the next run).
	fireRetry
)

func (s *ScheduleStore) fireAs(id string, mode fireMode) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok || !scheduleAvailable(sched) {
		s.mu.Unlock()
		return
	}
	// The person's pauses, resumes and edits so far. A wait for a busy pane
	// is started or continued only if none has landed since: pausing or
	// editing an automation ends its run, also one already on its way here.
	// So does the daemon turning it off (a one-time that has had its run, a
	// pane that is gone, the stop on agent messaging): due carries both.
	due := dueStamp{seq: s.editSeq[id], on: sched.Enabled}
	if mode == fireRetry {
		w, waiting := s.paneWaits[id]
		if !waiting || w.due.seq != due.seq || w.due.on && !sched.Enabled {
			// The wait ended (delivered, given up, paused, edited) after
			// this retry was started: there is nothing left to retry.
			if s.dropPaneWaitLocked(id) {
				_ = s.saveUnlocked()
			}
			s.mu.Unlock()
			return
		}
	}
	// End-condition gate: a recurring schedule whose Until has passed
	// should stop without firing. Record a skipped run so the user
	// sees *why* the daily 9am stopped after their deadline.
	if mode != fireRetry && !sched.oneTime() && sched.Until != nil && !sched.Until.After(time.Now()) {
		s.unscheduleUnlocked(id)
		sched.Enabled = false
		s.mu.Unlock()
		s.recordRun(id, ScheduleRun{At: time.Now(), Status: "skipped", Detail: "past until — auto-disabled"})
		return
	}
	// Snooze gate: consume the marker and skip exactly this fire.
	// SnoozeUntil is set to (nextFire + 1s) by SnoozeNext, so any fire
	// at or before it is the "next" one the user asked to skip.
	if mode != fireRetry && sched.SnoozeUntil != nil && !time.Now().After(*sched.SnoozeUntil) {
		sched.SnoozeUntil = nil
		_ = s.saveUnlocked()
		s.mu.Unlock()
		s.recordRun(id, ScheduleRun{At: time.Now(), Status: "skipped", Detail: "snoozed"})
		return
	}
	// The person's stop on agent messaging: a schedule an agent filed is
	// held, not run and not lost. Resume runs it once and turns it back on.
	// A prompt that was waiting for its busy pane when the person stopped
	// agent messaging is held the same way on its next retry, not typed
	// (unscheduleUnlocked ends the wait).
	if mode != firePerson && agentMessagingPaused() && scheduleFiledByAgent(sched) {
		s.unscheduleUnlocked(id)
		sched.Enabled = false
		sched.PausedReason = agentMessagingScheduleReason
		_ = s.saveUnlocked()
		s.mu.Unlock()
		s.notifyScheduleChanged(id)
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

	run := ScheduleRun{At: time.Now(), PaneID: paneID, byRunNow: mode == firePerson}
	ident, waited, ok := s.beginPromptRun(id, paneID, run, mode, due)
	if !ok {
		return
	}
	// The attempt ends the automation's wait for the pane, unless it ends in
	// a hold, or it is a Run now that typed nothing (the scheduled run it
	// did not replace goes on waiting).
	held, delivered := false, false
	defer func() {
		if !held && (mode != firePerson || delivered) {
			s.endPaneWait(id)
		}
		s.endPromptRun(id, paneID) // last: the attempt is over, saves included
	}()
	// hold keeps the prompt for a later try: nothing was typed, and the
	// pane will take it once why has passed. A Run now is answered at once
	// instead.
	hold := func(why paneWaitReason) {
		if mode == firePerson {
			run.Status = "skipped"
			run.Detail = why.row
			s.recordRun(id, run)
			return
		}
		held = true
		s.waitForPane(id, paneID, run, why, mode, due, ident)
	}
	// skip records a run that will not be tried again. One that had waited
	// for its pane is announced: the person was told nothing while it
	// waited, so a quiet skip now would lose the run without a word.
	skip := func(detail string) {
		run.Status = "skipped"
		run.Detail = detail
		run.tellPerson = waited && mode != firePerson
		s.recordRun(id, run)
		if run.tellPerson {
			s.toastGaveUp(s.noticeName(id), detail)
		}
	}

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
	// The direct read, taken once, just before typing (beginPromptRun's was
	// the cached one). A pane that turned busy in between waits like any
	// busy pane.
	state := pane.computeState()
	switch {
	case isFireReadyState(state, snap.Status):
	case snap.Status != PaneExited && state == StateBusy:
		hold(waitBusy)
		return
	case snap.Status != PaneExited && state == StateChoicePrompt:
		hold(waitMenu)
		return
	default:
		skip(fmt.Sprintf("pane not ready (state=%s)", state))
		return
	}

	// Terminal TUIs need body and Enter split across two PTY frames; that
	// discipline lives in submitLineToPane (pty_submit.go).
	body := strings.TrimRight(prompt, "\r\n")
	hasClaude := pane.HasClaudeDescendant()
	wrote, err := submitLineToPane(pane, body, hasClaude)
	if wrote > 0 {
		// Typed, submitted or not: the next prompt for this pane waits for
		// a new quiet period (paneNotFree).
		s.noteRelease(paneID)
	}
	switch {
	case errors.Is(err, errDraftGuardHeld):
		// The person is typing in the pane, or its input box has text:
		// nothing was typed (the check is made under the writer lock, with
		// the paste). The prompt waits, as for a busy pane.
		hold(typingWaitReason(err))
		return
	case errors.Is(err, errDeliveryChoicePrompt) && wrote == 0:
		// A permission menu opened after the readiness check.
		hold(waitMenu)
		return
	case errors.Is(err, errPutBackInBox):
		// Not ready: the person sends or clears the message in its input
		// box first (its chip shows it).
		skip(fmt.Sprintf("pane not ready (%v)", err))
		return
	}
	if err != nil {
		run.Status = "error"
		run.Detail = err.Error()
		s.recordRun(id, run)
		return
	}

	delivered = true
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
		// Run now is answered to the person who pressed it (runNow in the
		// API response); a second toast would say the same thing twice.
		if mode != firePerson {
			s.uiBus.publish(uiEvent{
				Type: "toast",
				Text: fmt.Sprintf("⏰ %s → %s", sched.Name, displayTitle(pane)),
			})
		}
	}
}

// A scheduled prompt whose pane cannot take it now waits for the pane.
// Before, the run was recorded as skipped at that second and never tried
// again: a weekly review that met a busy pane was lost for a week and nobody
// was told.
var (
	// paneWaitWindow is how long a scheduled prompt waits for its pane
	// before it is given up, recorded as skipped, and the person is told.
	paneWaitWindow = time.Hour
	// paneWaitPoll paces the checks on the pane in between.
	paneWaitPoll = 5 * time.Second
	// paneReleaseQuiet is the quiet a pane must show before a WAITING prompt
	// goes in: no output for this long, and no scheduled prompt typed into
	// it for this long. "Not busy" alone is only 1.5 s without output, and
	// without the second half every prompt waiting for one pane went in on
	// the same poll, back to back.
	paneReleaseQuiet = 10 * time.Second
)

// paneWaitGaveUp starts the run detail of a prompt given up after
// paneWaitWindow.
const paneWaitGaveUp = "not sent: "

// paneWaitReason is why a prompt is not typed into its pane now: row for the
// automation's row and a skipped Run now, gaveUp for the give-up record
// ("not sent: <gaveUp> for 60 min").
type paneWaitReason struct{ row, gaveUp string }

var (
	waitBusy  = paneWaitReason{"the pane is busy", "the pane stayed busy"}
	waitMenu  = paneWaitReason{"the pane is waiting on an answer", "the pane waited on an answer"}
	waitQueue = paneWaitReason{"another automation goes into the pane first", "other automations kept the pane"}
	waitTyped = paneWaitReason{"you are typing in the pane", "you were typing in the pane"}
	waitDraft = paneWaitReason{"the pane's input box has text", "the pane's input box had text"}
	// waitUnsent: a message someone else left unsent in the box (an agent's
	// message put back there, say), which is not the person typing.
	waitUnsent = paneWaitReason{"the pane's input box holds an unsent message", "the pane's input box held an unsent message"}
)

// typingWaitReason turns a draft-guard refusal (errDraftGuardHeld) into the
// reason shown to the person.
func typingWaitReason(err error) paneWaitReason {
	switch draftHeldReason(err) {
	case "the person is typing":
		return waitTyped
	case heldBoxTextReason:
		return waitDraft
	}
	return waitUnsent
}

// dueStamp is the automation as its run came due (fireAs): the person's edit
// count and whether it was on. A wait is started, continued or retried only
// while the count is the same and an automation that was on is still on.
// (Tests fire automations that are off; those may wait.)
type dueStamp struct {
	seq uint64
	on  bool
}

// paneWaitResumeDelay is how long after start the waits saved by the last
// run of the daemon are taken up again, once restored panes are back.
var paneWaitResumeDelay = 30 * time.Second

// waitRestart is the row's reason until the first try after a restart.
var waitRestart = paneWaitReason{"SoluTerminal restarted; checking the pane again", "the pane was not free after SoluTerminal restarted"}

// paneWait is one scheduled prompt waiting for its pane.
type paneWait struct {
	since  time.Time // when the pane first could not take it
	until  time.Time // when the wait is given up
	later  int       // scheduled runs that came due while this one waited
	due    dueStamp  // the automation when the run came due (see fireAs)
	pane   string    // paneWaitIdentity of the pane the prompt waits for
	paneID string    // the pane, for one release per pane, oldest first
	why    string    // paneWaitReason.row of the last refusal, for the row
}

// paneWaitIdentity names what runs in a pane: its kind, agent and command. A
// pane restarted under the same id as a different agent reads differently,
// and a prompt that waited for the old one is not typed into the new one.
func paneWaitIdentity(pane *Pane) string {
	snap := pane.Snapshot()
	cmd := ""
	if len(snap.Cmd) > 0 {
		cmd = snap.Cmd[0]
	}
	return snap.Kind + "|" + snap.Agent + "|" + cmd
}

// paneNotFree says why a terminal pane cannot take a scheduled prompt now but
// will later, or false when it can (or never will: an exited pane and a
// closed one keep their own handling in fireAs).
//
// lastRelease is when a scheduled prompt was last typed into the pane. After
// it, the others see the pane as taken until it has been quiet again, so a
// pane gets one prompt per quiet period however many wait for it. waited: the
// prompt has been waiting, so it also needs paneReleaseQuiet without output,
// not only a pane that is not busy this second. person: a Run now, which the
// person is there for; only a busy pane or a menu stops it.
//
// The cached read, not computeState: this runs every paneWaitPoll, and the
// direct read advances the fire-once positions, so a poll could take the
// "waiting for you" edge before the attention watcher's own sample saw it.
// New output always misses the cache, so a pane that has just turned busy
// reads busy here. fireAs takes its own direct read just before it types.
func paneNotFree(pane *Pane, lastRelease time.Time, waited, person bool) (paneWaitReason, bool) {
	if pane == nil {
		return paneWaitReason{}, false
	}
	snap := pane.Snapshot()
	if snap.Status == PaneExited {
		return paneWaitReason{}, false
	}
	switch pane.computeStateCached() {
	case StateBusy:
		return waitBusy, true
	case StateChoicePrompt:
		return waitMenu, true
	}
	if person {
		return paneWaitReason{}, false
	}
	now := time.Now()
	if !lastRelease.IsZero() && now.Sub(lastRelease) < paneReleaseQuiet {
		return waitQueue, true
	}
	if waited {
		if last := atomic.LoadInt64(&pane.LastActivityMs); last > 0 && now.Sub(time.UnixMilli(last)) < paneReleaseQuiet {
			return waitBusy, true
		}
	}
	return paneWaitReason{}, false
}

// olderWaitOnPaneLocked reports another automation waiting for the same pane
// ahead of this one: any wait there if this one is not waiting yet, else one
// that began earlier. Caller holds s.mu.
func (s *ScheduleStore) olderWaitOnPaneLocked(id, paneID string) bool {
	mine := s.paneWaits[id]
	for other, w := range s.paneWaits {
		if other == id || w.paneID != paneID {
			continue
		}
		if mine == nil || w.since.Before(mine.since) || w.since.Equal(mine.since) && other < id {
			return true
		}
	}
	return false
}

// beginPromptRun admits one attempt to type a prompt schedule into its pane.
// ok reports whether fireAs goes on to type; ident is the pane's identity and
// waited whether the run had been waiting for the pane.
//
// At most one attempt per pane runs at a time, across all automations, and
// one per automation: each reads the pane and types inside that turn, so two
// cannot both find the pane free and type back to back. Automations waiting
// for one pane go in oldest first. A pane that cannot take the prompt now
// ends a Run now at once with the reason; a scheduled run starts, or stays
// in, the wait for the pane.
func (s *ScheduleStore) beginPromptRun(id, paneID string, run ScheduleRun, mode fireMode, due dueStamp) (ident string, waited, ok bool) {
	var pane *Pane
	if s.mgr != nil {
		if p, found := s.mgr.Get(paneID); found {
			pane = p
			ident = paneWaitIdentity(p)
		}
	}
	skipped := func(detail string) {
		run.Status = "skipped"
		run.Detail = detail
		s.recordRun(id, run)
	}

	s.mu.Lock()
	w, waiting := s.paneWaits[id]
	if mode == fireRetry && !waiting {
		// The wait this retry was for is over.
		s.mu.Unlock()
		return "", false, false
	}
	if s.promptRunning[id] {
		s.mu.Unlock()
		if mode != fireRetry {
			// The next poll retries a retry; a timetable run or a Run now
			// is told why nothing more was typed.
			skipped("another run of this automation is being sent to the pane")
		}
		return "", waiting, false
	}
	waitedFor := ""
	if waiting {
		waitedFor = w.pane
	} else if sched := s.items[id]; sched != nil && sched.WaitingSince != nil && mode != firePerson {
		// A wait saved before a restart and not taken up yet (the run came
		// due inside paneWaitResumeDelay): this run is that waiting run, so
		// it is held to what ran in the pane then, and to a quiet pane.
		waiting, waitedFor = true, sched.WaitingPane
	}
	if s.paneTyping[paneID] != "" || mode != firePerson && s.olderWaitOnPaneLocked(id, paneID) {
		// Another automation is typing into this pane, or is ahead of this
		// one for it.
		s.mu.Unlock()
		if mode == firePerson {
			skipped("another automation is being sent to the pane")
			return "", waiting, false
		}
		s.waitForPane(id, paneID, run, waitQueue, mode, due, ident)
		return "", waiting, false
	}
	if s.promptRunning == nil {
		s.promptRunning = map[string]bool{}
	}
	if s.paneTyping == nil {
		s.paneTyping = map[string]string{}
	}
	s.promptRunning[id] = true
	s.paneTyping[paneID] = id
	lastRelease := s.paneLastRelease[paneID]
	s.mu.Unlock()

	if mode != firePerson && waitedFor != "" && ident != "" && ident != waitedFor {
		// The pane was restarted as something else while the prompt waited:
		// the prompt was meant for what ran there before.
		s.endPromptRun(id, paneID)
		s.endPaneWait(id)
		run.Status = "skipped"
		run.Detail = paneWaitGaveUp + "the pane was restarted as a different agent while the prompt waited"
		run.tellPerson = true
		s.recordRun(id, run)
		s.toastGaveUp(s.noticeName(id), run.Detail)
		return "", waiting, false
	}
	why, blocked := paneNotFree(pane, lastRelease, waiting, mode == firePerson)
	if !blocked {
		return ident, waiting, true
	}
	s.endPromptRun(id, paneID)
	if mode == firePerson {
		skipped(why.row)
		return "", waiting, false
	}
	s.waitForPane(id, paneID, run, why, mode, due, ident)
	return "", waiting, false
}

// keepSkippedReminderDue gives a one-time prompt reminder whose time has
// come something to run it, after a Run now that typed nothing left it on
// (recordRun). It usually has that already: its timer, or its wait for the
// pane. One the stop on agent messaging had held does not: Run now released
// the hold and turned it on, its time passed while it was held, and nothing
// would ever run it. It then waits for its pane like any run that came due,
// with why as the row's reason. (If agent messaging is still stopped, the
// next try holds it again, and resume runs it.)
func (s *ScheduleStore) keepSkippedReminderDue(id, why string) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok || !sched.oneTime() || !sched.Enabled || sched.FireCount > 0 || sched.kindLabel() != "prompt" {
		s.mu.Unlock()
		return
	}
	_, hasTimer := s.entryTimers[id]
	_, waiting := s.paneWaits[id]
	if hasTimer || waiting {
		s.mu.Unlock()
		return
	}
	if sched.RunAt.After(s.nowLocked()) {
		s.armOneTimeUnlocked(id, *sched.RunAt)
		s.mu.Unlock()
		return
	}
	due := dueStamp{seq: s.editSeq[id], on: true}
	paneID := sched.PaneID
	s.mu.Unlock()
	ident := ""
	if s.mgr != nil {
		if pane, found := s.mgr.Get(paneID); found {
			ident = paneWaitIdentity(pane)
		}
	}
	s.waitForPane(id, paneID, ScheduleRun{At: time.Now(), PaneID: paneID},
		paneWaitReason{row: why, gaveUp: "the pane was not free"}, fireScheduled, due, ident)
}

// toastGaveUp tells the person in the app that a waiting prompt was not sent.
func (s *ScheduleStore) toastGaveUp(name, detail string) {
	if s.uiBus == nil {
		return
	}
	s.uiBus.publish(uiEvent{
		Type: "toast",
		Text: fmt.Sprintf("⏰ %s %s", name, detail),
	})
}

// noticeName is the automation's name for a toast or phone push. One the
// person never named carries the start of its prompt as its name (fine on
// its own row); a notice says "Automation" instead of showing prompt text.
func (s *ScheduleStore) noticeName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.items[id]
	if !ok {
		return "Automation"
	}
	return noticeNameOf(sched)
}

func noticeNameOf(sched *Schedule) string {
	if sched.Name == "" || sched.Name == defaultName("", sched.Prompt) {
		return "Automation"
	}
	return sched.Name
}

// endPromptRun ends the attempt beginPromptRun admitted.
func (s *ScheduleStore) endPromptRun(id, paneID string) {
	s.mu.Lock()
	delete(s.promptRunning, id)
	if s.paneTyping[paneID] == id {
		delete(s.paneTyping, paneID)
	}
	s.mu.Unlock()
}

// endPaneWait ends an automation's wait for its pane, if it has one.
func (s *ScheduleStore) endPaneWait(id string) {
	s.mu.Lock()
	waiting := s.dropPaneWaitLocked(id)
	if waiting {
		_ = s.saveUnlocked()
	}
	s.mu.Unlock()
	if waiting {
		s.notifyScheduleChanged(id) // the row stops saying "waiting"
	}
}

// resumePaneWaits takes up again, paneWaitResumeDelay after start, the waits
// the last run of the daemon saved (Schedule.WaitingSince), so a restart, an
// update's included, does not lose a run that was waiting for its pane. A
// wait still inside its hour goes on from when it began, through every check
// a retry makes. One past its hour is given up at once, and the person is
// told: before, such a run just vanished (a one-time reminder was recorded
// as "missed" with no notice). The edit count starts again with the daemon,
// so a pause or edit made since the start is seen by the wait being gone.
func (s *ScheduleStore) resumePaneWaits(ids []string) {
	for _, id := range ids {
		s.mu.Lock()
		sched, ok := s.items[id]
		_, already := s.paneWaits[id]
		if !ok || already || !sched.Enabled || sched.WaitingSince == nil || !scheduleAvailable(sched) {
			// Paused, edited, deleted or run since the start, or a run that
			// came due since has taken the saved wait over (waitForPane).
			// One the start-up sweep turned off (past its until, unreadable
			// timetable) keeps no saved wait either.
			if ok && !already && !sched.Enabled && s.dropPaneWaitLocked(id) {
				_ = s.saveUnlocked()
			}
			s.mu.Unlock()
			continue
		}
		now := s.nowLocked()
		since := *sched.WaitingSince
		until := since.Add(paneWaitWindow)
		if !now.Before(until) {
			name, paneID := noticeNameOf(sched), sched.PaneID
			s.dropPaneWaitLocked(id)
			s.mu.Unlock()
			run := ScheduleRun{At: now, PaneID: paneID, Status: "skipped", tellPerson: true,
				Detail: paneWaitGaveUp + "SoluTerminal restarted while the prompt waited for its pane"}
			s.recordRun(id, run) // saves; a one-time is turned off
			s.toastGaveUp(name, run.Detail)
			continue
		}
		if s.paneWaits == nil {
			s.paneWaits = map[string]*paneWait{}
		}
		s.paneWaits[id] = &paneWait{since: since, until: until, due: dueStamp{seq: s.editSeq[id], on: true},
			pane: sched.WaitingPane, paneID: sched.PaneID, why: waitRestart.row}
		name := sched.Name
		s.mu.Unlock()
		log.Printf("scheduler: %q goes on waiting for its pane after the restart", name)
		s.notifyScheduleChanged(id)
	}
	s.retryPaneWaits()
}

// noteRelease records that a scheduled prompt was just typed into the pane.
func (s *ScheduleStore) noteRelease(paneID string) {
	s.mu.Lock()
	if s.paneLastRelease == nil {
		s.paneLastRelease = map[string]time.Time{}
	}
	s.paneLastRelease[paneID] = time.Now()
	s.mu.Unlock()
}

// waitForPane starts or continues a scheduled prompt's wait for its pane,
// and gives it up once paneWaitWindow has passed: the run is recorded as
// skipped with the reason, and the person is told by toast and phone push.
// Runs that came due meanwhile are counted into that one record, not
// queued: the pane gets the prompt once. A pane that stays busy through
// window after window is reported once, not once per window. seq is the
// editSeq read when this run came due: if the person has paused, resumed or
// edited the automation since, the run is over and no wait is started or
// continued for it (the pause already ended any wait there was). The same
// goes for an automation that was on when the run came due and has been
// turned off since by the daemon (a one-time reminder that got its run from
// another attempt): a wait started now would type it a second time. ident
// is the paneWaitIdentity the wait is for.
func (s *ScheduleStore) waitForPane(id, paneID string, run ScheduleRun, why paneWaitReason, mode fireMode, due dueStamp, ident string) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok || s.editSeq[id] != due.seq || due.on && !sched.Enabled {
		s.mu.Unlock()
		return
	}
	now := s.nowLocked()
	w := s.paneWaits[id]
	if w == nil {
		if s.paneWaits == nil {
			s.paneWaits = map[string]*paneWait{}
		}
		// A wait saved before a restart and not taken up yet (a run came
		// due inside paneWaitResumeDelay) is this same wait: its hour began
		// then, and it is for what ran in the pane then.
		since := now
		if sched.WaitingSince != nil {
			since = *sched.WaitingSince
			if sched.WaitingPane != "" {
				ident = sched.WaitingPane
			}
		}
		s.paneWaits[id] = &paneWait{since: since, until: since.Add(paneWaitWindow), due: due, pane: ident, paneID: paneID, why: why.row}
		// Saved, so a restart takes the wait up again (resumePaneWaits).
		sched.WaitingSince, sched.WaitingPane = &since, ident
		_ = s.saveUnlocked()
		name := sched.Name
		s.mu.Unlock()
		log.Printf("scheduler: %q waits for its pane (%s)", name, why.row)
		s.notifyScheduleChanged(id) // the row says "waiting" and why
		return
	}
	changed := w.why != why.row
	w.why = why.row
	if mode == fireScheduled {
		w.later++
	}
	if now.Before(w.until) {
		s.mu.Unlock()
		if changed {
			s.notifyScheduleChanged(id)
		}
		return
	}
	s.dropPaneWaitLocked(id) // recordRun below saves
	name := noticeNameOf(sched)
	// Same busy stretch: the run before this one was given up the same way,
	// and this wait began within a window of it. A give-up days later (a
	// weekly prompt, twice) is a new stretch and is announced again.
	again := len(sched.Runs) > 0 && sched.Runs[0].Status == "skipped" &&
		strings.HasPrefix(sched.Runs[0].Detail, paneWaitGaveUp) &&
		w.since.Sub(sched.Runs[0].At) <= paneWaitWindow
	sched.SkipCount += w.later // recordRun counts this record itself
	s.mu.Unlock()

	run.At = now // when it was given up
	run.Status = "skipped"
	run.Detail = paneWaitGaveUp + why.gaveUp + " for " + fmtWaited(now.Sub(w.since))
	if w.later > 0 {
		run.Detail += fmt.Sprintf(" (%d runs)", w.later+1)
	}
	run.tellPerson = !again
	s.recordRun(id, run)
	if !again {
		s.toastGaveUp(name, run.Detail)
	}
}

// scheduleFiledByAgent: an agent filed it over MCP (create_schedule).
func scheduleFiledByAgent(sched *Schedule) bool {
	return sched.CreatedBy != nil && sched.CreatedBy.Source == "mcp"
}

// releaseAgentMessagingHoldLocked lifts the stop's hold on one schedule: the
// recurring one is armed again. It reports whether the held run is still
// owed (it is, unless a one-time schedule already ran). Caller holds s.mu.
func (s *ScheduleStore) releaseAgentMessagingHoldLocked(sched *Schedule, now time.Time) bool {
	if sched.PausedReason != agentMessagingScheduleReason {
		return false
	}
	sched.PausedReason = ""
	if sched.oneTime() {
		if sched.FireCount > 0 {
			return false
		}
		// Its moment passed while held: it runs now, and the run disables it.
		sched.Enabled = true
		return true
	}
	s.unscheduleUnlocked(sched.ID)
	sched.Enabled = true
	sched.LastArmedAt = &now
	if err := s.scheduleUnlocked(sched); err != nil {
		sched.Enabled = false
		log.Printf("scheduler: could not re-arm %q after agent messaging resumed: %v", sched.Name, err)
		return false
	}
	return true
}

// resumeAgentMessagingHeld runs, once each, the agents' schedules held while
// agent messaging was paused, after delay, and turns the recurring ones back
// on. At boot the delay gives panes still being restored time to come back.
func (s *ScheduleStore) resumeAgentMessagingHeld(delay time.Duration) {
	if delay > 0 {
		// The hold stays recorded until the release itself, so a restart in
		// between loses no owed run.
		time.AfterFunc(delay, func() {
			if !agentMessagingPaused() {
				s.resumeAgentMessagingHeld(0)
			}
		})
		return
	}
	s.mu.Lock()
	var due, changed []string
	now := time.Now()
	for _, id := range s.order {
		sched := s.items[id]
		if sched == nil || sched.PausedReason != agentMessagingScheduleReason || !scheduleAvailable(sched) {
			continue
		}
		changed = append(changed, id)
		if s.releaseAgentMessagingHoldLocked(sched, now) {
			due = append(due, id)
		}
	}
	if len(changed) > 0 {
		_ = s.saveUnlocked()
	}
	s.mu.Unlock()
	for _, id := range changed {
		s.notifyScheduleChanged(id)
	}
	for _, id := range due {
		go s.fire(id)
	}
}

// fmtWaited words a wait for a run detail: "60 min", "45 s".
func fmtWaited(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}

// retryPaneWaits tries again the oldest waiting prompt of each pane: a pane
// takes one prompt per quiet period (paneNotFree), so the others would only
// be turned away. Each pane's attempt goes on its own goroutine, as cron's
// runs do, so one slow pane holds up no other.
func (s *ScheduleStore) retryPaneWaits() {
	s.mu.Lock()
	oldest := map[string]string{} // pane id → the automation waiting longest
	for id, w := range s.paneWaits {
		cur, seen := oldest[w.paneID]
		if !seen || w.since.Before(s.paneWaits[cur].since) || w.since.Equal(s.paneWaits[cur].since) && id < cur {
			oldest[w.paneID] = id
		}
	}
	s.mu.Unlock()
	for _, id := range oldest {
		go s.fireAs(id, fireRetry)
	}
}

// waitingForPane reports a scheduled prompt waiting for its pane.
func (s *ScheduleStore) waitingForPane(id string) bool {
	_, ok := s.paneWaitReason(id)
	return ok
}

// paneWaitReason is why a scheduled prompt is waiting for its pane, for its
// row ("you are typing in the pane"); ok is false when it is not waiting.
func (s *ScheduleStore) paneWaitReason(id string) (why string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.paneWaits[id]
	if !ok {
		return "", false
	}
	return w.why, true
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
	if !ok || !scheduleAvailable(sched) {
		return
	}
	s.unscheduleUnlocked(id)
	sched.Enabled = false
	sched.PausedReason = reason
	sched.noteChange(scheduleChangeDisabled, scheduleActorDaemon, reason, time.Now())
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
	if !ok || !scheduleAvailable(sched) {
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
	if run.tellPerson {
		// A notice the daemon raises by itself: never the start of the
		// prompt as the title (noticeNameOf).
		name = noticeNameOf(sched)
	}
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
	// The one exception: the person's Run now that typed nothing (the pane
	// was busy, they were typing). That was not the reminder's attempt: it
	// stays set for its time, or waiting for its pane, as it was. Before,
	// "Skipped: the pane is busy" also meant the reminder never came.
	runNowSkip := run.byRunNow && run.Status == "skipped"
	if sched.oneTime() && runNowSkip {
		// Left as it was.
	} else if sched.oneTime() {
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
	//   given up on a busy pane → also "schedule-error" (run.tellPerson):
	//                           the prompt waited paneWaitWindow and was
	//                           not sent.
	//   successful run        → "schedule-fired", per-schedule opt-in.
	// Other recurring skips (snoozed, Run now on a busy pane) stay quiet.
	if pusher != nil {
		switch {
		case runNowSkip:
			// The person is at the screen and was told there.
		case run.Status == "error" || (run.Status == "skipped" && (wasOneTime || run.tellPerson)):
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
