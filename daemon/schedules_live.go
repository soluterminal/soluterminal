package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Run now with live output, and Stop (Automations stage 2, ST-299 item 12).
//
// A task run's output streams into its events file (schedules_task_stream.go);
// while it runs, GET /api/schedules/{id}/live returns what the assistant has
// written so far: its text only, never a tool's output, which stays in the
// events file. POST /api/schedules/{id}/stop is the person's: it stops the run
// in flight (its process tree; a run followed after a restart only once its
// process is confirmed as the run), or ends a prompt's wait for its pane. A
// stopped run is recorded as stopped by the person and is never retried.
// Agents have no tool for either.

// liveTextMax bounds what /live returns: the newest part of the text. The
// stream keeps a rolling tail of this size (taskStream.tail), apart from the
// report's buffer, which keeps the start: so the live text keeps moving.
const liveTextMax = 64 << 10

// liveRun is a run in flight that the person can watch and stop.
type liveRun struct {
	runID   string
	started time.Time
	stream  *taskStream // nil for a built-in agent's run
	stop    func() error
	// stopping: a Stop is under way; stopped: it took (the process tree was
	// signalled). A failed stop clears stopping and leaves stopped false.
	stopping bool
	stopped  bool
}

// setLive registers schedID's run in flight.
func (s *ScheduleStore) setLive(schedID string, r *liveRun) {
	s.mu.Lock()
	if s.liveRuns == nil {
		s.liveRuns = map[string]*liveRun{}
	}
	s.liveRuns[schedID] = r
	s.mu.Unlock()
}

// endLive removes schedID's run in flight and says whether the person stopped
// it.
func (s *ScheduleStore) endLive(schedID string, r *liveRun) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveRuns[schedID] == r {
		delete(s.liveRuns, schedID)
	}
	return r != nil && r.stopped
}

