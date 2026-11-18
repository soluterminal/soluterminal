package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// scheduleDTO augments the persisted Schedule with computed fields the
// UI cares about (next-fire time) so we don't force clients to
// duplicate the cron-parsing logic.
type scheduleDTO struct {
	Schedule
	NextFire *time.Time `json:"nextFire,omitempty"`
	// AgentRunning: a daemon-spawned run of this schedule is in flight right
	// now. Set for both spawned kinds (agent and task) — the UI renders one
	// "running…" state and doesn't care which.
	AgentRunning bool `json:"agentRunning,omitempty"`
	// WaitingForPane: a scheduled prompt could not go into its pane and is
	// waiting for it (ScheduleStore.waitForPane); WaitingReason says why
	// ("the pane is busy", "you are typing in the pane").
	WaitingForPane bool   `json:"waitingForPane,omitempty"`
	WaitingReason  string `json:"waitingReason,omitempty"`
	// RunNow is set only on the answer to POST /api/schedules/{id}/fire.
	RunNow *runNowOutcome `json:"runNow,omitempty"`
}

func (a *apiServer) makeScheduleDTO(s Schedule) scheduleDTO {
	dto := scheduleDTO{Schedule: s}
	if s.Enabled {
		if t := a.scheds.NextFire(s.ID); !t.IsZero() {
			dto.NextFire = &t
		}
	}
	dto.AgentRunning = a.scheds.scheduleRunning(s)
	dto.WaitingReason, dto.WaitingForPane = a.scheds.paneWaitReason(s.ID)
	return dto
}

// listSchedules answers the Schedules pane. Managed agents were removed; of
// the schedules they left behind:
//
//   - a hidden Kind=="agent-run" trigger that is still here is omitted: it
//     holds no prompt, does nothing and can't be changed;
//   - a kept imported schedule (keptImportedSchedule: its prompt is not in
//     agents-export.md) is listed so the person can see it and delete it
//     (deleteKeptImportedSchedule). It never runs, and every other change
//     to it is refused.
//
// ?includeAgentRuns=1 also lists the hidden triggers, for debugging. MCP's
// list leaves out both kinds.
func (a *apiServer) listSchedules(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		writeJSON(w, 200, []scheduleDTO{})
		return
	}
	includeAgentRuns := r != nil && r.URL.Query().Get("includeAgentRuns") == "1"
	out := []scheduleDTO{}
	for _, s := range a.scheds.List() {
		if !scheduleListed(s) && !includeAgentRuns {
			continue
		}
		out = append(out, a.makeScheduleDTO(s))
	}
	writeJSON(w, 200, out)
}

// getSchedule answers one row, on the same visibility terms as the list: a
// hidden agent trigger is "not found" here too. It is
// what a client fetches when a `schedules-changed` event names one schedule.
func (a *apiServer) getSchedule(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		http.Error(w, "schedule not found", 404)
		return
	}
	s, ok := a.scheds.Get(r.PathValue("id"))
	if !ok || !scheduleListed(s) {
		http.Error(w, "schedule not found", 404)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(s))
}

// agentRunScheduleRefused answers 400 when a human-surface mutation targets a
// hidden agent trigger, naming the endpoint that DOES own it. Returns true when
// the request was refused (and answered).
func (a *apiServer) agentRunScheduleRefused(w http.ResponseWriter, id string) bool {
	if a.scheds == nil {
		return false
	}
	sched, ok := a.scheds.Get(id)
	if !ok || !sched.isAgentRun() {
		return false
	}
	http.Error(w, "this schedule belonged to a managed agent, which SoluTerminal no longer has; it no longer runs. The removed agents are recorded in "+agentsExportDisplayPath(a.dataDir)+".", 400)
	return true
}

