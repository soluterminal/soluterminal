package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
}

func (a *apiServer) makeScheduleDTO(s Schedule) scheduleDTO {
	dto := scheduleDTO{Schedule: s}
	if s.Enabled {
		if t := a.scheds.NextFire(s.ID); !t.IsZero() {
			dto.NextFire = &t
		}
	}
	dto.AgentRunning = a.scheds.scheduleRunning(s)
	return dto
}

// listSchedules answers the Schedules pane. Two classes of row are omitted,
// both because an agent card is their real surface:
//
//   - the hidden Kind=="agent-run" triggers backing managed agents
//     (docs/DESIGN-agents-surface.md §5.2) — a duplicate row here would offer
//     edits that the next agent save would overwrite;
//   - schedules IMPORTED into an agent, OwnedByAgentID != "" (§8.3 Phase 1).
//     These are the user's own hand-built schedules and they still exist,
//     unchanged, in schedules.json — they still fire, still log, still count.
//     Only the row moves.
//
// ?includeAgentRuns=1 is the debugging escape hatch for both — there is
// deliberately no such hatch over MCP.
func (a *apiServer) listSchedules(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		writeJSON(w, 200, []scheduleDTO{})
		return
	}
	includeAgentRuns := r != nil && r.URL.Query().Get("includeAgentRuns") == "1"
	out := []scheduleDTO{}
	for _, s := range a.scheds.List() {
		if (s.isAgentRun() || s.OwnedByAgentID != "") && !includeAgentRuns {
			continue
		}
		out = append(out, a.makeScheduleDTO(s))
	}
	writeJSON(w, 200, out)
}

// getSchedule answers one row, on the same visibility terms as the list: a
// hidden agent trigger or an imported schedule is "not found" here too. It is
// what a client fetches when a `schedules-changed` event names one schedule.
func (a *apiServer) getSchedule(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		http.Error(w, "schedule not found", 404)
		return
	}
	s, ok := a.scheds.Get(r.PathValue("id"))
	if !ok || s.isAgentRun() || s.OwnedByAgentID != "" {
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
	http.Error(w, "this schedule belongs to an agent — use POST /api/agents/"+sched.AgentID+"/pause", 400)
	return true
}

// ownedScheduleRefused is agentRunScheduleRefused for IMPORTED schedules
// (§8.3). The row is hidden but reachable by id, and a mutation through this
// door would change what the agent card describes without the card ever
// knowing — so it is refused, with both the card's controls and the rollback
// named in the message.
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
	http.Error(w, "this schedule is managed by agent "+sched.OwnedByAgentID+
		" — pause, run or delete it from the agent card (rollback: DELETE /api/agents/"+
		sched.OwnedByAgentID+")", 400)
	return true
}

// scheduleMutationRefused is the pair of guards every human-surface schedule
// mutation runs, so a new handler cannot pick up one and miss the other.
func (a *apiServer) scheduleMutationRefused(w http.ResponseWriter, id string) bool {
	return a.agentRunScheduleRefused(w, id) || a.ownedScheduleRefused(w, id)
}

func (a *apiServer) createSchedule(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	var in ScheduleInput
	if !decodeJSON(w, r, &in, defaultJSONBodyBytes) {
		return
	}
	sched, err := a.scheds.Update(id, in)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
}

func (a *apiServer) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	if err := a.scheds.Delete(id); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.WriteHeader(204)
}

func (a *apiServer) enableSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.SetEnabled(id, true)
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
	if a.scheduleMutationRefused(w, id) {
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
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.SetEnabled(id, false)
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
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	sched, err := a.scheds.FireNow(id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	// Give the fire a moment to record its run, then return the refreshed
	// schedule so the UI can show the new entry. The fire itself is
	// synchronous from the caller's POV but the run-record write happens
	// on the goroutine — a 50ms nap is a pragmatic compromise.
	time.Sleep(50 * time.Millisecond)
	for _, s := range a.scheds.List() {
		if s.ID == sched.ID {
			writeJSON(w, 200, a.makeScheduleDTO(s))
			return
		}
	}
	writeJSON(w, 200, a.makeScheduleDTO(*sched))
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
	if !a.promptInputGate(r)(p) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "human_required",
			"message": "That pane is showing a permission prompt. Agents can't answer it on their own; the person can, or can turn on Settings → Devices → Agent permissions.",
		})
		return
	}
	if _, err := p.Write([]byte(req.Text)); err != nil {
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
