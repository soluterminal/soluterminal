package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"soluterminal-shared/providers"
)

// preFireLeadTime is how far ahead we warn the user that a schedule is
// about to fire. The web side (desktop-only) translates the corresponding
// uiBus event into a spoken "scheduled task firing in five minutes" alert.
const preFireLeadTime = 5 * time.Minute

// Schedule is a prompt-injection job. When its trigger fires, the
// scheduler writes the prompt (plus a trailing \r to mimic pressing
// Enter) into the target pane's PTY. Used primarily to drive Claude
// Code sessions on a timer — "every morning, summarize open PRs",
// "remind me in 30 minutes" — but the primitive is generic: any
// pane's PTY is a valid target.
//
// Two trigger kinds, mutually exclusive:
//   - Cron: recurring. Fires on every cron match until disabled or
//     until an end condition (Until / MaxFires) is hit.
//   - RunAt: one-time. Fires once at the given instant, then the
//     schedule auto-disables regardless of fire outcome.
//
// Fire semantics:
//   - We skip if the pane is busy (still running a tool), in a choice-
//     prompt (numeric y/n dialog), or exited. Otherwise we fire. That
//     gates out the dangerous cases (corrupting a tool-call in flight,
//     clobbering an unanswered confirmation) without requiring
//     elaborate per-state policy.
//   - We don't catch up missed fires after downtime. If the daemon was
//     off when the schedule would have fired, that fire is lost.
//     Better than accidentally running yesterday's daily 7x at 3am.
//     One-time schedules whose RunAt is already past on load record a
//     "skipped" run with reason "missed while daemon was down" and
//     auto-disable.
type Schedule struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Cron     string     `json:"cron,omitempty"`     // 5-field cron — empty for one-time
	RunAt    *time.Time `json:"runAt,omitempty"`    // one-time fire — nil for recurring
	Until    *time.Time `json:"until,omitempty"`    // recurring: stop after this instant (nil = forever)
	MaxFires int        `json:"maxFires,omitempty"` // recurring: stop after N successful fires (0 = unlimited)
	// SnoozeUntil suppresses exactly one upcoming recurring fire — set
	// by the sidebar 💤 button to (nextFire + 1s) so a single
	// occurrence is skipped with a "snoozed" status, then cleared.
	// Cheap alternative to disable → re-enable when the user knows a
	// specific fire would collide with in-progress work.
	SnoozeUntil *time.Time `json:"snoozeUntil,omitempty"`
	// Kind selects the work performed on fire. "" / "prompt" (legacy files
	// have no kind) = submit Prompt to PaneID (semantic provider input for a
	// Focus pane, PTY input otherwise). "agent" = run the
	// built-in local agent AgentID as a detached child process (see
	// schedules_agent.go) — PaneID/Prompt are empty for agent schedules.
	// "task" = run a fresh headless Claude session on Prompt in Cwd (see
	// schedules_task.go) — PaneID is empty for task schedules.
	Kind    string `json:"kind,omitempty"`
	AgentID string `json:"agentId,omitempty"` // kind=agent: built-in registry ID
	// OwnedByAgentID is the agt_ id of the managed agent this schedule was
	// IMPORTED into (docs/DESIGN-agents-surface.md §8.3 Phase 1). Set only by
	// ManagedAgentStore.ImportSchedule and cleared only by its rollback
	// (ManagedAgentStore.Delete → ClearOwnedBy).
	//
	// It changes exactly one thing about the schedule: the row is hidden from
	// the Schedules pane and the MCP schedule tools, because the agent card now
	// owns pause/run/history for it. The schedule itself keeps its id, its
	// cron entry, its prompt, its logs and its counters, and keeps firing
	// through the identical path — that is what makes the import
	// behavior-neutral and the rollback a single DELETE.
	//
	// Deliberately absent from ScheduleInput: Update preserves it, and no
	// client can assign one.
	OwnedByAgentID string `json:"ownedByAgentId,omitempty"`
	// CreatedBy is CREATOR provenance — who filed this schedule — and is
	// nothing to do with PaneID below, which for kind=prompt is the TARGET
	// pane the prompt gets injected into (and which validate() rejects
	// outright for kind=task/agent). An orchestrator pane that files a
	// nightly task schedule is the createdBy; the schedule has no target.
	//
	// Same precedent as OwnedByAgentID: absent from ScheduleInput so no
	// client can author one, preserved across Update, nil for HTTP-created
	// and legacy schedules. It changes nothing about firing.
	CreatedBy *ScheduleOrigin `json:"createdBy,omitempty"`
	// kind=task fields. Permission is a preset name ("plan" | "edit"), never
	// a raw claude flag. TimeoutMin 0 = the 30-minute default.
	Cwd        string `json:"cwd,omitempty"`
	Permission string `json:"permission,omitempty"`
	Model      string `json:"model,omitempty"`
	TimeoutMin int    `json:"timeoutMin,omitempty"`
	// Apply passes the agent's write-mode flag (--apply). Off = dry-run:
	// the agent plans and writes previews but posts/pushes nothing.
	Apply  bool   `json:"apply,omitempty"`
	PaneID string `json:"paneId,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// NotifyOnRun sends a phone/OS push on every successful run (state
	// "schedule-fired"). Errors always push regardless — a nightly job
	// failing while nobody watches is exactly what push exists for. The
	// web dialog defaults this ON for one-time reminders (a reminder
	// that can't reach you isn't a reminder) and OFF for recurring.
	NotifyOnRun bool `json:"notifyOnRun,omitempty"`
	// DeliverChat posts each successful task run's report into a chat through
	// an optional delivery feature (scheduleChatDelivery, extensions.go).
	// Task kind only — it's the report that gets delivered.
	DeliverChat *ScheduleChatRef `json:"deliverChat,omitempty"`
	// CatchUp: if recurring runs were missed while the daemon was off,
	// run ONCE shortly after startup (not once per missed fire — the
	// no-catch-up policy still holds; this is "run the nightly report I
	// missed", not "replay three days of them"). Task-kind recurring
	// only: task runs need no pane, so they can't race the workspace
	// restore that is still in flight at boot.
	CatchUp bool `json:"catchUp,omitempty"`
	Enabled bool `json:"enabled"`
	// PausedReason says why the DAEMON paused this schedule (today: its
	// target pane is gone), so the row can explain itself instead of failing
	// and paging on every fire. Empty for a schedule the user paused. Cleared
	// when the schedule is turned back on or retargeted.
	PausedReason string        `json:"pausedReason,omitempty"`
	CreatedAt    time.Time     `json:"createdAt"`
	Runs         []ScheduleRun `json:"runs,omitempty"` // last N runs (newest first)

	// Lifetime counters — every recordRun bumps the one matching the
	// run's status. Separate from `Runs` because the run-list is capped
	// at 20, so a long-running schedule loses older entries; the
	// counters survive forever. Persisted with the schedule file.
	FireCount   int        `json:"fireCount"`
	SkipCount   int        `json:"skipCount"`
	ErrorCount  int        `json:"errorCount"`
	LastFiredAt *time.Time `json:"lastFiredAt,omitempty"`
	// LastArmedAt is when the schedule last flipped disabled→enabled.
	// Anchors the boot-time missed-fire count so time spent deliberately
	// paused is never reported as "missed while the app was off".
	LastArmedAt *time.Time `json:"lastArmedAt,omitempty"`
	// LastReadAt is the newest run timestamp the user has SEEN in the
	// reports reader. Daemon-side so read state follows the user across
	// devices — reading the digest on the phone clears the unread dot on
	// the desktop. Only meaningful for task-kind schedules (reports).
	LastReadAt *time.Time `json:"lastReadAt,omitempty"`
}

// ScheduleOrigin records WHO FILED a schedule — the pane an agent was talking
// in when it called create_schedule. It is deliberately not the same thing as
// Schedule.PaneID, which for kind=prompt is the pane the prompt gets delivered
// TO (and which validate() rejects for kind=task/agent). PaneTitle is the
// creator pane's header title snapshotted at create time; PaneID stays the
// durable identity.
type ScheduleOrigin struct {
	Source    string `json:"source,omitempty"`
	PaneID    string `json:"paneId,omitempty"`
	PaneTitle string `json:"paneTitle,omitempty"`
}

// oneTime reports whether this schedule is a one-time run-at.
func (s *Schedule) oneTime() bool { return s.RunAt != nil }

// missedFireCountCap bounds the countMissedFires walk — a 1-minute cron
// off for a month would otherwise iterate ~43k times at every boot for a
// number nobody reads past "a lot".
const missedFireCountCap = 500

// countMissedFires reports how many fires a recurring schedule would have
// had between its last observed activity and now — i.e. fires lost while
// the daemon was down. Returns 0 for unparseable crons (the load sweep
// disables those separately) and clamps at missedFireCountCap.
func countMissedFires(sched *Schedule, now time.Time) int {
	anchor := sched.CreatedAt
	if sched.LastFiredAt != nil && sched.LastFiredAt.After(anchor) {
		anchor = *sched.LastFiredAt
	}
	if sched.LastArmedAt != nil && sched.LastArmedAt.After(anchor) {
		anchor = *sched.LastArmedAt
	}
	if len(sched.Runs) > 0 && sched.Runs[0].At.After(anchor) {
		anchor = sched.Runs[0].At
	}
	if anchor.IsZero() || !anchor.Before(now) {
		return 0
	}
	expr, err := cronParser.Parse(sched.Cron)
	if err != nil {
		return 0
	}
	missed := 0
	cursor := anchor
	for missed < missedFireCountCap {
		next := expr.Next(cursor)
		if next.IsZero() || !next.Before(now) {
			break
		}
		missed++
		cursor = next
	}
	return missed
}

// isTask reports whether this schedule runs a fresh headless Claude session.
func (s *Schedule) isTask() bool { return s.Kind == "task" }

// scheduleRunning reports a daemon-started run of sched in flight (a task
// run, or a run of a registered kind).
func (s *ScheduleStore) scheduleRunning(sched Schedule) bool {
	if sched.isTask() {
		return s.taskRunningFor(sched.ID)
	}
	if kind, ok := scheduleKinds[sched.Kind]; ok && kind.running != nil {
		return kind.running(s, sched)
	}
	return false
}

const (
	// agentKindSchedule is the hidden schedule kind backing a managed agent's
	// cron trigger; agentTriggerCron is the trigger context its runs report.
	agentKindSchedule = "agent-run"
	agentTriggerCron  = "cron"
)

// isAgentRun reports whether this schedule is the HIDDEN trigger backing a
// managed agent's cron entry (docs/DESIGN-agents-surface.md §5.2). AgentID
// carries the agt_ id. These are minted only by the ManagedAgentStore — the public
// schedules API and the MCP schedule tools refuse the kind — so the Schedules
// pane never shows a row the user cannot meaningfully edit.
func (s *Schedule) isAgentRun() bool { return s.Kind == agentKindSchedule }

type ScheduleRun struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"` // "fired" | "skipped" | "error"
	Detail string    `json:"detail,omitempty"`
	// PaneID is the pane a prompt-kind run delivered into — lets the run
	// history offer "go to pane". Empty for agent runs and early failures.
	PaneID string `json:"paneId,omitempty"`
	// LogFile is the captured-output file (basename under the agent
	// home's runs/ dir) for agent-kind runs — served by
	// GET /api/schedules/{id}/runlog. Empty for prompt runs.
	LogFile string `json:"logFile,omitempty"`
	// Summary is the human lede of a successful task run — the first few
	// lines of Claude's output. It rides the notifyOnRun push as the body
	// (the report reaches the phone, not just "it ran") and gives the UI
	// something readable without opening the full log.
	Summary string `json:"summary,omitempty"`
}