// ownedScheduleRefused is agentRunScheduleRefused for IMPORTED schedules
// (§8.3). Managed agents were removed; an imported schedule still here is one
// the removal kept because agents-export.md does not record its prompt. It
// is listed but never runs: every mutation is refused except the person's
// delete (deleteKeptImportedSchedule), which records it first.
//
// Only the HUMAN endpoints call it. The daemon's own paths — SetPaused's
// SetEnabled, the cron runner's fire, ClearOwnedBy — go through the store
// directly and are unaffected.
func (a *apiServer) ownedScheduleRefused(w http.ResponseWriter, id string) bool {
	if a.scheds == nil {
		return false
	}
	sched, ok := a.scheds.Get(id)
	if !ok || sched.OwnedByAgentID == "" {
		return false
	}
	http.Error(w, "this schedule was imported into a managed agent ("+sched.OwnedByAgentID+
		"), which SoluTerminal no longer has. It no longer runs, and it is kept because its prompt is not in "+
		agentsExportDisplayPath(a.dataDir)+". You can delete it in the Automations list; its prompt is saved to "+
		dataFileDisplayPath(a.dataDir, agentsExportKeptFile)+" first.", 400)
	return true
}

// keptDeleteMu serialises deleteKeptImportedSchedule.
var keptDeleteMu sync.Mutex

// isKeptImportedSchedule reports whether id names a kept imported schedule.
func (a *apiServer) isKeptImportedSchedule(id string) bool {
	if a.scheds == nil {
		return false
	}
	s, ok := a.scheds.Get(id)
	return ok && keptImportedSchedule(s)
}

// scheduleMutationRefused is the pair of guards every human-surface schedule
// mutation runs, so a new handler cannot pick up one and miss the other.
// scheduleMCPTools are where an agent manages schedules instead: the REST
// routes are the person's, like the app's other changes. A schedule runs
// commands and types into panes, so a run started here is input like any.
var scheduleMCPTools = []string{"create_schedule", "set_schedule_enabled", "cancel_schedule"}

func (a *apiServer) scheduleMutationRefused(w http.ResponseWriter, id string) bool {
	return a.agentRunScheduleRefused(w, id) || a.ownedScheduleRefused(w, id)
}

func (a *apiServer) createSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "create a schedule", scheduleMCPTools...) {
		return
	}
	var in ScheduleInput
	if !decodeJSON(w, r, &in, defaultJSONBodyBytes) {
		return
	}
	sched, err := a.scheds.Create(in)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 201, a.makeScheduleDTO(*sched))
}

func (a *apiServer) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "change a schedule", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	var in ScheduleInput
	if !decodeJSON(w, r, &in, defaultJSONBodyBytes) {
		return
	}
	sched, err := a.scheds.UpdateBy(id, in, scheduleActorPerson)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

func (a *apiServer) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "delete a schedule", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.deleteKeptImportedSchedule(w, id) {
		return
	}
	if a.scheduleMutationRefused(w, id) {
		return
	}
	if err := a.scheds.DeleteBy(id, scheduleActorPerson); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.WriteHeader(204)
}

// deleteKeptImportedSchedule lets the person delete an imported schedule that
// the managed-agents removal kept (its prompt is not in agents-export.md). The
// schedule, prompt included, is appended to agents-export-kept.md first; if
// that fails, nothing is deleted. Its run logs stay on disk. The caller has
// already passed requireHuman; enable and edit stay refused. It reports
// whether it answered the request.
func (a *apiServer) deleteKeptImportedSchedule(w http.ResponseWriter, id string) bool {
	if a.scheds == nil {
		return false
	}
	// One kept delete at a time, re-checked under the lock, so two
	// concurrent deletes of one schedule append its record once. (A retry
	// after the schedule list failed to save appends it again; the file is
	// append-only, so a repeat entry is harmless.)
	keptDeleteMu.Lock()
	defer keptDeleteMu.Unlock()
	sched, ok := a.scheds.Get(id)
	if !ok || !keptImportedSchedule(sched) {
		return false
	}
	if err := appendKeptScheduleRecord(a.dataDir, sched, time.Now()); err != nil {
		log.Printf("schedules: kept imported schedule %s not deleted: could not record it first: %v", id, err)
		http.Error(w, "this schedule was not deleted: its prompt could not be saved to "+
			dataFileDisplayPath(a.dataDir, agentsExportKeptFile)+" first", 500)
		return true
	}
	deleted, err := deleteKeptImportedSchedule(a.scheds, id)
	switch {
	case err != nil:
		http.Error(w, "this schedule's prompt is saved, but the schedule list could not be saved: "+err.Error(), 500)
	case !deleted:
		http.Error(w, "schedule not found", 404)
	default:
		log.Printf("schedules: deleted kept imported schedule %s (from removed agent %s); recorded in %s", id, sched.OwnedByAgentID, agentsExportKeptFile)
		w.WriteHeader(204)
	}
	return true
}

