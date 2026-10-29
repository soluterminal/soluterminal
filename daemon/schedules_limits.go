package main

// Task-run failure handling that must not cost the user a run:
//
//   - A temporary API or provider error (529 overloaded, 5xx, a dropped
//     connection) is retried ONCE after taskRetryDelay. A one-time task used to
//     be lost for good on a single 529 (feedback #54).
//   - A spent usage limit ("You've hit your weekly limit · resets …") holds
//     every task run until the reset. The scheduler used to keep spawning
//     sessions into the limit every few minutes; now a recurring run records a
//     quiet skip and a one-time run moves to just after the reset.
//
// Both read only the last few lines of a FAILED run's output, so a report
// that merely discusses an outage or a limit is never misread.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"soluterminal-shared/providers"
)

// taskRetryDelay is how long a task waits before its one retry. A var so
// tests can shrink it.
var taskRetryDelay = 2 * time.Minute

// taskLimitFallbackHold is how long runs are held when a usage-limit message
// names no reset time we can read.
var taskLimitFallbackHold = time.Hour

const (
	taskFailTransient = "transient"
	taskFailQuota     = "quota"
)

// taskTransientNeedles are lowercase markers of a provider or network fault
// that is expected to clear on its own.
var taskTransientNeedles = []string{
	"api error: 500", "api error: 502", "api error: 503", "api error: 504", "api error: 529",
	"overloaded_error", "overloaded", "internal server error", "service unavailable",
	"bad gateway", "gateway timeout", "connection error", "request timed out",
	"econnreset", "etimedout", "eai_again", "socket hang up", "fetch failed",
}

// taskErrorTail is the part of a failed run's output that can describe why
// it failed: its last few non-empty lines.
func taskErrorTail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < 5; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			tail = append(tail, l)
		}
	}
	return strings.Join(tail, "\n")
}

// classifyTaskFailure names a failed run's cause when it is one the
// scheduler handles, or "" for everything else (a real task failure).
func classifyTaskFailure(out []byte) string {
	tail := strings.ToLower(taskErrorTail(out))
	if tail == "" {
		return ""
	}
	if p, _, _, ok := providers.ByRuntimeID("claude-headless"); ok {
		for _, needle := range p.Errors.Quota {
			if strings.Contains(tail, needle) {
				return taskFailQuota
			}
		}
	}
	for _, needle := range taskTransientNeedles {
		if strings.Contains(tail, needle) {
			return taskFailTransient
		}
	}
	return ""
}

