package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Task runs stream (Automations stage 2, ST-299 PR 2).
//
// A task run is `claude -p --output-format stream-json --verbose`, writing
// straight into a file in its run folder (run-<ms>.events.jsonl, 0600), not
// into a pipe the daemon holds. So the run's output outlives the daemon that
// started it, and the daemon reads it as it grows (taskStream): the model
// from the first event, the report from the final "result" event, with the
// tokens and cost Claude reports there. When the run ends the daemon writes
// the same readable run-<ms>.log the reports reader shows today and removes
// the events file.
//
// Each run in flight also has a record beside it (run-<ms>.inflight.json):
// which automation, which process (pid and start time), when, and its
// files. A record still there at start means SoluTerminal stopped while the
// run was going. One whose process has ended is finished from its events
// file: its real outcome if Claude wrote its result, else "interrupted"
// (sweepTaskInflight). One whose process still runs is left for now.

const (
	taskEventsSuffix   = ".events.jsonl"
	taskInflightSuffix = ".inflight.json"
)

// taskEventsLimit bounds a run's events file; a run past it is stopped. A
// var for tests.
var taskEventsLimit int64 = 64 << 20

// taskStreamPoll paces the reads of a live run's events file. A var for tests.
var taskStreamPoll = 250 * time.Millisecond

var taskInflightNameRe = regexp.MustCompile(`^run-(\d+)\.inflight\.json$`)

// ScheduleRunUsage is what a task run used, as Claude reported it.
type ScheduleRunUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// CostUSD is Claude's own estimate (total_cost_usd); nil when it gave
	// none. On a subscription it is not a bill.
	CostUSD *float64 `json:"costUsd,omitempty"`
	Turns   int      `json:"turns,omitempty"`
}

// taskStreamResult is the final event of a stream-json run.
type taskStreamResult struct {
	Subtype   string   `json:"subtype"`
	IsError   bool     `json:"is_error"`
	Result    string   `json:"result"`
	Errors    []string `json:"errors"`
	NumTurns  int      `json:"num_turns"`
	SessionID string   `json:"session_id"`
	Usage     struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	ModelUsage   map[string]struct {
		InputTokens              int64 `json:"inputTokens"`
		OutputTokens             int64 `json:"outputTokens"`
		CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

// taskStream reads one run's events file as it grows. Safe for one writer
// (poll) and concurrent readers (snapshot).
type taskStream struct {
	mu     sync.Mutex
	path   string
	off    int64
	rem    []byte
	model  string
	text   boundedBuf // the assistant's text, in order
	plain  boundedBuf // lines that are not events: stderr, or plain output
	result *taskStreamResult
	// dropping: an over-long line is being skipped up to its newline.
	dropping bool
	// tail is the newest liveTextMax bytes of the assistant's text, for
	// the live view (schedules_live.go); tailClipped says older text went.
	tail        []byte
	tailClipped bool
}

func newTaskStream(path string) *taskStream {
	return &taskStream{path: path, text: boundedBuf{limit: taskLogLimit}, plain: boundedBuf{limit: taskLogLimit}}
}

// taskStreamLineMax bounds one event line. A longer one is dropped whole, up
// to its newline: an event that long is a tool's output (file contents,
// command output), which must stay in the events file and never reach the
// report, its summary or a push. A var for tests.
var taskStreamLineMax = 1 << 20

// taskStreamDroppedNote stands in the report for a dropped over-long line.
const taskStreamDroppedNote = "[an event longer than 1 MiB was left out]"

// poll reads what was added to the file since the last poll, never past
// taskEventsLimit, and returns the file's size (a run past the limit is
// stopped by its caller).
func (t *taskStream) poll() int64 {
	f, err := os.Open(t.path)
	if err != nil {
		return 0
	}
	defer f.Close()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return t.off
	}
	buf := make([]byte, 64<<10)
	for t.off < taskEventsLimit {
		want := int64(len(buf))
		if left := taskEventsLimit - t.off; left < want {
			want = left
		}
		n, err := f.Read(buf[:want])
		if n > 0 {
			t.off += int64(n)
			chunk := buf[:n]
			if t.dropping {
				// The rest of an over-long line: nothing of it is kept.
				i := bytes.IndexByte(chunk, '\n')
				if i < 0 {
					chunk = nil
				} else {
					t.dropping = false
					chunk = chunk[i+1:]
				}
			}
			t.rem = append(t.rem, chunk...)
			for {
				i := bytes.IndexByte(t.rem, '\n')
				if i < 0 {
					break
				}
				t.feedLocked(t.rem[:i])
				t.rem = t.rem[i+1:]
			}
			if len(t.rem) > taskStreamLineMax {
				t.dropLongLineLocked()
			}
		}
		if err != nil {
			break
		}
	}
	if fi, err := f.Stat(); err == nil {
		return fi.Size()
	}
	return t.off
}

// finish reads the rest, including a last line without a newline.
func (t *taskStream) finish() {
	t.poll()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.rem) > 0 && !t.dropping {
		t.feedLocked(t.rem)
	}
	t.rem = nil
}

