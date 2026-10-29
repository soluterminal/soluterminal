package main

// Task-kind schedules: the Schedules engine's third work kind, and the one
// every user gets. On fire it runs a FRESH headless Claude session
// (`claude -p "<prompt>"`) in a chosen directory, captures the transcript as
// the run's log, and records the exit status.
//
// Why not just "prompt into a pane" (the original kind)? That path needs a
// pane to be alive AND idle at fire time — the #1 recorded skip reason — it
// interleaves with whatever the human is doing in that pane, and it can't run
// on a headless box. A task run has none of those constraints: it is
// self-contained, parallel-safe, and observable after the fact.
//
// Security stance: `cwd` and `prompt` come from the local API, but so does
// `POST /api/panes`'s arbitrary `cmd` — a task schedule therefore adds NO
// remote-exec authority beyond what the same-origin API already grants. The
// argv is built here, never accepted from the client: the caller picks a
// permission PRESET by name, not a flag string.
//
// Safety rails (mirroring schedules_agent.go, which has run in production):
//   • per-schedule single-flight — a slow run never stacks on itself;
//   • a global concurrency cap so N schedules firing at once can't fork-bomb;
//   • a hard timeout (default 30m, ceiling 4h) with a killed child tree;
//   • dry-run-first defaults: "plan" (read + plan, no writes) unless the user
//     explicitly opts up to "edit". bypassPermissions is NOT offered at all.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"soluterminal-shared/providers"
)

const (
	taskRunTimeoutDefault = 30 * time.Minute
	taskRunTimeoutMax     = 4 * time.Hour
	// taskLogLimit caps captured output per run — same bound as agent runs.
	taskLogLimit = 256 * 1024
)

// taskPermission maps a UI preset to Claude's --permission-mode. Only these
// two are reachable: "plan" cannot write, and "edit" (acceptEdits) writes
// files but still refuses the destructive tool classes Claude gates. The
// client sends the preset NAME; the flag never crosses the API boundary.
var taskPermissionModes = map[string]string{
	"plan": "plan",
	"edit": "acceptEdits",
}

func validTaskPermission(p string) bool {
	_, ok := taskPermissionModes[p]
	return ok
}

// taskRunDir is where a task schedule's per-run logs live:
// <dataDir>/schedule-runs/<scheduleID>/. Agent runs use the agent's own home
// (see writeRunLog callers) — the runlog endpoint resolves both.
func (s *ScheduleStore) taskRunDir(schedID string) string {
	return filepath.Join(filepath.Dir(s.path), "schedule-runs", schedID)
}

// agentRunSlotKey namespaces a managed-agent run inside the taskRunning map.
// Schedule ids are sched_-prefixed, so an "agent:" key can never collide with
// one — which is what lets ONE map bound both kinds.
func agentRunSlotKey(agentID string) string { return "agent:" + agentID }

