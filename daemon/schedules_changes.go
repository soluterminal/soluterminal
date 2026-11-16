package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Schedule changes: who turned a schedule on or off, or deleted it, and who
// among the agents may.
//
// The person changes any schedule from the app (the REST routes, which take
// only a person). An agent changes schedules through the MCP tools, and only
// the ones its own pane created: a schedule the person made, one another pane
// made, or a built-in agent's schedule stays as the person left it until the
// person changes it. Each change that takes effect is recorded with who made
// it, when, and from where.

const (
	scheduleChangeEnabled  = "enabled"
	scheduleChangeDisabled = "disabled"
	scheduleChangeDeleted  = "deleted"

	scheduleSourceUI     = "ui"
	scheduleSourceMCP    = "mcp"
	scheduleSourceDaemon = "daemon"

	// schedulePersonName is ScheduleChange.By for the person.
	schedulePersonName = "person"

	// maxScheduleChanges caps the record kept on one schedule.
	maxScheduleChanges = 20
	// maxDeletedSchedules caps the deleted-schedules file.
	maxDeletedSchedules = 200

	deletedSchedulesFile = "schedules-deleted.json"
)

// ScheduleChange is one recorded change of a schedule's state.
type ScheduleChange struct {
	At time.Time `json:"at"`
	// Action: "enabled", "disabled" or (in the deleted-schedules file) "deleted".
	Action string `json:"action"`
	// By: "person", the acting pane's @handle or label, or "SoluTerminal" for
	// a change the daemon made itself.
	By     string `json:"by"`
	PaneID string `json:"paneId,omitempty"`
	// Source: "ui" (the app), "mcp" (an agent's tool call) or "daemon".
	Source string `json:"source"`
	// Reason is set for the daemon's own changes ("its target pane is gone").
	Reason string `json:"reason,omitempty"`
}

// ScheduleActor is who is making a change, as it will be recorded.
type ScheduleActor struct {
	By     string
	PaneID string
	Source string
}

// scheduleActorDaemon is the daemon acting by itself, or through a path that
// carries no caller (an agent card's pause acts on its hidden trigger).
var scheduleActorDaemon = ScheduleActor{By: "SoluTerminal", Source: scheduleSourceDaemon}

// scheduleActorPerson is the person acting in the app.
var scheduleActorPerson = ScheduleActor{By: schedulePersonName, Source: scheduleSourceUI}

func scheduleChangeFor(enabled bool) string {
	if enabled {
		return scheduleChangeEnabled
	}
	return scheduleChangeDisabled
}

// noteChange puts a change at the head of the schedule's record. The caller
// holds the store's lock and saves.
func (s *Schedule) noteChange(action string, actor ScheduleActor, reason string, at time.Time) {
	c := ScheduleChange{
		At: at.UTC(), Action: action,
		By: clipRunes(oneLine(actor.By), 80), PaneID: actor.PaneID,
		Source: actor.Source, Reason: clipRunes(oneLine(reason), 120),
	}
	if c.By == "" {
		c.By = scheduleActorDaemon.By
	}
	if c.Source == "" {
		c.Source = scheduleSourceDaemon
	}
	s.Changes = append([]ScheduleChange{c}, s.Changes...)
	if len(s.Changes) > maxScheduleChanges {
		s.Changes = s.Changes[:maxScheduleChanges]
	}
}

// pausedChange is the record of the pause a disabled schedule is in now, if
// one was written (a schedule paused before records were kept has none).
func (s *Schedule) pausedChange() (ScheduleChange, bool) {
	if s.Enabled || len(s.Changes) == 0 || s.Changes[0].Action != scheduleChangeDisabled {
		return ScheduleChange{}, false
	}
	return s.Changes[0], true
}

// finished reports a schedule that is off because it has nothing left to
// run, not because someone paused it.
func (s *Schedule) finished(now time.Time) bool {
	if s.Enabled {
		return false
	}
	if s.oneTime() {
		return s.FireCount > 0 || !s.RunAt.After(now)
	}
	if s.Until != nil && !s.Until.After(now) {
		return true
	}
	return s.MaxFires > 0 && s.FireCount >= s.MaxFires
}

// deletedSchedule is one line of the deleted-schedules file: enough to say
// what was deleted, by whom and when. The prompt is not kept.
type deletedSchedule struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind,omitempty"`
	AgentID   string          `json:"agentId,omitempty"`
	CreatedBy *ScheduleOrigin `json:"createdBy,omitempty"`
	Change    ScheduleChange  `json:"change"`
}

func (s *ScheduleStore) deletedPath() string {
	return filepath.Join(filepath.Dir(s.path), deletedSchedulesFile)
}