// dropLongLineLocked drops the line being read, which is over
// taskStreamLineMax, up to its newline, leaving only a note in the report.
func (t *taskStream) dropLongLineLocked() {
	t.rem = nil
	t.dropping = true
	_, _ = t.plain.Write([]byte(taskStreamDroppedNote + "\n"))
}

func (t *taskStream) feedLocked(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	if len(trimmed) > taskStreamLineMax {
		// Over the limit, whether it came in one read or several: dropped
		// whole (dropLongLineLocked), never read as text.
		_, _ = t.plain.Write([]byte(taskStreamDroppedNote + "\n"))
		return
	}
	if trimmed[0] != '{' {
		_, _ = t.plain.Write(append(append([]byte(nil), trimmed...), '\n'))
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		_, _ = t.plain.Write(append(append([]byte(nil), trimmed...), '\n'))
		return
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && ev.Model != "" {
			t.model = ev.Model
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				if len(t.text.buf) > 0 {
					_, _ = t.text.Write([]byte("\n\n"))
				}
				_, _ = t.text.Write([]byte(c.Text))
				if len(t.tail) > 0 {
					t.tail = append(t.tail, "\n\n"...)
				}
				t.tail = append(t.tail, c.Text...)
				if len(t.tail) > liveTextMax {
					t.tail = append([]byte(nil), runeSafeTailBytes(t.tail, liveTextMax)...)
					t.tailClipped = true
				}
			}
		}
	case "result":
		var r taskStreamResult
		if json.Unmarshal(trimmed, &r) == nil {
			t.result = &r
		}
	}
}

// report is the run's readable output: Claude's final result, or the text it
// wrote along the way, or (a CLI that printed plain text, or an early crash)
// the plain lines. A failed run's plain lines (stderr) follow its result, so
// the cause stays readable.
func (t *taskStream) report() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var main string
	switch {
	case t.result != nil && strings.TrimSpace(t.result.Result) != "":
		main = t.result.Result
	case t.result != nil && len(t.result.Errors) > 0:
		main = strings.Join(t.result.Errors, "\n")
	default:
		main = string(t.text.buf)
	}
	plain := strings.TrimSpace(string(t.plain.buf))
	switch {
	case strings.TrimSpace(main) == "":
		return []byte(plain)
	case plain != "" && (t.result == nil || t.result.IsError):
		return []byte(strings.TrimSpace(main) + "\n\n" + plain)
	}
	return []byte(strings.TrimSpace(main))
}

// streamOutcome is what the events say about how the run ended.
type streamOutcome struct {
	hasResult bool
	isError   bool
	why       string // the CLI's reason for an error result
	model     string
	usage     *ScheduleRunUsage
}