const maxRunHistory = 20

// cronParser accepts the standard 5-field syntax. Seconds are not
// supported — scheduling at sub-minute granularity would be abusive
// anyway (we inject into a human-facing terminal).
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ScheduleStore owns all schedules: persistence, the cron scheduler
// goroutine, and the fire wiring. Lives for the daemon's lifetime.
type ScheduleStore struct {
	mu       sync.Mutex
	path     string
	items    map[string]*Schedule
	order    []string // insertion order for stable listing
	cron     *cron.Cron
	entryIDs map[string]cron.EntryID // schedule ID → cron entry ID (for Remove)
	// One-time schedules use time.Timer instead of the cron library —
	// cron can't represent "this exact instant, once" (any cron that
	// matches the specific minute would also fire next year on the
	// same date).
	entryTimers map[string]*time.Timer
	mgr         *Manager
	uiBus       *uiBus

	// Pre-fire warning dedup. Keyed on schedule ID → the NextFire time
	// we last warned about, so the watchPreFire ticker only publishes
	// one "firing-soon" event per occurrence even though the schedule
	// stays in the warning window for ~preFireLeadTime.
	preFireWarnedAt map[string]time.Time
	preFireCancel   context.CancelFunc

	// agentRunning tracks in-flight daemon-spawned agent runs by agent ID
	// (not schedule ID: two schedules targeting the same agent must not
	// overlap). See schedules_agent.go.
	agentRunning map[string]bool

	// taskRunning tracks in-flight headless-Claude task runs, keyed by
	// SCHEDULE id (unlike agentRunning: two task schedules are independent
	// even in the same directory). It bounds global fan-out to
	// resources.maxAgentRuns + resources.reservedBookkeepingRuns (run_slots.go).
	//
	// Managed-agent runs share this map under an "agent:<agentID>" key
	// (acquireAgentRunSlot, schedules_task.go), which is what makes the global
	// cap ONE counter across both spawned kinds rather than two of the same
	// size — docs/DESIGN-agents-surface.md §5.2-5.3.
	taskRunning map[string]bool
	// taskReserved is the subset of taskRunning keys holding a
	// bookkeeping-reserved slot (run_slots.go); general in-flight is
	// len(taskRunning)-len(taskReserved).
	taskReserved map[string]bool

	// runCaps is installed by main.go via SetRunCapsResolver. It is called
	// UNDER s.mu and takes settingsFileStore.mu inside. The lock order is safe:
	// settingsFileStore never calls back into ScheduleStore. A nil resolver (the
	// SOLU_WORKFLOW_DISABLE=1 path and tests) uses env/default caps.
	runCaps func() agentRunCaps

	// ext holds optional features' state on the store (extSlot, extensions.go).
	ext extState

	// pusher delivers schedule-error / schedule-fired phone pushes. Set by
	// SetPusher when cloud creds arrive, cleared on disconnect — nil means
	// no cloud, pushes silently skipped.
	pusher *Pusher

	// metrics counts creates and runs by kind/status (IncLabelled is
	// nil-safe, but the pointer itself is guarded by mu because it's set
	// after the store — and its cron — is already live).
	metrics *Metrics

	// Cloud reminder mirroring (see schedules_reminders.go). backendID
	// disambiguates multiple daemons on one machine claim; the timer is
	// the sync debounce.
	backendID         string
	reminderSyncTimer *time.Timer

	// agentRunner starts a managed-agent run when a hidden Kind=="agent-run"
	// schedule fires (agents_run.go). Installed by main.go via SetAgentRunner
	// so the scheduler holds a function, not the agent store — the runner is
	// the ONLY thing this file knows about agents. Nil = no agent store on
	// this daemon; a fire then records an error rather than panicking.
	agentRunner func(agentID, triggerContext string) (string, string)

	// ownedRunGate and ownedRunMirror are the two hooks an IMPORTED schedule
	// (OwnedByAgentID != "") runs through, installed by main.go the same way
	// agentRunner is — functions, not the store, so this file still knows
	// nothing about agents beyond three callbacks.
	//
	// The gate answers "may this fire proceed?" with the agent's own pause
	// walls; the mirror copies each recorded outcome onto the agent's card.
	// Both are called OFF s.mu: they reach into the ManagedAgentStore, which reaches
	// back here, and holding both mutexes in either order is the deadlock.
	ownedRunGate   func(agentID string) (bool, string)
	ownedRunMirror func(agentID string, run ScheduleRun)

	// lifeCtx ends when the store stops; a task run waiting out its one
	// retry (schedules_limits.go) gives up on it instead of firing into a
	// dying daemon.
	lifeCtx context.Context

	// usageLimitUntil is when the Claude subscription's usage limit resets,
	// learned from the last task run that hit it. Task runs before then are
	// skipped instead of spending a spawn on "You've hit your weekly limit".
	// Memory only: after a restart the next run re-learns it.
	usageLimitUntil time.Time

	// lastReminderSync is the payload of the last reminder sync the cloud
	// accepted, and when; an unchanged set is not re-sent until
	// reminderResyncEvery has passed (schedules_reminders.go).
	lastReminderSync   string
	lastReminderSyncAt time.Time

	// editSeq counts the user's pauses, resumes and edits per schedule
	// (SetEnabled, Update). The daemon's own re-arm of a one-time task past a
	// usage-limit reset checks it under s.mu, so it can never undo a pause or
	// edit that landed after it decided (deferOneTime).
	editSeq map[string]uint64
}

