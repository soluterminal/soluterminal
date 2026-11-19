package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Pause all automations (Automations stage 2, ST-299 item 12; secure's
// condition 2): one switch that holds every automation, whatever its kind
// (prompts, tasks, built-in agents, catch-up runs) and whoever made it. "Pause
// all autonomy" sets it too.
//
// While paused, a run that comes due is held, not run and not lost: the
// automation is switched off with the reason (allPausedReason) and its owed
// run is kept. On resume each held automation runs once and a recurring one
// is armed again. A run already going is left to finish (Stop ends it), and
// the person's own Run now still works. The switch is saved, so it holds
// across a restart.

const (
	automationsPausedFile = "automations-paused.json"
	allPausedReason       = "all automations are paused; runs once when they resume"
	// pauseSourceAutonomy marks a pause made by "Pause all autonomy": only
	// its resume clears it; a pause the person made here stays.
	pauseSourceAutonomy = "autonomy"
)

// automationsPause is the switch, as saved and as GET /pause-all answers.
type automationsPause struct {
	Paused bool       `json:"paused"`
	Since  *time.Time `json:"since,omitempty"`
	By     string     `json:"by,omitempty"`
	Source string     `json:"source,omitempty"`
}

func (s *ScheduleStore) pausePath() string {
	return filepath.Join(filepath.Dir(s.path), automationsPausedFile)
}

// loadAllPaused reads the saved switch (at start).
func (s *ScheduleStore) loadAllPaused() {
	data, err := os.ReadFile(s.pausePath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var p automationsPause
	if err == nil {
		err = json.Unmarshal(data, &p)
		if err != nil {
			quarantineCorruptState(s.pausePath(), err)
		}
	}
	if err != nil {
		// A switch that cannot be read fails closed: paused, so nothing
		// held runs until the person resumes.
		log.Printf("scheduler: the pause-all switch could not be read (%v); starting paused", err)
		now := time.Now()
		p = automationsPause{Paused: true, Since: &now, By: "SoluTerminal (its pause switch could not be read)"}
	}
	s.allPaused = p
}

// AllPaused reports the switch.
func (s *ScheduleStore) AllPaused() automationsPause {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allPaused
}

// errNotThisPause: "Pause all autonomy"'s resume does not lift a pause the
// person made here.
var errNotThisPause = errors.New("the automations were paused on their own, not by Pause all autonomy")

// SetAllPaused turns the switch on or off. source is "autonomy" for Pause all
// autonomy and its resume, "" for the Automations switch. Turning it off
// releases the holds: each held automation runs once, recurring ones are
// armed again.
func (s *ScheduleStore) SetAllPaused(on bool, actor ScheduleActor, source string) error {
	s.mu.Lock()
	cur := s.allPaused
	if !on && cur.Paused && source == pauseSourceAutonomy && cur.Source != pauseSourceAutonomy {
		s.mu.Unlock()
		return errNotThisPause
	}
	if on == cur.Paused {
		s.mu.Unlock()
		return nil
	}
	now := time.Now()
	if on {
		// The switch is saved first: if it can't be, nothing changes.
		next := automationsPause{Paused: true, Since: &now, By: actor.By, Source: source}
		if err := writePause(s.pausePath(), next); err != nil {
			s.mu.Unlock()
			return err
		}
		s.allPaused = next
		// A prompt waiting for its pane keeps waiting (its pane-identity
		// checks intact) but types nothing while paused (fireAs).
		s.mu.Unlock()
		log.Printf("scheduler: all automations paused by %q (source %q)", actor.By, source)
		s.notifyChanged()
		return nil
	}
	// Resume: the switch is saved first; then each hold is released and
	// saved, and its run started, one at a time. A stop (or a failed write)
	// part way leaves the rest held, and the next start releases them
	// (releaseHeldAfterPause).
	if err := writePause(s.pausePath(), automationsPause{}); err != nil {
		s.mu.Unlock()
		return err
	}
	s.allPaused = automationsPause{}
	s.mu.Unlock()
	log.Printf("scheduler: all automations resumed by %q (source %q)", actor.By, source)
	s.releaseHeldAfterPause()
	s.notifyChanged()
	return nil
}

// releaseHeldAfterPause releases each automation still held by a pause that
// is over: saved released, then its owed run started. On resume, and at start
// for holds a stop left behind.
func (s *ScheduleStore) releaseHeldAfterPause() {
	s.mu.Lock()
	var held []string
	if !s.allPaused.Paused {
		for _, id := range s.order {
			if sched := s.items[id]; sched != nil && sched.PausedReason == allPausedReason && scheduleAvailable(sched) {
				held = append(held, id)
			}
		}
	}
	s.mu.Unlock()
	for _, id := range held {
		s.mu.Lock()
		sched, ok := s.items[id]
		if !ok || s.allPaused.Paused || sched.PausedReason != allPausedReason {
			s.mu.Unlock()
			continue
		}
		owed := s.releaseHoldLocked(sched, allPausedReason, time.Now())
		if err := s.saveUnlocked(); err != nil {
			// Not saved: held again, for the next try.
			s.holdForPauseLocked(sched)
			s.mu.Unlock()
			log.Printf("scheduler: could not release %s after the pause: %v", id, err)
			continue
		}
		s.mu.Unlock()
		s.notifyScheduleChanged(id)
		if owed {
			go s.fire(id)
		}
	}
}

func writePause(path string, p automationsPause) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(path, data, 0o600)
}

// holdForPauseLocked holds an automation while all are paused: off, with the
// reason, its owed run kept for the resume. Caller holds s.mu.
func (s *ScheduleStore) holdForPauseLocked(sched *Schedule) {
	s.unscheduleUnlocked(sched.ID)
	if sched.Enabled {
		s.noteChangeLocked(sched, scheduleChangeDisabled, scheduleActorDaemon, "all automations are paused", time.Now())
	}
	sched.Enabled = false
	sched.PausedReason = allPausedReason
}

// releaseHoldLocked lifts a hold with reason why: a recurring automation is
// armed again; it reports whether the held run is still owed (it is, unless
// a one-time one already ran). Caller holds s.mu.
func (s *ScheduleStore) releaseHoldLocked(sched *Schedule, why string, now time.Time) bool {
	if sched.PausedReason != why {
		return false
	}
	sched.PausedReason = ""
	if sched.oneTime() {
		if sched.FireCount > 0 {
			return false
		}
		sched.Enabled = true
		return true
	}
	s.unscheduleUnlocked(sched.ID)
	sched.Enabled = true
	sched.LastArmedAt = &now
	if err := s.scheduleUnlocked(sched); err != nil {
		sched.Enabled = false
		log.Printf("scheduler: could not re-arm %q after automations resumed: %v", sched.Name, err)
		return false
	}
	return true
}

// pauseAllRequest is POST /api/schedules/pause-all's body.
type pauseAllRequest struct {
	Paused *bool `json:"paused"` // required: a missing value changes nothing
}

func (a *apiServer) getPauseAll(w http.ResponseWriter, r *http.Request) {
	if a.scheds == nil {
		writeJSON(w, 200, automationsPause{})
		return
	}
	writeJSON(w, 200, a.scheds.AllPaused())
}

// setPauseAll is the Automations switch: the person's, both ways.
func (a *apiServer) setPauseAll(w http.ResponseWriter, r *http.Request) {
	var req pauseAllRequest
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if req.Paused == nil {
		http.Error(w, `"paused" must be true or false`, 400)
		return
	}
	doing := "resume all automations"
	if *req.Paused {
		doing = "pause all automations"
	}
	if !a.requireHuman(w, r, doing, scheduleMCPTools...) {
		return
	}
	if a.scheds == nil {
		http.Error(w, "schedules unavailable", 404)
		return
	}
	if err := a.scheds.SetAllPaused(*req.Paused, scheduleActorPerson, ""); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, a.scheds.AllPaused())
}

func init() {
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/schedules/pause-all", a.getPauseAll)
		mux.HandleFunc("POST /api/schedules/pause-all", a.setPauseAll)
	})
}