func (t *taskStream) outcome() streamOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := streamOutcome{model: t.model}
	r := t.result
	if r == nil {
		return o
	}
	o.hasResult = true
	o.isError = r.IsError || (r.Subtype != "" && r.Subtype != "success")
	if o.isError {
		switch {
		case len(r.Errors) > 0:
			o.why = r.Errors[0]
		case strings.TrimSpace(r.Result) != "":
			o.why = lastErrorLine([]byte(r.Result))
		default:
			o.why = r.Subtype
		}
	}
	u := &ScheduleRunUsage{CostUSD: r.TotalCostUSD, Turns: r.NumTurns}
	if len(r.ModelUsage) > 0 {
		var best string
		var bestOut int64 = -1
		names := make([]string, 0, len(r.ModelUsage))
		for name := range r.ModelUsage {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			m := r.ModelUsage[name]
			u.InputTokens += m.InputTokens
			u.OutputTokens += m.OutputTokens
			u.CacheReadTokens += m.CacheReadInputTokens
			u.CacheWriteTokens += m.CacheCreationInputTokens
			if m.OutputTokens > bestOut {
				best, bestOut = name, m.OutputTokens
			}
		}
		// The model that wrote the most is the run's model (a run can use a
		// small model on the side).
		o.model = best
	} else {
		u.InputTokens = r.Usage.InputTokens
		u.OutputTokens = r.Usage.OutputTokens
		u.CacheReadTokens = r.Usage.CacheReadInputTokens
		u.CacheWriteTokens = r.Usage.CacheCreationInputTokens
	}
	o.usage = u
	return o
}

// taskInflight is the record of one task run in flight, beside its files.
type taskInflight struct {
	SchedID string `json:"schedId"`
	RunID   string `json:"runId"`
	PID     int    `json:"pid"`
	// StartTicks is the process's start time (Linux /proc), so a pid used
	// again by another process is not taken for the run. 0 where unknown.
	StartTicks uint64    `json:"startTicks,omitempty"`
	Started    time.Time `json:"started"`
	TimeoutMs  int64     `json:"timeoutMs"`
	Permission string    `json:"permission"`
	Cwd        string    `json:"cwd"`
	Model      string    `json:"model,omitempty"`
	Trigger    string    `json:"trigger,omitempty"`
	Account    string    `json:"account,omitempty"`
	Header     string    `json:"header"`
}

// taskRunFiles names one run's files in its run folder.
type taskRunFiles struct {
	dir   string
	stamp int64 // start, Unix ms: run-<stamp>.*
}

func (f taskRunFiles) events() string {
	return filepath.Join(f.dir, fmt.Sprintf("run-%d%s", f.stamp, taskEventsSuffix))
}
func (f taskRunFiles) inflight() string {
	return filepath.Join(f.dir, fmt.Sprintf("run-%d%s", f.stamp, taskInflightSuffix))
}

// eventsOver reports whether the run's events file is past taskEventsLimit.
func (f taskRunFiles) eventsOver() bool {
	fi, err := os.Stat(f.events())
	return err == nil && fi.Size() > taskEventsLimit
}

// markTaskOverflow records a run stopped for too much output.
func markTaskOverflow(run *ScheduleRun) {
	run.Status = "error"
	run.Cause = runCauseRunFailed
	run.Outcome = ""
	run.Detail = fmt.Sprintf("its output passed %d KiB — stopped", taskEventsLimit>>10)
	run.Summary = "failed: too much output"
}

// recorded reports whether the run's in-flight record is gone: another
// daemon (the one before an update) recorded the run and removed it.
func (f taskRunFiles) recorded() bool {
	_, err := os.Stat(f.inflight())
	return errors.Is(err, os.ErrNotExist)
}

// remove deletes the run's events file and in-flight record.
func (f taskRunFiles) remove() {
	if f.dir == "" {
		return
	}
	_ = os.Remove(f.events())
	_ = os.Remove(f.inflight())
}

// processIsRun reports whether rec's process is still running (and is the
// same process, where its start time is known).
//
// Only a process whose start time matches the record counts: a pid used
// again by another program is never taken for the run, followed or stopped.
// Where the start time cannot be read (macOS, Windows, a record written in
// the instant before its process started), nothing counts: the run is
// recorded from its events file, and its process, if any, is left alone.
func (rec taskInflight) processIsRun() bool {
	if rec.PID <= 0 || rec.StartTicks == 0 || !processAlive(rec.PID) {
		return false
	}
	_, start, ok := readProcCPUTicks(rec.PID)
	return ok && start == rec.StartTicks
}

// writeTaskInflight saves a run's in-flight record.
func writeTaskInflight(files taskRunFiles, rec taskInflight) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(files.inflight(), data, 0o600)
}