// bumpEditUnlocked records a user pause/resume/edit. Caller holds s.mu.
func (s *ScheduleStore) bumpEditUnlocked(id string) {
	if s.editSeq == nil {
		s.editSeq = map[string]uint64{}
	}
	s.editSeq[id]++
}

// editStamp snapshots whether a schedule is on and its edit count, together.
func (s *ScheduleStore) editStamp(id string) (enabled bool, seq uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.items[id]
	if !ok {
		return false, 0, false
	}
	return sched.Enabled, s.editSeq[id], true
}

// SetAgentRunner installs the managed-agent run hook. Safe for concurrent use.
func (s *ScheduleStore) SetAgentRunner(fn func(agentID, triggerContext string) (string, string)) {
	s.mu.Lock()
	s.agentRunner = fn
	s.mu.Unlock()
}

// SetRunCapsResolver installs the live run-pool settings resolver.
func (s *ScheduleStore) SetRunCapsResolver(fn func() agentRunCaps) {
	s.mu.Lock()
	s.runCaps = fn
	s.mu.Unlock()
}

func (s *ScheduleStore) runCapsUnlocked() agentRunCaps {
	if s.runCaps != nil {
		return s.runCaps()
	}
	return defaultAgentRunCaps()
}

// SetOwnedRunGate installs the pause gate imported schedules fire through
// (docs/DESIGN-agents-surface.md §8.3). Safe for concurrent use.
func (s *ScheduleStore) SetOwnedRunGate(fn func(agentID string) (bool, string)) {
	s.mu.Lock()
	s.ownedRunGate = fn
	s.mu.Unlock()
}