func (a *apiServer) enableSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "turn on a schedule", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.SetEnabledBy(id, true, scheduleActorPerson)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

// markScheduleRead advances the read marker for the reports reader.
// Body: {"at": <RFC3339 run timestamp>}. Monotonic (store-enforced).
//
// Guarded like every other human-surface write: a hidden row's runs are read on
// its agent card now, so a stale tab advancing this marker through the schedule
// would clear an unread mark for a report nobody opened.
func (a *apiServer) markScheduleRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A kept imported schedule is listed and its old reports reach the
	// Inbox, so reading them must clear them; marking read changes nothing
	// about what runs.
	if !a.isKeptImportedSchedule(id) && a.scheduleMutationRefused(w, id) {
		return
	}
	var body struct {
		At time.Time `json:"at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.At.IsZero() {
		http.Error(w, "body must be {\"at\": <RFC3339 timestamp>}", 400)
		return
	}
	sched, err := a.scheds.MarkRead(id, body.At)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

func (a *apiServer) disableSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "turn off a schedule", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.SetEnabledBy(id, false, scheduleActorPerson)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

// snoozeSchedule skips the next upcoming fire of a recurring schedule.
// See ScheduleStore.SnoozeNext for semantics — refuses on one-time or
// paused schedules, idempotent for the same upcoming fire.
func (a *apiServer) snoozeSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "skip a schedule's next run", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.SnoozeNext(id)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

func (a *apiServer) fireSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "run a schedule now", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, outcome, err := a.scheds.RunNow(id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	// The refreshed schedule, so the UI shows the new run, plus what Run now
	// did (runNow), so it can say "Started" or "Skipped: why" at once
	// instead of answering 200 either way.
	dto := a.makeScheduleDTO(*sched)
	if cur, ok := a.scheds.Get(sched.ID); ok {
		dto = a.makeScheduleDTO(cur)
	}
	dto.RunNow = &outcome
	writeJSON(w, 200, dto)
}

type injectReq struct {
	Text string `json:"text"`
}

// injectPane writes raw bytes into a pane's PTY. Used by the
// "Fire now" / "Send prompt" UI and any future automation. Accepts
// ASCII control characters (\r, \t, \x1b…) as-is — caller is
// responsible for escaping.
func (a *apiServer) injectPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	var req injectReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", 400)
		return
	}
	caller := a.identifyCaller(r)
	// The person's stop on agent messaging: an agent types into no other
	// pane through here either (agent_messaging_pause.go).
	if caller.Kind != callerHuman && !(caller.tracedPane() && caller.PaneID == id) && agentMessagingPaused() {
		http.Error(w, agentMessagingRefusal("text typed into another pane cannot be held"), http.StatusConflict)
		return
	}
	promptHeld := func() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "human_required",
			"message": "That pane is showing a permission prompt. Agents can't answer it on their own; the person can, or can turn on Settings → Privacy & security → Agent permissions.",
		})
	}
	if !a.promptInputGateFor(caller)(p) {
		promptHeld()
		return
	}
	// The prompt gate again at write time, under the writer lock.
	write := func(b []byte) (int, error) {
		return p.writeWithPrecondition(b, true, false, a.promptWriteCheck(caller, p, b, "rest-schedule"))
	}
	if caller.Kind != callerHuman {
		// As writeProgrammaticInput, but it may not send or clear a put-back
		// message in the input box either, except with keys that answer a
		// live menu.
		write = func(b []byte) (int, error) {
			policy := p.autoModePolicy()
			return p.writeWithFleetAdmissionPolicy(b, true, false, &policy, time.Now().Add(ariaJevTimeout), bothChecks(a.promptWriteCheck(caller, p, b, "rest-schedule"), p.putBackKeysCheck()))
		}
	}
	if _, err := write([]byte(req.Text)); err != nil {
		if errors.Is(err, errPromptHeld) {
			promptHeld()
			return
		}
		if errors.Is(err, errAutoModePolicyChanged) || errors.Is(err, errPutBackInBox) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "write failed: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"bytes": len(req.Text)})
}

// previewSchedule answers "given this cron expression, when would it
// next fire?" — 5 upcoming fires by default. Powers the dialog's
// inline preview so users can sanity-check a custom cron before saving,
// catching footguns like `0 9 * * 1` meaning Monday (robfig: 0=Sun,
// 1=Mon) and `*/5 9 * * *` firing only at 9:00/9:05/9:10/etc.
//
// Stateless: doesn't touch the store. Cheap — cron.Schedule.Next is
// near-instant.
func (a *apiServer) previewSchedule(w http.ResponseWriter, r *http.Request) {
	expr := r.URL.Query().Get("cron")
	if expr == "" {
		http.Error(w, "cron is required", 400)
		return
	}
	count := 5
	if c := r.URL.Query().Get("count"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 && n <= 20 {
			count = n
		}
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		http.Error(w, "invalid cron: "+err.Error(), 400)
		return
	}
	out := make([]time.Time, 0, count)
	cursor := time.Now()
	for i := 0; i < count; i++ {
		next := sched.Next(cursor)
		if next.IsZero() {
			break
		}
		out = append(out, next)
		cursor = next
	}
	// Cron expressions run in THIS machine's wall clock (cron.WithLocation
	// time.Local), not the browser's — a remote/phone client in another
	// timezone needs to know whose "9am" it just scheduled. Abbreviation +
	// offset, since time.Local.String() is often just "Local".
	tzName, tzOffsetSec := time.Now().Zone()
	writeJSON(w, 200, map[string]any{
		"fires":           out,
		"tzName":          tzName,
		"tzOffsetMinutes": tzOffsetSec / 60,
	})
}

// scheduleRunLog serves one agent run's captured output from the agent
// home's runs/ ring buffer (see writeRunLog). The file parameter must be a
// bare "run-<ts>.log" name — the regexp gate makes path traversal
// impossible, and the read stays inside the compiled-in agent home.
func (a *apiServer) scheduleRunLog(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		http.Error(w, "schedules unavailable", 404)
		return
	}
	sched, ok := a.scheds.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "schedule not found", 404)
		return
	}
	// Run logs live per-kind: agents write into their own home (shared with
	// the agent's other state), tasks into <dataDir>/schedule-runs/<id>/.
	var dir string
	kind, isKind := scheduleKinds[sched.Kind]
	switch {
	case isKind && kind.runDir != nil:
		var err error
		if dir, err = kind.runDir(sched); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	case sched.isTask():
		dir = a.scheds.taskRunDir(sched.ID)
	default:
		http.Error(w, "this schedule kind keeps no run logs", 400)
		return
	}
	file := r.URL.Query().Get("file")
	if !runLogNameRe.MatchString(file) {
		http.Error(w, "invalid log file name", 400)
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		http.Error(w, "log not found (rotated out or never written)", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

// Kept as a compile-time check that the ScheduleStore pointer is
// non-nil where expected. Delete if it gets noisy.
var _ = errors.New