// classifyTaskRun turns how a run ended into its record: the outcome from
// the events (when Claude wrote its result), else from the exit. exitCode is
// -1 when unknown; exited false means the daemon did not see the process end
// (it ended while SoluTerminal was stopped).
func classifyTaskRun(run *ScheduleRun, st *taskStream, exited bool, werr error, timedOut bool, permission string, timeout time.Duration) []byte {
	out := st.report()
	o := st.outcome()
	if o.model != "" {
		run.Model = o.model
	}
	run.Usage = o.usage
	dur := time.Duration(run.DurationMs) * time.Millisecond
	dur = dur.Round(time.Second)
	code, failedExit := -1, false
	if exited && werr != nil {
		failedExit = true
		if ee, ok := werr.(interface{ ExitCode() int }); ok {
			code = ee.ExitCode()
		}
	}
	switch {
	case timedOut:
		run.Status = "error"
		run.Cause = runCauseTimeout
		run.Detail = fmt.Sprintf("timed out after %s — killed", timeout)
		run.Summary = "failed: task timed out"
	case o.hasResult && o.isError, failedExit:
		run.Status = "error"
		run.Cause = runCauseRunFailed
		if failedExit {
			run.Detail = fmt.Sprintf("stopped with exit code %d after %s", code, dur)
		} else {
			run.Detail = fmt.Sprintf("ended with an error after %s", dur)
		}
		why := o.why
		if why == "" {
			why = lastErrorLine(out)
		}
		if why != "" {
			run.Summary = "failed: " + clipRunesEllipsis(why, taskPreviewMaxRunes)
		} else {
			run.Summary = "failed"
		}
	case !exited && !o.hasResult:
		run.Status = "error"
		run.Cause = runCauseInterrupted
		run.Detail = "SoluTerminal stopped while this run was going, and it ended without a result"
		run.Summary = "failed: interrupted"
	default:
		run.Status = "fired"
		run.Detail = fmt.Sprintf("%s · finished in %s", permission, dur)
		run.Summary = taskOutputPreview(out)
		switch taskReportVerdict(out) {
		case runOutcomeNothing:
			run.Outcome = runOutcomeNothing
		case runOutcomeFailed:
			// The run ended cleanly, but its report says it could not do
			// its job (a task on 10 Nov was recorded as done while its
			// report said it failed). Status stays "fired": it ran to its
			// end, so it counts toward "stop after N runs" and its report
			// still goes to its chat; the outcome says it failed, and
			// recordRun pushes it as a failure.
			run.Outcome = runOutcomeFailed
			run.Cause = runCauseReportedFailed
			run.Detail = fmt.Sprintf("%s · the report says it could not finish · after %s", permission, dur)
			why := strings.TrimSpace(strings.TrimPrefix(firstReportLine(out), taskReportFailedLead))
			run.Summary = "failed: " + clipRunesEllipsis(why, taskPreviewMaxRunes)
		}
	}
	return out
}

// sweepTaskInflight finishes the task runs SoluTerminal stopped during: each
// in-flight record left in a run folder. A run whose process has ended is
// recorded from its events file. A run whose process still runs is followed
// until it ends (followTaskRun), holding its automation's run slot so the
// timetable cannot start a second run beside it. Runs at start, before the
// timetable starts, so a one-time task whose run was in flight is not also
// recorded as missed.
//
// It returns the followers to start once the store is ready: a run that ends
// at once must not be recorded while the start is still setting the store up.
func (s *ScheduleStore) sweepTaskInflight() (followers []func()) {
	root := filepath.Join(filepath.Dir(s.path), "schedule-runs")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		if !d.IsDir() || !historyIDRe.MatchString(d.Name()) {
			continue
		}
		dir := filepath.Join(root, d.Name())
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			m := taskInflightNameRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			var stamp int64
			_, _ = fmt.Sscan(m[1], &stamp)
			files := taskRunFiles{dir: dir, stamp: stamp}
			data, err := os.ReadFile(files.inflight())
			var rec taskInflight
			if err != nil || json.Unmarshal(data, &rec) != nil || rec.SchedID != d.Name() {
				log.Printf("scheduler: dropping an unreadable in-flight record %s", e.Name())
				files.remove()
				continue
			}
			s.mu.Lock()
			_, known := s.items[rec.SchedID]
			// The run holds its automation's slot until it is recorded:
			// nothing starts beside it, and a one-time task is not counted
			// as missed meanwhile.
			holds := known && !s.taskRunning[rec.SchedID]
			if holds {
				s.taskRunning[rec.SchedID] = true
			}
			s.mu.Unlock()
			if !known {
				files.remove()
				continue
			}
			if s.recoveredSince == nil {
				s.recoveredSince = map[string]time.Time{}
			}
			if rec.Started.After(s.recoveredSince[rec.SchedID]) {
				s.recoveredSince[rec.SchedID] = rec.Started
			}
			if rec.processIsRun() {
				log.Printf("scheduler: task %s run %s (pid %d) is still going from before SoluTerminal restarted; following it", rec.SchedID, rec.RunID, rec.PID)
			}
			followers = append(followers, func() { s.followTaskRun(rec, files, holds) })
		}
	}
	return followers
}