// SetOwnedRunMirror installs the card mirror for imported schedules' run
// outcomes. Safe for concurrent use.
func (s *ScheduleStore) SetOwnedRunMirror(fn func(agentID string, run ScheduleRun)) {
	s.mu.Lock()
	s.ownedRunMirror = fn
	s.mu.Unlock()
}

// SetPusher installs (or clears, with nil) the cloud push relay used for
// schedule lifecycle notifications. Safe for concurrent use.
func (s *ScheduleStore) SetPusher(p *Pusher) {
	s.mu.Lock()
	s.pusher = p
	s.lastReminderSync = "" // a new connection gets the set even if unchanged
	s.mu.Unlock()
	// Fresh cloud connection → upload the current reminder set so the
	// cloud safety net covers reminders created while offline.
	if p != nil {
		s.scheduleReminderSync()
	}
}

// SetMetrics installs the metrics sink. Safe for concurrent use.
func (s *ScheduleStore) SetMetrics(m *Metrics) {
	s.mu.Lock()
	s.metrics = m
	s.mu.Unlock()
}

// kindLabel normalizes the empty legacy kind for metric labels.
func (s *Schedule) kindLabel() string {
	if s.Kind == "" {
		return "prompt"
	}
	return s.Kind
}

func NewScheduleStore(dataDir string, mgr *Manager, bus *uiBus) (*ScheduleStore, error) {
	s := &ScheduleStore{
		path:            filepath.Join(dataDir, "schedules.json"),
		items:           map[string]*Schedule{},
		entryIDs:        map[string]cron.EntryID{},
		entryTimers:     map[string]*time.Timer{},
		preFireWarnedAt: map[string]time.Time{},
		agentRunning:    map[string]bool{},
		taskRunning:     map[string]bool{},
		taskReserved:    map[string]bool{},
		mgr:             mgr,
		uiBus:           bus,
		// Use the local timezone so "every morning at 9" matches the
		// user's wall clock. Users in two-timezone setups can use
		// per-expression TZ syntax ("CRON_TZ=UTC 0 9 * * *").
		cron: cron.New(cron.WithLocation(time.Local), cron.WithParser(cronParser)),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	now := time.Now()
	var catchUpIDs []string
	for _, id := range s.order {
		sched := s.items[id]
		if !sched.Enabled {
			continue
		}
		// One-time schedules whose moment passed while the daemon was
		// off are recorded as missed and auto-disabled — mirrors the
		// cron "don't catch up" policy.
		if sched.oneTime() && sched.RunAt.Before(now) {
			log.Printf("scheduler: one-time %q (%s) missed while daemon was down (runAt=%s)", sched.Name, sched.ID, sched.RunAt.Format(time.RFC3339))
			sched.Enabled = false
			sched.Runs = append([]ScheduleRun{{At: now, Status: "skipped", Detail: "missed while daemon was down"}}, sched.Runs...)
			sched.SkipCount++
			if len(sched.Runs) > maxRunHistory {
				sched.Runs = sched.Runs[:maxRunHistory]
			}
			continue
		}
		// Recurring schedules whose Until already passed are also
		// auto-disabled. Saves the user a click and avoids logging
		// "stopped, past Until" on every would-be fire.
		if !sched.oneTime() && sched.Until != nil && !sched.Until.After(now) {
			log.Printf("scheduler: recurring %q (%s) past until=%s, disabling", sched.Name, sched.ID, sched.Until.Format(time.RFC3339))
			sched.Enabled = false
			continue
		}
		// Missed-run visibility: recurring fires lost to downtime used to
		// vanish without a trace (the deliberate no-catch-up policy made
		// the gap invisible, not just un-replayed). Record ONE synthetic
		// skipped run summarizing the gap. Anchored on the newest of
		// (last fire, last recorded run, creation) so a restart right
		// after this sweep doesn't re-count the same gap.
		if !sched.oneTime() {
			if missed := countMissedFires(sched, now); missed > 0 {
				detail := fmt.Sprintf("%d run%s missed while the app was off", missed, plural(missed))
				if missed >= missedFireCountCap {
					detail = fmt.Sprintf("%d+ runs missed while the app was off", missedFireCountCap)
				}
				if sched.CatchUp && sched.isTask() {
					detail += " — catching up with one run"
					catchUpIDs = append(catchUpIDs, sched.ID)
				}
				sched.Runs = append([]ScheduleRun{{At: now, Status: "skipped", Detail: detail}}, sched.Runs...)
				if len(sched.Runs) > maxRunHistory {
					sched.Runs = sched.Runs[:maxRunHistory]
				}
				sched.SkipCount += missed
				log.Printf("scheduler: recurring %q (%s): %s", sched.Name, sched.ID, detail)
			}
		}
		if err := s.scheduleUnlocked(sched); err != nil {
			// Bad cron expression on disk — disable so the user
			// sees a problem in the UI instead of a silent no-op.
			log.Printf("scheduler: disabling %q (%s): %v", sched.Name, sched.ID, err)
			sched.Enabled = false
		}
	}
	// Persist any auto-disables from the load sweep so the UI reflects
	// reality on first fetch.
	_ = s.saveUnlocked()
	s.cron.Start()

	ctx, cancel := context.WithCancel(context.Background())
	s.preFireCancel = cancel
	s.lifeCtx = ctx
	go s.watchPreFire(ctx)

	// Catch-up fires, one per opted-in task schedule that missed runs.
	// Delayed so a boot storm (workspace restore, cloud reconnect) settles
	// first, and cancellable via the same context as the pre-fire watcher
	// so Stop() during the delay never fires into a dying process. fire()
	// re-checks every gate (enabled, until, snooze, single-flight).
	if len(catchUpIDs) > 0 {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(catchUpDelay):
			}
			for _, id := range catchUpIDs {
				if ctx.Err() != nil {
					return
				}
				// Re-check enabled: the user may have paused it during the
				// delay, and fire() itself doesn't gate on Enabled (normally
				// only wired cron entries reach it).
				if sc, ok := s.Get(id); !ok || !sc.Enabled {
					continue
				}
				log.Printf("scheduler: catch-up run for %s", id)
				s.fire(id)
			}
		}()
	}

	return s, nil
}