var (
	taskResetDate = regexp.MustCompile(`(?i)resets\s+([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)
	taskResetTime = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)
	taskMonths    = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
		"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
		"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
	}
)

func taskClock12(hour, minute, meridiem string) (int, int, bool) {
	h, err := strconv.Atoi(hour)
	if err != nil || h < 1 || h > 12 {
		return 0, 0, false
	}
	m := 0
	if minute != "" {
		if m, err = strconv.Atoi(minute); err != nil || m > 59 {
			return 0, 0, false
		}
	}
	h %= 12
	if strings.EqualFold(meridiem, "pm") {
		h += 12
	}
	return h, m, true
}

// taskLimitResetAt reads when a usage limit resets from Claude's message:
// "…|<unix epoch>", "resets Oct 30, 9am" (weekly) or "resets 10:40pm"
// (session). Times are local, as the CLI prints them. Without a readable
// time it holds for taskLimitFallbackHold, so runs still resume on their own.
func taskLimitResetAt(out []byte, now time.Time) time.Time {
	tail := taskErrorTail(out)
	for _, line := range strings.Split(tail, "\n") {
		if i := strings.LastIndex(line, "|"); i >= 0 {
			if epoch, err := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64); err == nil && epoch > 0 {
				if t := time.Unix(epoch, 0); t.After(now) {
					return t
				}
			}
		}
	}
	if m := taskResetDate.FindStringSubmatch(tail); m != nil {
		mon, okMon := taskMonths[strings.ToLower(m[1])[:3]]
		day, _ := strconv.Atoi(m[2])
		h, mi, okClock := taskClock12(m[3], m[4], m[5])
		if okMon && okClock && day >= 1 && day <= 31 {
			t := time.Date(now.Year(), mon, day, h, mi, 0, 0, now.Location())
			if t.Before(now.Add(-24 * time.Hour)) { // a date already past means next year
				t = t.AddDate(1, 0, 0)
			}
			if t.After(now) {
				return t
			}
		}
	}
	if m := taskResetTime.FindStringSubmatch(tail); m != nil {
		if h, mi, ok := taskClock12(m[1], m[2], m[3]); ok {
			t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
			return t
		}
	}
	return now.Add(taskLimitFallbackHold)
}

// fmtResume renders a resume time for a run's detail line.
func fmtResume(t time.Time) string {
	return t.Local().Format("Mon 2 Jan 15:04")
}

// noteUsageLimit records that task runs are held until `until`.
func (s *ScheduleStore) noteUsageLimit(until time.Time) {
	s.mu.Lock()
	if until.After(s.usageLimitUntil) {
		s.usageLimitUntil = until
	}
	s.mu.Unlock()
}

// clearUsageLimit lifts the hold: a run got through, or the user pressed
// "Run now" (which then acts as the probe — if the limit is still spent, that
// run fails and sets the hold again).
func (s *ScheduleStore) clearUsageLimit() {
	s.mu.Lock()
	s.usageLimitUntil = time.Time{}
	s.mu.Unlock()
}

// heldByUsageLimit returns the reset time while task runs are held.
func (s *ScheduleStore) heldByUsageLimit(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.usageLimitUntil) {
		return s.usageLimitUntil, true
	}
	return time.Time{}, false
}

// deferOneTime moves a one-time schedule to `at` and re-arms it, for a run
// that could not happen because the usage limit is spent. It changes RunAt
// rather than holding a hidden timer, so the row, the cloud reminder mirror
// and a daemon restart all agree on when it will run. seq is the editStamp
// the caller decided on: if the user paused, resumed or edited the schedule
// since, it does nothing — checked under the same lock those changes take.
func (s *ScheduleStore) deferOneTime(id string, at time.Time, seq uint64) bool {
	s.mu.Lock()
	sched, ok := s.items[id]
	if !ok || !sched.oneTime() || sched.FireCount > 0 || s.editSeq[id] != seq {
		s.mu.Unlock()
		return false
	}
	s.unscheduleUnlocked(id)
	at = at.Add(time.Minute) // just after the reset, not on its edge
	sched.RunAt = &at
	sched.Enabled = true
	if err := s.scheduleUnlocked(sched); err != nil {
		sched.Enabled = false
	}
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.notifyScheduleChanged(id)
	return true
}

// waitTaskRetry waits out taskRetryDelay before the one retry, then asks
// again whether a run may start. It returns false — and the first failure is
// recorded as it was — when the store is stopping, or during the wait the
// schedule was deleted, paused, or edited (the retry would run a stale
// prompt, folder or model), or its owning agent was paused (individually or
// by "Pause all autonomy").
func (s *ScheduleStore) waitTaskRetry(schedID string, launch Schedule) bool {
	timer := time.NewTimer(taskRetryDelay)
	defer timer.Stop()
	var done <-chan struct{}
	if s.lifeCtx != nil {
		done = s.lifeCtx.Done()
	}
	select {
	case <-done:
		return false
	case <-timer.C:
	}
	cur, ok := s.Get(schedID)
	if !ok {
		return false
	}
	if launch.Enabled && !cur.Enabled {
		return false
	}
	if cur.Prompt != launch.Prompt || cur.Cwd != launch.Cwd || cur.Permission != launch.Permission || cur.Model != launch.Model {
		return false
	}
	if ownedBy, gate := s.ownedRunHooks(schedID); ownedBy != "" && gate != nil {
		if skip, _ := gate(ownedBy); skip {
			return false
		}
	}
	return true
}

// usageLimitSkipDetail is the run detail for a run held by the usage limit.
func usageLimitSkipDetail(until time.Time) string {
	return fmt.Sprintf("Claude usage limit reached — runs resume after %s", fmtResume(until))
}

// Retry safety. A retry re-runs the whole prompt, so it is only safe when the
// failed attempt never started a tool: once it has, it may already have posted
// a comment, pushed or sent a message, and a second run could repeat that.
// Text output says nothing about tool use, so each attempt runs with two
// Claude Code hooks (passed with --settings, which ADD to the user's own
// hooks — checked against claude 2.1.310):
//
//   - SessionStart writes "started": proof the hooks ran at all. Without it
//     (hooks disabled by the user or by managed policy, an older CLI, a
//     wrapper) nothing is known, so the run is NOT retried.
//   - PreToolUse with no matcher writes "tool" before every tool call,
//     including MCP tools and calls that are then denied (over-counting is
//     the safe direction).
//
// Retry only when "started" exists and "tool" does not.

// taskMarkersEnv also names the marker folder in the child's environment, for
// wrappers (and tests) that stand in for claude.
const taskMarkersEnv = "SOLU_TASK_MARKERS"

const (
	taskMarkerStarted = "started"
	taskMarkerTool    = "tool"
)

type taskAttemptTrace struct {
	hooksRan    bool
	toolStarted bool
}

func (t taskAttemptTrace) safeToRetry() bool { return t.hooksRan && !t.toolStarted }

func (t taskAttemptTrace) whyNoRetry() string {
	if t.toolStarted {
		return "it had already started using tools"
	}
	return "could not confirm it had not started using tools"
}

// prepareTaskMarkers empties the schedule's marker folder (task runs are
// single-flight per schedule, so one fixed folder is enough and run logs stay
// byte-identical across runs) and returns it with the --settings JSON.
// Returns "" when the folder cannot be made: the run then goes ahead
// without retry evidence, and so is never retried.
func (s *ScheduleStore) prepareTaskMarkers(schedID string) (dir, settings string) {
	dir = filepath.Join(s.taskRunDir(schedID), ".attempt")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", ""
	}
	// Forward slashes work in sh, Git Bash and PowerShell alike.
	mark := func(name string) map[string]any {
		return map[string]any{"type": "command", "command": `echo x >> "` + filepath.ToSlash(filepath.Join(dir, name)) + `"`}
	}
	b, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"SessionStart": []any{map[string]any{"hooks": []any{mark(taskMarkerStarted)}}},
		"PreToolUse":   []any{map[string]any{"hooks": []any{mark(taskMarkerTool)}}},
	}})
	if err != nil {
		return "", ""
	}
	return dir, string(b)
}

func readTaskMarkers(dir string) taskAttemptTrace {
	if dir == "" {
		return taskAttemptTrace{}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	t := taskAttemptTrace{hooksRan: exists(taskMarkerStarted), toolStarted: exists(taskMarkerTool)}
	_ = os.RemoveAll(dir)
	return t
}