// taskRecoverDelay is how long after start the runs from before a restart
// are recorded or followed: by then the push channel, chat delivery and
// metrics are wired, so a run that failed while the app was off is pushed
// like any other (the catch-up run waits the same way). A var for tests.
var taskRecoverDelay = catchUpDelay

// followTaskRun records a run started before SoluTerminal restarted, after
// taskRecoverDelay: at once if it has ended; else once it ends, keeping its
// time and output limits. holds says it holds the automation's run slot.
//
// A process the daemon cannot confirm is the run (no start time where it
// cannot be read: macOS, Windows) but whose pid still exists is waited for
// up to its time limit and never signalled; if it is still there then, the
// run is recorded with its pid named, so a run still going is not unseen.
func (s *ScheduleStore) followTaskRun(rec taskInflight, files taskRunFiles, holds bool) {
	defer func() {
		if holds {
			s.mu.Lock()
			releaseRunSlot(s.taskRunning, s.taskReserved, rec.SchedID)
			s.mu.Unlock()
		}
		s.notifyScheduleChanged(rec.SchedID)
	}()
	select {
	case <-s.lifeDone():
		return // stopping: the next start finds the record again
	case <-time.After(taskRecoverDelay):
	}
	// During an update the daemon before this one may still be running for
	// a moment, and record the run itself (and remove its files). A record
	// that is gone was recorded by it: nothing is left to do here.
	if files.recorded() {
		return
	}
	deadline := rec.Started.Add(time.Duration(rec.TimeoutMs) * time.Millisecond)
	timedOut := false
	s.noteRunStarted(rec.SchedID, rec.Started)
	defer s.noteRunEnded(rec.SchedID)
	verified := rec.processIsRun()
	unconfirmed := !verified && rec.PID > 0 && rec.StartTicks == 0 && processAlive(rec.PID)
	if unconfirmed {
		for processAlive(rec.PID) && (rec.TimeoutMs <= 0 || time.Now().Before(deadline)) {
			select {
			case <-s.lifeDone():
				return
			case <-time.After(taskFollowPoll):
			}
		}
		if processAlive(rec.PID) && !files.recorded() {
			s.recordUnconfirmedTaskRun(rec, files)
			return
		}
	}
	// A followed run can be watched and stopped like one this daemon
	// started: its stream is read as it grows, and Stop signals its process
	// only once it is confirmed again as the run.
	var live *liveRun
	if verified {
		live = &liveRun{runID: rec.RunID, started: rec.Started, stream: newTaskStream(files.events()), stop: func() error {
			if !rec.processIsRun() {
				return errStaleStop // it ended by itself first
			}
			p, err := os.FindProcess(rec.PID)
			if err != nil {
				return err
			}
			return killChildTree(p)
		}}
		s.setLive(rec.SchedID, live)
	}
	for verified && rec.processIsRun() {
		live.stream.poll()
		if files.eventsOver() {
			// The same output limit as a run this daemon started.
			if p, err := os.FindProcess(rec.PID); err == nil && rec.processIsRun() {
				_ = killChildTree(p)
			}
			break
		}
		if rec.TimeoutMs > 0 && !time.Now().Before(deadline) {
			// Checked again just before the signal: only the run's own
			// process is ever stopped.
			if p, err := os.FindProcess(rec.PID); err == nil && rec.processIsRun() {
				_ = killChildTree(p)
			}
			timedOut = true
			break
		}
		select {
		case <-s.lifeDone():
			return // SoluTerminal is stopping; the next start follows it again
		case <-time.After(taskFollowPoll):
		}
	}
	stopped := s.endLive(rec.SchedID, live)
	if files.recorded() {
		return
	}
	s.recordTaskRunAfterRestartStopped(rec, files, timedOut, stopped)
}