// catchUpDelay is how long after boot the missed-run catch-up fires. A var,
// not a const, so tests can shrink it.
var catchUpDelay = 20 * time.Second

func (s *ScheduleStore) Stop() {
	if s == nil {
		return
	}
	if s.preFireCancel != nil {
		s.preFireCancel()
	}
	ctx := s.cron.Stop() // returns context.Context that cancels once running jobs finish
	<-ctx.Done()
}

// Get returns a snapshot of one schedule by ID.
func (s *ScheduleStore) Get(id string) (Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sched, ok := s.items[id]; ok {
		return *sched, true
	}
	return Schedule{}, false
}

// List returns a snapshot of all schedules in insertion order. Safe for
// concurrent callers.
func (s *ScheduleStore) List() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, 0, len(s.order))
	for _, id := range s.order {
		if sched, ok := s.items[id]; ok {
			out = append(out, *sched)
		}
	}
	return out
}

// NextFire returns the next scheduled execution time for a given
// schedule ID, or the zero time if it isn't currently scheduled
// (disabled, deleted, or bad cron). For one-time schedules the "next"
// fire is simply RunAt.
func (s *ScheduleStore) NextFire(id string) time.Time {
	s.mu.Lock()
	// Copy the schedule VALUE under the lock (the Get/List idiom) — the
	// items map holds pointers that SetEnabled/Update mutate in place, so
	// reading fields off the pointer after Unlock is a data race (caught
	// by TestScheduleStoreConcurrentAccess under -race).
	var sched Schedule
	ptr, hasItem := s.items[id]
	if hasItem {
		sched = *ptr
	}
	entryID, hasEntry := s.entryIDs[id]
	_, hasTimer := s.entryTimers[id]
	s.mu.Unlock()
	if !hasItem || !sched.Enabled {
		return time.Time{}
	}
	if sched.oneTime() {
		if hasTimer && sched.RunAt != nil {
			return *sched.RunAt
		}
		return time.Time{}
	}
	if !hasEntry {
		return time.Time{}
	}
	e := s.cron.Entry(entryID)
	if e.ID == 0 {
		return time.Time{}
	}
	return e.Next
}