// liveView is what GET /live answers.
type liveView struct {
	Running   bool       `json:"running"`
	RunID     string     `json:"runId,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Model     string     `json:"model,omitempty"`
	// Text is the assistant's text so far (its newest liveTextMax bytes);
	// Clipped says older text was left out.
	Text    string `json:"text,omitempty"`
	Clipped bool   `json:"clipped,omitempty"`
	// Waiting: a prompt waiting for its pane (it can be stopped too), and
	// WaitID which wait it is, for Stop.
	Waiting string `json:"waiting,omitempty"`
	WaitID  string `json:"waitId,omitempty"`
}

// waitID names one wait for a pane: when it began.
func waitID(w *paneWait) string { return strconv.FormatInt(w.since.UnixNano(), 10) }

// Live is the run in flight of an automation, as far as it has got.
func (s *ScheduleStore) Live(id string) liveView {
	s.mu.Lock()
	r := s.liveRuns[id]
	v := liveView{}
	if w := s.paneWaits[id]; w != nil {
		// Copied under the lock: waitForPane updates the reason.
		v.Waiting, v.WaitID = w.why, waitID(w)
		if v.Waiting == "" {
			v.Waiting = "waiting for its pane"
		}
	}
	s.mu.Unlock()
	if r == nil {
		return v
	}
	v.Running, v.RunID = true, r.runID
	at := r.started
	v.StartedAt = &at
	if r.stream != nil {
		v.Text, v.Clipped, v.Model = r.stream.liveText(liveTextMax)
	}
	return v
}

// liveText is the assistant's text so far, its newest max bytes, and the
// model. The stream's own reader keeps it current; it is read here, not
// read further.
func (t *taskStream) liveText(max int) (text string, clipped bool, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.tail
	if len(b) > max {
		b, clipped = runeSafeTailBytes(b, max), true
	}
	return string(b), clipped || t.tailClipped, t.model
}

// errNothingToStop: the automation has no run in flight and no prompt waiting.
var errNothingToStop = errors.New("nothing is running")

// errStaleStop: the run the person saw (and pressed Stop on) has ended; what
// runs now, if anything, was not the one they meant.
var errStaleStop = errors.New("that run has already ended")

// errBeingSent: the waiting prompt is being typed into its pane at this
// moment; it cannot be taken back.
var errBeingSent = errors.New("it is being typed into its pane right now")

// StopRun stops an automation's run in flight, or ends its prompt's wait for
// its pane. The person's action (POST /stop is requireHuman). runID names the
// run the person saw ("" for a prompt's wait): Stop never reaches a run that
// began after it.
func (s *ScheduleStore) StopRun(id, runID, waitIDSeen string) (string, error) {
	s.mu.Lock()
	r := s.liveRuns[id]
	if r != nil {
		if runID == "" || r.runID != runID {
			s.mu.Unlock()
			return "", errStaleStop
		}
		if r.stopping || r.stopped {
			s.mu.Unlock()
			return "stopping", nil
		}
		// Marked before the signal, so the run's own end (woken by it)
		// always sees it; taken back if the signal fails.
		r.stopping, r.stopped = true, true
		stop := r.stop
		s.mu.Unlock()
		var err error
		if stop != nil {
			err = stop()
		}
		s.mu.Lock()
		r.stopping = false
		if err != nil {
			r.stopped = false
		}
		s.mu.Unlock()
		if errors.Is(err, errStaleStop) {
			return "", err
		}
		if err != nil {
			return "", fmt.Errorf("could not stop the run: %w", err)
		}
		return "stopped", nil
	}
	if runID != "" {
		s.mu.Unlock()
		return "", errStaleStop
	}
	w, waiting := s.paneWaits[id]
	if waiting && (waitIDSeen == "" || waitIDSeen != waitID(w)) {
		// Another wait than the one the person saw.
		s.mu.Unlock()
		return "", errStaleStop
	}
	if waiting && s.promptRunning[id] {
		s.mu.Unlock()
		return "", errBeingSent
	}
	sched, ok := s.items[id]
	var paneID string
	if ok {
		paneID = sched.PaneID
	}
	if waiting {
		s.dropPaneWaitLocked(id)
		// A retry already admitted for this wait sees the stamp change
		// and types nothing (fireAs, waitForPane).
		s.bumpEditUnlocked(id)
		_ = s.saveUnlocked()
	}
	s.mu.Unlock()
	if !waiting {
		return "", errNothingToStop
	}
	// This is the run's attempt: a one-time reminder is done (not left on
	// to be counted as missed at the next start).
	s.recordRun(id, ScheduleRun{At: time.Now(), PaneID: paneID, Status: "skipped", Trigger: runTriggerRetry, Cause: runCauseStopped,
		Detail: "stopped by you while it waited for its pane"})
	return "stopped", nil
}

// markStopped records a run the person stopped.
func markStopped(run *ScheduleRun) {
	run.Status = "error"
	run.Outcome = ""
	run.Cause = runCauseStopped
	run.Detail = "stopped by you"
	if run.DurationMs > 0 {
		run.Detail += " after " + fmtShortDuration(time.Duration(run.DurationMs)*time.Millisecond)
	}
	run.Summary = "stopped by you"
}

func fmtShortDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

// stopProcessTree stops a process this daemon started (and its children).
func stopProcessTree(p *os.Process) func() error {
	return func() error { return killChildTree(p) }
}

func (a *apiServer) scheduleLive(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		http.Error(w, "schedule not found", 404)
		return
	}
	sched, ok := a.scheds.Get(r.PathValue("id"))
	if !ok || !scheduleListed(sched) {
		http.Error(w, "schedule not found", 404)
		return
	}
	writeJSON(w, 200, a.scheds.Live(sched.ID))
}

func (a *apiServer) stopSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "stop an automation's run", scheduleMCPTools...) {
		return
	}
	id := r.PathValue("id")
	if a.scheduleMutationRefused(w, id) {
		return
	}
	if _, ok := a.scheds.Get(id); !ok {
		http.Error(w, "schedule not found", 404)
		return
	}
	outcome, err := a.scheds.StopRun(id, r.URL.Query().Get("run"), r.URL.Query().Get("wait"))
	switch {
	case errors.Is(err, errNothingToStop), errors.Is(err, errStaleStop), errors.Is(err, errBeingSent):
		http.Error(w, err.Error(), 409)
		return
	case err != nil:
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]string{"outcome": outcome})
}

func init() {
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/schedules/{id}/live", a.scheduleLive)
		mux.HandleFunc("POST /api/schedules/{id}/stop", a.stopSchedule)
	})
}