// recordDeletedUnlocked appends a deletion to the deleted-schedules file and
// the log. The schedule is already gone; a failure to write the record is
// logged and does not bring it back. The caller holds the store's lock.
func (s *ScheduleStore) recordDeletedUnlocked(gone Schedule, actor ScheduleActor, at time.Time) {
	var rec Schedule
	rec.noteChange(scheduleChangeDeleted, actor, "", at)
	entry := deletedSchedule{
		ID: gone.ID, Name: gone.Name, Kind: gone.Kind, AgentID: gone.AgentID,
		CreatedBy: gone.CreatedBy, Change: rec.Changes[0],
	}
	log.Printf("scheduler: %s deleted by %q (source %s, pane %q)", gone.ID, entry.Change.By, entry.Change.Source, entry.Change.PaneID)
	list := s.deletedUnlocked()
	list = append([]deletedSchedule{entry}, list...)
	if len(list) > maxDeletedSchedules {
		list = list[:maxDeletedSchedules]
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		err = writeFileAtomicSynced(s.deletedPath(), data, 0o600)
	}
	if err != nil {
		log.Printf("scheduler: could not record the deletion of %s: %v", gone.ID, err)
	}
}

func (s *ScheduleStore) deletedUnlocked() []deletedSchedule {
	data, err := os.ReadFile(s.deletedPath())
	if err != nil {
		return nil
	}
	var list []deletedSchedule
	if err := json.Unmarshal(data, &list); err != nil {
		quarantineCorruptState(s.deletedPath(), err)
		return nil
	}
	return list
}

// Deleted returns the recorded deletions, newest first.
func (s *ScheduleStore) Deleted() []deletedSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletedUnlocked()
}

// ---- Agent callers ----

const scheduleAskPersonHint = "To have it changed, ask the person with post_user_ask; they change it in the app's Automations list."

// scheduleUnverifiedCallerNotice refuses a caller that is no verified pane.
const scheduleUnverifiedCallerNotice = "SoluTerminal couldn't verify which pane you are, so you can't turn on, pause or delete automations. Nothing was changed. Ask the person in your reply; they change it in the app's Automations list."

// scheduleAgentActor decides an agent's set_schedule_enabled or
// cancel_schedule and names the agent for the record. doing is the act in
// words ("pause it", "turn it back on", "delete it").
//
// Only a caller traced to a pane gets past the first check: a pane named by a
// header or by SOLU_PANE_ID alone, a program outside the panes and a request
// forwarded for a remote device all arrive here with no caller pane. The pane
// must then be the one that created the schedule, compared by pane id as the
// schedule recorded it; a pane restored under a new id is not taken for the
// old one, since an id that proves nothing must not carry ownership.
func (a *apiServer) scheduleAgentActor(sched Schedule, caller paneCaller, meta mcpCallMeta, doing string) (ScheduleActor, error) {
	pane := strings.TrimSpace(caller.ID)
	if pane == "" || meta.id.Forwarded {
		return ScheduleActor{}, errors.New(scheduleUnverifiedCallerNotice)
	}
	name := "'" + clipRunes(oneLine(sched.Name), 80) + "'"
	switch {
	case sched.Kind != "" && sched.Kind != "prompt" && sched.Kind != "task":
		// A built-in agent's schedule, or any kind an agent cannot create.
		return ScheduleActor{}, errors.New("Automation " + name + " runs a built-in agent, so only the person can " + doing + ". Nothing was changed. " + scheduleAskPersonHint)
	case sched.CreatedBy == nil || strings.TrimSpace(sched.CreatedBy.PaneID) == "":
		return ScheduleActor{}, errors.New("Automation " + name + " was not created by your pane, so only the person can " + doing + ". Nothing was changed. " + scheduleAskPersonHint)
	case sched.CreatedBy.PaneID != pane:
		return ScheduleActor{}, errors.New("Automation " + name + " was created by another pane, so only the person can " + doing + ". Nothing was changed. " + scheduleAskPersonHint)
	}
	by := pane
	if a.mgr != nil {
		if p, ok := a.mgr.Get(pane); ok {
			by = a.paneCallerLabel(p)
		}
	}
	return ScheduleActor{By: by, PaneID: pane, Source: scheduleSourceMCP}, nil
}

// agentScheduleCreateRefusal is the interim rule for automations an agent
// creates over MCP (create_schedule; ST-153): a Claude task that may change
// files ("edit", or any later mode beyond read-only "plan") runs unattended on
// every fire, so agents can't create one. The person sets it up in the app's
// Automations list. It returns the refusal, or "" when the agent may create
// the schedule.
//
// This is the one place the rule lives: Automations stage 2 replaces it with
// a pending-approval flow for agent-created automations.
func agentScheduleCreateRefusal(in ScheduleInput) string {
	if in.Kind != "task" || in.Permission == "plan" || !validTaskPermission(in.Permission) {
		return ""
	}
	return fmt.Sprintf("A task with permission %q runs Claude unattended and lets it change files in %s on every run, so agents can't create one. Nothing was created. If the user wants it, ask them to set it up in the app's Automations list (post_user_ask); otherwise use permission \"plan\" (read-only).", in.Permission, in.Cwd)
}