// taskFollowPoll paces followTaskRun. A var for tests.
var taskFollowPoll = 2 * time.Second

// lifeDone is closed when the store stops (nil before it starts: never).
func (s *ScheduleStore) lifeDone() <-chan struct{} {
	s.mu.Lock()
	ctx := s.lifeCtx
	s.mu.Unlock()
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// recordUnconfirmedTaskRun records a run whose pid is still there past its
// time limit, where the daemon cannot confirm the process is the run. It is
// not signalled; the record says so and names the pid for the person.
func (s *ScheduleStore) recordUnconfirmedTaskRun(rec taskInflight, files taskRunFiles) {
	st := newTaskStream(files.events())
	st.finish()
	run := ScheduleRun{ID: rec.RunID, At: rec.Started, Trigger: rec.Trigger, Account: rec.Account, Model: rec.Model, replayed: true}
	endRun(&run, time.Now())
	out := st.report()
	run.Status = "error"
	run.Cause = runCauseInterrupted
	run.Detail = fmt.Sprintf("SoluTerminal restarted while this run was going and cannot confirm on this system that process %d is still it, so it did not stop it; it may still be running", rec.PID)
	run.Summary = fmt.Sprintf("failed: past its time limit, may still be running (process %d)", rec.PID)
	run.LogFile = writeRunLog(files.dir, rec.Started, append([]byte(rec.Header), out...))
	s.recordTaskResult(rec.SchedID, run, out, files)
	log.Printf("scheduler: task %s run %s: process %d still there past the time limit; not signalled (cannot confirm it is the run)", rec.SchedID, rec.RunID, rec.PID)
}

func (s *ScheduleStore) recordTaskRunAfterRestartTimed(rec taskInflight, files taskRunFiles, timedOut bool) {
	s.recordTaskRunAfterRestartStopped(rec, files, timedOut, false)
}

// recordTaskRunAfterRestartStopped is recordTaskRunAfterRestartTimed for a
// run the person may have stopped.
func (s *ScheduleStore) recordTaskRunAfterRestartStopped(rec taskInflight, files taskRunFiles, timedOut, stopped bool) {
	// Recorded already (SoluTerminal stopped after saving the run, before
	// removing these files): nothing to record twice. A record whose save
	// failed is recorded again; its history line is not written twice
	// (replayed).
	if cur, ok := s.Get(rec.SchedID); ok {
		for _, r := range cur.Runs {
			if r.ID != "" && r.ID == rec.RunID {
				files.remove()
				return
			}
		}
	}
	st := newTaskStream(files.events())
	st.finish()
	run := ScheduleRun{ID: rec.RunID, At: rec.Started, Trigger: rec.Trigger, Account: rec.Account, Model: rec.Model, replayed: true}
	end := time.Now()
	if fi, err := os.Stat(files.events()); err == nil && fi.ModTime().After(rec.Started) && !timedOut {
		end = fi.ModTime() // the last thing it wrote
	}
	endRun(&run, end)
	timeout := time.Duration(rec.TimeoutMs) * time.Millisecond
	out := classifyTaskRun(&run, st, false, nil, timedOut, rec.Permission, timeout)
	if files.eventsOver() && !timedOut {
		markTaskOverflow(&run)
	}
	if stopped && !timedOut {
		markStopped(&run)
	}
	if run.Cause != runCauseInterrupted {
		run.Detail += " · recorded after SoluTerminal restarted"
	}
	run.LogFile = writeRunLog(files.dir, rec.Started, append([]byte(rec.Header), out...))
	// The same end as a run this daemon saw: a one-time is done, "stop
	// after N" counts it, a spent usage limit holds task runs, and the files
	// go once the record is saved. Before the store starts it pushes nothing
	// (no pusher yet).
	s.recordTaskResult(rec.SchedID, run, out, files)
	log.Printf("scheduler: task %s run %s from before the restart recorded: status=%s", rec.SchedID, rec.RunID, run.Status)
}