// acquireAgentRunSlot admits a managed-agent run against the SAME counter
// fireTask checks (docs/DESIGN-agents-surface.md §5.2, §5.3). An event storm
// of agent runs and a user's nightly task schedules draw from one configurable
// pool and one admission rule. Board-steward templates can take the bookkeeping
// reserve before the general pool, so a long general run cannot starve routine
// board maintenance.
//
// Returns the verdict plus the exact skip detail the agent's run record shows.
func (s *ScheduleStore) acquireAgentRunSlot(agentID string, bookkeeping bool) (bool, string) {
	if s == nil {
		return false, "schedules unavailable on this daemon"
	}
	key := agentRunSlotKey(agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, detail := admitRunSlot(s.taskRunning, s.taskReserved, key, bookkeeping, s.runCapsUnlocked(), "agent")
	if ok && s.taskReserved[key] {
		general := len(s.taskRunning) - len(s.taskReserved)
		log.Printf("scheduler: %s admitted on the bookkeeping reserve (%d general runs in flight)", agentID, general)
	}
	return ok, detail
}

// releaseAgentRunSlot frees what acquireAgentRunSlot took.
func (s *ScheduleStore) releaseAgentRunSlot(agentID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	releaseRunSlot(s.taskRunning, s.taskReserved, agentRunSlotKey(agentID))
	s.mu.Unlock()
}

// ownedRunHooks reads a schedule's owning agent and the gate to ask about it,
// under one lock acquisition. The gate is returned rather than called here: it
// reaches into the ManagedAgentStore, and this store's mutex must not be held across
// that hop.
func (s *ScheduleStore) ownedRunHooks(schedID string) (string, func(string) (bool, string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.items[schedID]
	if !ok {
		return "", nil
	}
	return sched.OwnedByAgentID, s.ownedRunGate
}

// fireTask is the task-kind counterpart of fire()/fireAgent(). Cheap checks
// run synchronously so skips are recorded in order; the spawn+wait detaches.
//
// This is the CLOCK's door: the runner's own cron fire, and any hand fire of a
// schedule the user can still see. An imported schedule's pause walls are asked
// about here because this path never passes runAgent.
func (s *ScheduleStore) fireTask(schedID, cwd, prompt, permission, model string, timeoutMin int) {
	s.fireTaskGated(schedID, cwd, prompt, permission, model, timeoutMin, true)
}

// fireTaskPreGated is the imported-agent door (runAgent's legacy branch,
// agents_run.go). runAgent has ALREADY applied the very walls the gate applies,
// with the one distinction the gate cannot see: "Run now" is an explicit human
// action and is exempt from pause on every card (§6.5, runAgent's
// `agent.Paused && !manual`). Re-asking here would swallow the run the user
// just clicked while the API answered "started" — so the decision is not taken
// twice.
//
// Everything past the gate is identical between the two doors; this is only
// about WHO already decided.
func (s *ScheduleStore) fireTaskPreGated(schedID, cwd, prompt, permission, model string, timeoutMin int) {
	s.fireTaskGated(schedID, cwd, prompt, permission, model, timeoutMin, false)
}

func (s *ScheduleStore) fireTaskGated(schedID, cwd, prompt, permission, model string, timeoutMin int, askOwner bool) {
	run := ScheduleRun{At: time.Now()}
	if providers.HasRetiredDeepSeekReference(model) {
		run.Status = "error"
		run.Detail = "retired DeepSeek model is not dispatchable"
		s.recordRun(schedID, run)
		return
	}
	// An IMPORTED schedule answers to its agent card's pause AND to the
	// daemon-wide "Pause all autonomy" (docs/DESIGN-agents-surface.md §6.5,
	// §8.3). This is the only place that can be true for its CRON fire: that
	// fire arrives here directly and never passes runAgent's gates, so without
	// it the one agent the user most wants to be able to stop would be the one
	// Pause all does not reach.
	//
	// Everything below this block — argv, stdin, logs, single-flight — is
	// untouched by the import, which is what makes the migration a no-op.
	if askOwner {
		if ownedBy, gate := s.ownedRunHooks(schedID); ownedBy != "" {
			// Fail closed: a schedule a card owns is hidden from Automations, so
			// it must never run before (or without) the gate that can stop it,
			// e.g. a cron fire in the boot window before the gate is installed.
			skip, detail := true, "the agent card that owns this schedule is not loaded"
			if gate != nil {
				skip, detail = gate(ownedBy)
			}
			if skip {
				run.Status = "skipped"
				run.Detail = detail
				s.recordRun(schedID, run)
				return
			}
		}
	}
	if !validTaskPermission(permission) {
		run.Status = "error"
		run.Detail = "unknown permission preset: " + permission
		s.recordRun(schedID, run)
		return
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		run.Status = "error"
		run.Detail = "working directory is gone: " + cwd
		s.recordRun(schedID, run)
		return
	}
	bin, err := resolveClaudeBin()
	if err != nil {
		run.Status = "error"
		run.Detail = err.Error()
		s.recordRun(schedID, run)
		return
	}
	// The Claude usage limit is spent: don't spawn a session just to read
	// "You've hit your weekly limit" again. A one-time run moves past the
	// reset; a recurring one records a skip (skips don't page the phone).
	if until, held := s.heldByUsageLimit(time.Now()); held {
		if cur, ok := s.Get(schedID); ok && cur.oneTime() {
			// A one-time run that is not on (a pause raced this fire) is
			// simply not run; one that is on moves past the reset.
			if on, seq, _ := s.editStamp(schedID); on && s.deferOneTime(schedID, until, seq) {
				log.Printf("scheduler: task %s deferred to after the usage-limit reset (%s)", schedID, fmtResume(until))
			}
			return
		}
		run.Status = "skipped"
		run.Detail = usageLimitSkipDetail(until)
		s.recordRun(schedID, run)
		return
	}

	s.mu.Lock()
	ok, detail := admitRunSlot(s.taskRunning, s.taskReserved, schedID, false, s.runCapsUnlocked(), "task")
	s.mu.Unlock()
	if !ok {
		run.Status = "skipped"
		run.Detail = detail
		s.recordRun(schedID, run)
		return
	}
	s.notifyScheduleChanged(schedID) // sidebar flips to "running…" immediately

	go s.runTaskProcess(schedID, bin, cwd, prompt, permission, model, timeoutMin)
}

// runTaskProcess resolves the timeout from the schedule's TimeoutMin and
// releases the single-flight slot when done.
func (s *ScheduleStore) runTaskProcess(schedID, bin, cwd, prompt, permission, model string, timeoutMin int) {
	defer func() {
		s.mu.Lock()
		releaseRunSlot(s.taskRunning, s.taskReserved, schedID)
		s.mu.Unlock()
		s.notifyScheduleChanged(schedID)
	}()

	timeout := taskRunTimeoutDefault
	if timeoutMin > 0 {
		timeout = time.Duration(timeoutMin) * time.Minute
		if timeout > taskRunTimeoutMax {
			timeout = taskRunTimeoutMax
		}
	}
	s.runTaskProcessWithTimeout(schedID, bin, cwd, prompt, permission, model, timeout)
}

// runTaskProcessWithTimeout is the spawn+wait body. Split from runTaskProcess
// so tests can drive sub-minute timeouts (TimeoutMin's unit is minutes).
// Does NOT touch the single-flight slot — its caller owns that, and holds it
// through the retry wait so the row keeps showing "running…" and no second
// run can start meanwhile (schedules_limits.go).
func (s *ScheduleStore) runTaskProcessWithTimeout(schedID, bin, cwd, prompt, permission, model string, timeout time.Duration) {
	launch, _ := s.Get(schedID)
	run, out, trace := s.runTaskAttempt(schedID, bin, cwd, prompt, permission, model, timeout)
	if run.Status == "error" && classifyTaskFailure(out) == taskFailTransient {
		switch {
		case !trace.safeToRetry():
			// The attempt may already have acted (posted, pushed, sent):
			// running the whole prompt again could do it twice.
			log.Printf("scheduler: task %s hit a temporary error after it may have acted (tools=%v hooks=%v); not retrying", schedID, trace.toolStarted, trace.hooksRan)
			run.Detail += " · not retried: " + trace.whyNoRetry()
		default:
			log.Printf("scheduler: task %s hit a temporary error (%s) before any tool use; retrying once in %s", schedID, run.Summary, taskRetryDelay)
			if s.waitTaskRetry(schedID, launch) {
				first := strings.TrimPrefix(run.Summary, "failed: ")
				run, out, _ = s.runTaskAttempt(schedID, bin, cwd, prompt, permission, model, timeout)
				run.Detail += " · retried once after a temporary error"
				if first != "" {
					run.Detail += " (" + first + ")"
				}
			}
		}
	}
	var limitUntil time.Time
	switch {
	case run.Status == "fired":
		s.clearUsageLimit()
	case run.Status == "error" && classifyTaskFailure(out) == taskFailQuota:
		limitUntil = taskLimitResetAt(out, time.Now())
		s.noteUsageLimit(limitUntil)
		run.Detail += " · usage limit reached; task runs resume after " + fmtResume(limitUntil)
	}
	// Only re-arm a one-time task that is still on: if the user paused it
	// while it ran, that pause stands.
	stillOn, seq, _ := s.editStamp(schedID)
	s.recordRun(schedID, run)
	// A one-time task's single chance must not be spent on the limit: move
	// it to just after the reset (recordRun disabled it a moment ago). A
	// pause or edit landing at any point after the stamp makes this a no-op.
	if !limitUntil.IsZero() && stillOn {
		s.deferOneTime(schedID, limitUntil, seq)
	}
	log.Printf("scheduler: task %s finished: %s", schedID, run.Detail)

	if s.uiBus != nil {
		s.uiBus.publish(uiEvent{Type: "schedule-fired", Text: schedID})
		icon := "🤖"
		if run.Status == "error" {
			icon = "⚠️"
		}
		if sched, ok := s.Get(schedID); ok {
			// Task run-results live in the Inbox (reports feed); keep them out
			// of the Alerts "Recent" log to avoid duplicating that surface.
			s.uiBus.publish(uiEvent{Type: "toast", Text: fmt.Sprintf("%s %s: %s", icon, sched.Name, run.Detail), SkipRecent: true})
		}
	}
}

// runTaskAttempt spawns one headless run and returns its record (not yet
// recorded) and captured output.
func (s *ScheduleStore) runTaskAttempt(schedID, bin, cwd, prompt, permission, model string, timeout time.Duration) (ScheduleRun, []byte, taskAttemptTrace) {
	run := ScheduleRun{At: time.Now()}
	markers, settings := s.prepareTaskMarkers(schedID)

	// Headless, non-interactive. --permission-mode is preset-derived (never
	// client-supplied). Plain text output: the log IS the deliverable a human
	// reads later, so stream-json's frame soup would be a downgrade here.
	//
	// The prompt goes on STDIN, not as `-p <prompt>`. Claude's `-p/--print`
	// takes an OPTIONAL value, so its parser refuses a value beginning with a
	// dash: a task whose prompt starts with "--" (or "-n", …) dies with
	// "error: unknown option". Verified against the installed claude CLI.
	// Stdin sidesteps argv parsing entirely and is the canonical headless form.
	args := []string{
		"-p",
		"--permission-mode", taskPermissionModes[permission],
		"--output-format", "text",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = providerChildEnvForBin(bin, withResolvedNodeBin(os.Environ(), resolveNodeBin))
	if markers != "" {
		cmd.Env = append(cmd.Env, taskMarkersEnv+"="+markers)
	}
	var prevRun *ScheduleRun
	prevRead := false
	if sched, ok := s.Get(schedID); ok {
		prevRun = lastReportRun(sched.Runs)
		prevRead = taskReportSeen(sched, prevRun)
	}
	cmd.Stdin = strings.NewReader(taskPromptStdin(prompt, prevRun, prevRead))
	applyDetached(cmd) // own session: survives a daemon restart, killable as a tree
	out := &boundedBuf{limit: taskLogLimit}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	if err := startWithoutCoreDumps(cmd); err != nil {
		run.Status = "error"
		run.Detail = "spawn failed: " + err.Error()
		return run, nil, readTaskMarkers(markers)
	}
	log.Printf("scheduler: task %s started (perm=%s pid=%d cwd=%s)", schedID, permission, cmd.Process.Pid, cwd)
	kill := time.AfterFunc(timeout, func() { _ = killChildTree(cmd.Process) })
	werr := waitOwnedCommand(cmd)
	timedOut := !kill.Stop()

	// The header is the ONE difference an import makes to a run log
	// (docs/DESIGN-agents-surface.md §9 S8 AC): an owned schedule's header
	// gains an `· agent=agt_…` segment naming the card the run also lands on.
	// Argv, stdin, the log directory, the output cap and the kill behavior are
	// all identical to a pre-import run, which is what the equivalence check
	// rests on.
	header := fmt.Sprintf("# scheduled task · permission=%s · cwd=%s · started=%s\n# prompt: %s\n\n",
		permission, cwd, start.Format(time.RFC3339), prompt)
	if sched, ok := s.Get(schedID); ok && sched.OwnedByAgentID != "" {
		header = fmt.Sprintf("# scheduled task · permission=%s · cwd=%s · agent=%s · started=%s\n# prompt: %s\n\n",
			permission, cwd, sched.OwnedByAgentID, start.Format(time.RFC3339), prompt)
	}
	run.LogFile = writeRunLog(s.taskRunDir(schedID), start, append([]byte(header), out.buf...))

	dur := time.Since(start).Round(time.Second)
	switch {
	case timedOut:
		run.Status = "error"
		run.Detail = fmt.Sprintf("timed out after %s — killed", timeout)
		run.Summary = "failed: task timed out"
	case werr == nil:
		run.Status = "fired"
		run.Detail = fmt.Sprintf("%s · finished in %s", permission, dur)
		run.Summary = taskOutputPreview(out.buf)
	default:
		run.Status = "error"
		code := -1
		if ee, ok := werr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		run.Detail = fmt.Sprintf("%s · exit %d in %s", permission, code, dur)
		if line := lastErrorLine(out.buf); line != "" {
			run.Summary = "failed: " + line
		} else {
			run.Summary = "failed"
		}
	}
	return run, out.buf, readTaskMarkers(markers)
}

// taskRunningFor reports whether a task schedule has a run in flight (the DTO's
// agentRunning flag doubles for both spawned kinds — the UI just shows "running…").
func (s *ScheduleStore) taskRunningFor(schedID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taskRunning[schedID]
}

// reportStyleHint rides every managed-agent prompt on stdin (agents_run.go);
// task schedules use the stricter taskReportHint below. Output renders in
// the web reader's markdown surface, and "## Needs you" first is the
// product's report contract — enforced at RUN time (not create time) so
// schedules created before the contract, or via MCP/API with bare prompts,
// produce structured reports too. Formatting-only guidance: it never
// changes WHAT the task does, and the run-log header still records the
// user's own prompt verbatim.
const reportStyleHint = "\n\n(Format your reply as a markdown report. If anything needs" +
	" the user's decision or action, open with a \"## Needs you\" section;" +
	" otherwise open with the line \"Nothing needs you today.\" Short ##" +
	" sections after that; tables and checklists welcome.)"

// taskReportHint is the task-schedule form of the report contract. A task
// run is a fresh headless session that cannot remember its last report, so
// under the plain contract nearly every daily run re-opened with "## Needs
// you" for the same standing asks, and the Inbox badge meant nothing. Tasks
// therefore reserve the heading for something NEW, and taskPromptStdin hands
// the run the opening of its previous report to compare against. Managed
// agents keep reportStyleHint: their decisions are tracked until acknowledged.
const taskReportHint = "\n\n(Format your reply as a markdown report. Open with a" +
	" \"## Needs you\" section only for a decision or action the user must take" +
	" that is new since your last report; do not repeat earlier asks there." +
	" Otherwise open with the line \"Nothing new needs you.\" Short ##" +
	" sections after that; tables and checklists welcome.)"

// lastReportRun is the newest successful run with a report lede, or nil.
func lastReportRun(runs []ScheduleRun) *ScheduleRun {
	for i := range runs {
		if runs[i].Status == "fired" && strings.TrimSpace(runs[i].Summary) != "" {
			r := runs[i]
			return &r
		}
	}
	return nil
}

// taskReportSeen reports whether the user can be taken to have seen prev's
// asks. The read marker is per schedule, not per run: opening a NEWER failed
// run moves it past prev without ever showing prev. So prev only counts as
// seen when it is still the newest non-skipped run and the marker reached it;
// otherwise the next run carries its open asks.
func taskReportSeen(sched Schedule, prev *ScheduleRun) bool {
	if prev == nil || sched.LastReadAt == nil || sched.LastReadAt.Before(prev.At) {
		return false
	}
	for _, r := range sched.Runs {
		if r.Status == "skipped" {
			continue
		}
		return r.At.Equal(prev.At)
	}
	return false
}

// taskPromptStdin is what a task run reads on stdin: the user's prompt, the
// report contract, and — when there is one — the opening of the previous
// report (Summary is capped at taskPreviewMaxRunes), so "new since your last
// report" is something the run can actually judge. If the user has NOT read
// that report yet, its asks never reached them: the run is told to carry the
// ones still open, so replacing the Inbox row never drops an unseen ask.
func taskPromptStdin(prompt string, prev *ScheduleRun, prevRead bool) string {
	in := prompt + taskReportHint
	if prev != nil {
		in += fmt.Sprintf("\n\n(Your last report, from %s, began:\n%s\n)",
			prev.At.Local().Format("Mon 2 Jan 2006"), strings.TrimSpace(prev.Summary))
		if !prevRead {
			in += "\n(The user has not read that report yet: keep any of its asks that are" +
				" still open under \"## Needs you\".)"
		}
	}
	return in
}

// taskOutputPreview turns raw task output into a push-notification body:
// the opening of the report, capped so the OS banner shows a coherent
// lede rather than a mid-word chop. Newlines are kept — Claude's reports
// front-load their point, and Android renders multi-line bodies.
const taskPreviewMaxRunes = 280

func taskOutputPreview(out []byte) string {
	text := strings.TrimSpace(string(out))
	runes := []rune(text)
	if len(runes) <= taskPreviewMaxRunes {
		return text
	}
	cut := runes[:taskPreviewMaxRunes]
	// Back up to the last whitespace so we never split a word (unless the
	// text is one giant token, in which case a hard chop is all there is).
	for i := len(cut) - 1; i > taskPreviewMaxRunes/2; i-- {
		if cut[i] == ' ' || cut[i] == '\n' || cut[i] == '\t' {
			cut = cut[:i]
			break
		}
	}
	return strings.TrimSpace(string(cut)) + "…"
}

// lastErrorLine returns the last non-empty line of output, trimmed of whitespace
// and capped so an error summary never overflows UI notification boundaries.
func lastErrorLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			runes := []rune(line)
			if len(runes) > taskPreviewMaxRunes {
				return strings.TrimSpace(string(runes[:taskPreviewMaxRunes])) + "…"
			}
			return line
		}
	}
	return ""
}