type ScheduleInput struct {
	Name     string     `json:"name"`
	Cron     string     `json:"cron,omitempty"`
	RunAt    *time.Time `json:"runAt,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
	MaxFires int        `json:"maxFires,omitempty"`
	Kind     string     `json:"kind,omitempty"`    // "" | "prompt" | "agent"
	AgentID  string     `json:"agentId,omitempty"` // kind=agent only
	Apply    bool       `json:"apply,omitempty"`   // kind=agent only
	PaneID   string     `json:"paneId,omitempty"`
	Prompt   string     `json:"prompt,omitempty"`
	// kind=task only.
	Cwd         string           `json:"cwd,omitempty"`
	Permission  string           `json:"permission,omitempty"`
	Model       string           `json:"model,omitempty"`
	TimeoutMin  int              `json:"timeoutMin,omitempty"`
	NotifyOnRun bool             `json:"notifyOnRun,omitempty"`
	DeliverChat *ScheduleChatRef `json:"deliverChat,omitempty"` // kind=task only
	CatchUp     bool             `json:"catchUp,omitempty"`     // kind=task recurring only
	Enabled     *bool            `json:"enabled,omitempty"`     // nil = true
}

func (in *ScheduleInput) validate() error {
	if providers.HasRetiredDeepSeekReference(in.Model) {
		return errors.New("schedule model cannot reference a retired DeepSeek model")
	}
	cronExpr := strings.TrimSpace(in.Cron)
	hasCron := cronExpr != ""
	hasRunAt := in.RunAt != nil
	if hasCron == hasRunAt {
		return errors.New("exactly one of cron or runAt is required")
	}
	if hasCron {
		if _, err := cronParser.Parse(cronExpr); err != nil {
			return fmt.Errorf("invalid cron expression: %w", err)
		}
		if in.MaxFires < 0 {
			return errors.New("maxFires must be >= 0")
		}
		if in.Until != nil && !in.Until.After(time.Now()) {
			return errors.New("until must be in the future")
		}
	} else {
		// One-time constraints: end-condition fields are recurring-only,
		// and the moment must still be in the future (we don't replay
		// past fires — see the "no catch-up" policy in the type comment).
		if in.MaxFires != 0 {
			return errors.New("maxFires only applies to recurring schedules")
		}
		if in.Until != nil {
			return errors.New("until only applies to recurring schedules")
		}
		if !in.RunAt.After(time.Now()) {
			return errors.New("runAt must be in the future")
		}
	}
	switch strings.TrimSpace(in.Kind) {
	case "", "prompt":
		if strings.TrimSpace(in.PaneID) == "" {
			return errors.New("paneId is required")
		}
		if strings.TrimSpace(in.Prompt) == "" {
			return errors.New("prompt is required")
		}
	case "task":
		if strings.TrimSpace(in.Prompt) == "" {
			return errors.New("prompt is required")
		}
		cwd := strings.TrimSpace(in.Cwd)
		if cwd == "" {
			return errors.New("cwd is required")
		}
		if !filepath.IsAbs(cwd) {
			return errors.New("cwd must be an absolute path")
		}
		// Existence is checked here (a typo'd path should fail at save time,
		// not silently at 3am) AND again at fire time (the directory can be
		// deleted in between).
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return errors.New("cwd is not an existing directory")
		}
		if !validTaskPermission(strings.TrimSpace(in.Permission)) {
			return errors.New(`permission must be "plan" or "edit"`)
		}
		if in.TimeoutMin < 0 || time.Duration(in.TimeoutMin)*time.Minute > taskRunTimeoutMax {
			return fmt.Errorf("timeoutMin must be between 0 and %d", int(taskRunTimeoutMax.Minutes()))
		}
		if strings.TrimSpace(in.PaneID) != "" {
			return errors.New("task schedules take no paneId")
		}
	case agentKindSchedule:
		// Hidden agent triggers are minted by the ManagedAgentStore alone — a client
		// that could create one could give itself a cron-fired headless run
		// with no agent record, and therefore no card, behind it.
		return errors.New("agent-run schedules are managed via /api/agents")
	default:
		kind, ok := scheduleKinds[strings.TrimSpace(in.Kind)]
		if !ok || kind.validate == nil {
			return errors.New("kind must be " + scheduleKindChoices())
		}
		if err := kind.validate(*in); err != nil {
			return err
		}
	}
	if in.CatchUp {
		if strings.TrimSpace(in.Kind) != "task" {
			return errors.New("catchUp applies to Claude-task schedules only")
		}
		if in.RunAt != nil {
			return errors.New("catchUp applies to recurring schedules only")
		}
	}
	if in.DeliverChat != nil {
		if strings.TrimSpace(in.Kind) != "task" {
			return errors.New("deliverChat applies to Claude-task schedules only")
		}
		if strings.TrimSpace(in.DeliverChat.Source) == "" || strings.TrimSpace(in.DeliverChat.ChatID) == "" {
			return errors.New("deliverChat needs source and chatId")
		}
	}
	return nil
}

// scheduleName resolves the display name: explicit > the kind's title (a
// registered kind's, e.g. an agent's) > first prompt line (defaultName).
func scheduleName(in ScheduleInput) string {
	if strings.TrimSpace(in.Name) == "" {
		if kind, ok := scheduleKinds[strings.TrimSpace(in.Kind)]; ok && kind.title != nil {
			if title := kind.title(in); title != "" {
				return title
			}
		}
	}
	return defaultName(in.Name, in.Prompt)
}

// scheduleKindChoices lists the kinds a client may create, for the
// validation error: "prompt", the registered kinds, then "task".
func scheduleKindChoices() string {
	kinds := []string{`"prompt"`}
	for _, name := range scheduleKindOrder {
		if scheduleKinds[name].validate != nil {
			kinds = append(kinds, `"`+name+`"`)
		}
	}
	kinds = append(kinds, `"task"`)
	if len(kinds) == 2 {
		return kinds[0] + " or " + kinds[1]
	}
	return strings.Join(kinds[:len(kinds)-1], ", ") + ", or " + kinds[len(kinds)-1]
}

// Create adds a new schedule and, if enabled, wires it into the cron. The
// resulting schedule carries no creator provenance — the HTTP API and the
// dialog behind it have no pane to attribute. See CreateWithOrigin.
func (s *ScheduleStore) Create(in ScheduleInput) (*Schedule, error) {
	return s.CreateWithOrigin(in, nil)
}

// CreateWithOrigin is Create with creator provenance attached (Schedule.CreatedBy).
// Only the MCP tool has a caller pane to name, so only it passes a non-nil
// origin; nothing about validation or firing changes either way.
func (s *ScheduleStore) CreateWithOrigin(in ScheduleInput, createdBy *ScheduleOrigin) (*Schedule, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	sched := &Schedule{
		ID:          generateScheduleID(),
		Name:        scheduleName(in),
		Cron:        strings.TrimSpace(in.Cron),
		RunAt:       in.RunAt,
		Until:       in.Until,
		MaxFires:    in.MaxFires,
		Kind:        strings.TrimSpace(in.Kind),
		AgentID:     strings.TrimSpace(in.AgentID),
		Apply:       in.Apply,
		Cwd:         strings.TrimSpace(in.Cwd),
		Permission:  strings.TrimSpace(in.Permission),
		Model:       strings.TrimSpace(in.Model),
		TimeoutMin:  in.TimeoutMin,
		PaneID:      in.PaneID,
		Prompt:      strings.TrimRight(in.Prompt, " \t\r\n"),
		NotifyOnRun: in.NotifyOnRun,
		DeliverChat: in.DeliverChat,
		CatchUp:     in.CatchUp,
		CreatedBy:   createdBy,
		Enabled:     enabled,
		CreatedAt:   time.Now(),
	}
	s.mu.Lock()
	s.items[sched.ID] = sched
	s.order = append(s.order, sched.ID)
	if enabled {
		if err := s.scheduleUnlocked(sched); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	if err := s.saveUnlocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	metrics := s.metrics
	s.mu.Unlock()
	metrics.IncLabelled("solud_schedule_creates_total", map[string]string{"kind": sched.kindLabel()})
	s.notifyChanged()
	return sched, nil
}

// createAgentRunSchedule mints the hidden cron trigger backing one of a
// managed agent's cron triggers (docs/DESIGN-agents-surface.md §5.2). Internal
// on purpose: ScheduleInput.validate refuses the kind, so this is the only way
// an "agent-run" schedule can come into existence, and the ManagedAgentStore is its
// only caller.
func (s *ScheduleStore) createAgentRunSchedule(agentID, name, cronExpr string, enabled bool) (*Schedule, error) {
	cronExpr = strings.TrimSpace(cronExpr)
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("agentId is required")
	}
	if cronExpr == "" {
		return nil, errors.New("cron is required")
	}
	if _, err := cronParser.Parse(cronExpr); err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	sched := &Schedule{
		ID:        generateScheduleID(),
		Name:      defaultName(name, "agent "+agentID),
		Cron:      cronExpr,
		Kind:      agentKindSchedule,
		AgentID:   agentID,
		Enabled:   enabled,
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	s.items[sched.ID] = sched
	s.order = append(s.order, sched.ID)
	if enabled {
		if err := s.scheduleUnlocked(sched); err != nil {
			delete(s.items, sched.ID)
			s.order = removeString(s.order, sched.ID)
			s.mu.Unlock()
			return nil, err
		}
	}
	if err := s.saveUnlocked(); err != nil {
		s.unscheduleUnlocked(sched.ID)
		delete(s.items, sched.ID)
		s.order = removeString(s.order, sched.ID)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	s.notifyChanged()
	return sched, nil
}

// SetOwnedBy marks a schedule as imported into a managed agent
// (docs/DESIGN-agents-surface.md §8.3 Phase 1). Nothing about the schedule's
// behavior changes — it keeps its cron entry, its prompt and its logs; the flag
// only moves its SURFACE from the Schedules pane to the agent card.
//
// Refuses a second owner: two cards claiming one schedule's runs would make
// both of them wrong about what they are showing.
func (s *ScheduleStore) SetOwnedBy(schedID, agentID string) error {
	if strings.TrimSpace(agentID) == "" {
		return errors.New("agentId is required")
	}
	s.mu.Lock()
	sched, ok := s.items[schedID]
	if !ok {
		s.mu.Unlock()
		return errors.New("schedule not found")
	}
	if sched.OwnedByAgentID != "" && sched.OwnedByAgentID != agentID {
		owner := sched.OwnedByAgentID
		s.mu.Unlock()
		return fmt.Errorf("schedule is already owned by agent %s", owner)
	}
	prev := sched.OwnedByAgentID
	sched.OwnedByAgentID = agentID
	if err := s.saveUnlocked(); err != nil {
		sched.OwnedByAgentID = prev
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	s.notifyChanged()
	return nil
}

// ClearOwnedBy releases an imported schedule back to the Schedules pane. This
// is the migration's rollback path (ManagedAgentStore.Delete): the schedule reappears
// with its current Enabled state, its counters and its history intact, exactly
// as if the import had never happened.
func (s *ScheduleStore) ClearOwnedBy(schedID string) error {
	s.mu.Lock()
	sched, ok := s.items[schedID]
	if !ok {
		s.mu.Unlock()
		return errors.New("schedule not found")
	}
	if sched.OwnedByAgentID == "" {
		s.mu.Unlock()
		return nil
	}
	prev := sched.OwnedByAgentID
	sched.OwnedByAgentID = ""
	if err := s.saveUnlocked(); err != nil {
		sched.OwnedByAgentID = prev
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	s.notifyChanged()
	return nil
}

// Update replaces fields on an existing schedule. Unset optionals are
// preserved. Returns the updated schedule.
func (s *ScheduleStore) Update(id string, in ScheduleInput) (*Schedule, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("schedule not found")
	}
	enabled := sched.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	s.bumpEditUnlocked(id)
	sched.Name = scheduleName(in)
	sched.Cron = strings.TrimSpace(in.Cron)
	sched.RunAt = in.RunAt
	sched.Until = in.Until
	sched.MaxFires = in.MaxFires
	sched.Kind = strings.TrimSpace(in.Kind)
	sched.AgentID = strings.TrimSpace(in.AgentID)
	sched.Apply = in.Apply
	sched.Cwd = strings.TrimSpace(in.Cwd)
	sched.Permission = strings.TrimSpace(in.Permission)
	sched.Model = strings.TrimSpace(in.Model)
	sched.TimeoutMin = in.TimeoutMin
	if in.PaneID != sched.PaneID || enabled {
		// Retargeted or turned back on: the daemon's pause reason is stale.
		sched.PausedReason = ""
	}
	sched.PaneID = in.PaneID
	sched.Prompt = strings.TrimRight(in.Prompt, " \t\r\n")
	sched.NotifyOnRun = in.NotifyOnRun
	sched.DeliverChat = in.DeliverChat
	sched.CatchUp = in.CatchUp
	// Rewire: remove existing entry if any, then re-add if enabled.
	s.unscheduleUnlocked(id)
	wasEnabled := sched.Enabled
	sched.Enabled = enabled
	if enabled {
		if !wasEnabled {
			// Same disabled→enabled stamp as SetEnabled — see LastArmedAt.
			now := time.Now()
			sched.LastArmedAt = &now
		}
		if err := s.scheduleUnlocked(sched); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	if err := s.saveUnlocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	s.notifyChanged()
	return sched, nil
}

func (s *ScheduleStore) Delete(id string) error {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return errors.New("schedule not found")
	}
	isTask := sched.isTask()
	s.unscheduleUnlocked(id)
	delete(s.items, id)
	s.order = removeString(s.order, id)
	if err := s.saveUnlocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	// Task run logs live in a per-schedule directory keyed by this ID; with
	// the schedule gone nothing can ever reference them again.
	if isTask {
		_ = os.RemoveAll(s.taskRunDir(id))
	}
	s.notifyChanged()
	return nil
}

// MarkRead advances the schedule's read marker — the newest run timestamp
// the user has seen in the reports reader. Monotonic: a stale tab POSTing
// an old timestamp can never un-read newer runs.
func (s *ScheduleStore) MarkRead(id string, at time.Time) (*Schedule, error) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("schedule not found")
	}
	if sched.LastReadAt != nil && !at.After(*sched.LastReadAt) {
		s.mu.Unlock()
		return sched, nil
	}
	t := at
	sched.LastReadAt = &t
	if err := s.saveUnlocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	s.notifyScheduleChanged(id)
	return sched, nil
}

// SetEnabled toggles a schedule on/off without touching any other
// fields. Faster than a full Update for the common sidebar-checkbox path.
func (s *ScheduleStore) SetEnabled(id string, enabled bool) (*Schedule, error) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("schedule not found")
	}
	if sched.Enabled == enabled {
		s.mu.Unlock()
		return sched, nil
	}
	// Refuse to re-enable a one-time schedule whose moment has passed
	// or that's already fired — the trigger is spent, there's nothing
	// to wire up. User should duplicate/recreate instead.
	if enabled && sched.oneTime() {
		if sched.RunAt == nil || !sched.RunAt.After(time.Now()) || sched.FireCount > 0 {
			s.mu.Unlock()
			return nil, errors.New("one-time schedule has already fired or its runAt is in the past")
		}
	}
	s.unscheduleUnlocked(id)
	s.bumpEditUnlocked(id)
	sched.Enabled = enabled
	if enabled {
		sched.PausedReason = ""
		// Stamp the disabled→enabled edge so the boot-time missed-fire
		// count never blames a deliberate pause on daemon downtime.
		now := time.Now()
		sched.LastArmedAt = &now
		if err := s.scheduleUnlocked(sched); err != nil {
			sched.Enabled = false
			s.mu.Unlock()
			return nil, err
		}
	}
	if err := s.saveUnlocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	s.notifyChanged()
	return sched, nil
}

// SnoozeNext marks the schedule's next natural fire as skipped. A
// one-click alternative to disable → re-enable for the "this specific
// upcoming fire would collide with in-flight work" case. Refuses on
// one-time schedules (the whole point of RunAt is a user-chosen
// instant — snoozing past it would defeat the trigger) and on
// disabled schedules (no next fire to skip).
//
// Stores SnoozeUntil = nextFire + 1s; fire() checks this and records
// a "skipped" run with detail "snoozed", then clears the marker.
// Clicking snooze a second time before the next fire lands shifts
// the marker to the fire after that — the new NextFire is already
// the post-snooze one.
func (s *ScheduleStore) SnoozeNext(id string) (*Schedule, error) {
	s.mu.Lock()
	sched, ok := s.items[id]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("schedule not found")
	}
	if sched.oneTime() {
		return nil, errors.New("snooze only applies to recurring schedules")
	}
	if !sched.Enabled {
		return nil, errors.New("schedule is paused — no next fire to snooze")
	}
	next := s.NextFire(id)
	if next.IsZero() {
		return nil, errors.New("no upcoming fire to snooze")
	}
	mark := next.Add(1 * time.Second)
	s.mu.Lock()
	sched.SnoozeUntil = &mark
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.notifyChanged()
	return sched, nil
}

// FireNow triggers a one-off run, outside the cron schedule. Useful for
// testing a job from the UI. Respects the same gating as a normal fire.
func (s *ScheduleStore) FireNow(id string) (*Schedule, error) {
	s.mu.Lock()
	sched, ok := s.items[id]
	if ok && sched.isTask() {
		// "Run now" is the user's probe past a usage-limit hold: if the limit
		// is still spent, this run fails and sets the hold again.
		s.usageLimitUntil = time.Time{}
	}
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("schedule not found")
	}
	s.fire(sched.ID)
	return sched, nil
}

func (s *ScheduleStore) notifyChanged() {
	s.notifyScheduleChanged("")
}

// notifyScheduleChanged tells clients one schedule changed (a run started or
// landed, a report was read). The event names it in Text, so a client can
// fetch just that row (GET /api/schedules/{id}) instead of the whole list —
// every run used to make every open window reload the full list (~160 KB on a
// busy machine) twice. An empty id means "the list itself changed" (create,
// delete, edit): refetch everything. Clients that ignore Text simply refetch
// the list, as before.
func (s *ScheduleStore) notifyScheduleChanged(id string) {
	// Any schedule mutation may change the upcoming-reminder set; keep the
	// cloud mirror fresh (debounced, no-op without a pusher).
	s.scheduleReminderSync()
	if s.uiBus == nil {
		return
	}
	s.uiBus.publish(uiEvent{Type: "schedules-changed", Text: id})
}
