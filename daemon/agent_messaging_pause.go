package main

// The person's one-click stop for agent-to-agent messaging (ST-238 slice 1).
//
// While it is on, nothing an agent sends reaches another pane, and nothing
// is dropped either:
//   - ask_pane and send_input messages to an agent pane are HELD on that
//     pane (pane_draft_guard.go), with their sender, text and this reason,
//     where the person can send or discard each one. They go in, in order,
//     once messaging resumes. They do not expire while paused.
//   - what cannot be held (keys, text to a pane with no agent, answer_prompt,
//     spawn_pane) is refused with a notice, so the caller keeps its request.
//   - queued workers stay queued, and schedules an agent filed are held and
//     run once messaging resumes.
// Only the person turns it on or off (requireHuman): an agent could
// otherwise lift the stop it is meant to be under. It is public: the team
// build's "Pause all autonomy" (autonomy_*.go) stays private and does not
// hold messages.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const agentMessagingFile = "agent-messaging.json"

// agentMessagingPausedReason is the held reason on a pane's held messages.
const agentMessagingPausedReason = "agent messaging is paused"

// agentMessagingScheduleReason is the PausedReason of an agent's schedule
// held by the stop; resume runs it once and turns it back on.
const agentMessagingScheduleReason = "held while agent messaging is paused; runs when it resumes"

type agentMessagingState struct {
	Paused bool `json:"paused"`
	// Since: when the pause started, Unix ms.
	Since int64 `json:"since,omitempty"`
}

var agentMessaging struct {
	// mu orders writes (file then flag); reads use the atomics.
	mu     sync.Mutex
	paused atomic.Bool
	since  atomic.Int64
	// resumedAt restarts the held-message expiry clock on resume, so a
	// message held overnight still gets its full wait afterwards.
	resumedAt atomic.Int64
	// warning is shown to the person until a later save succeeds: the saved
	// setting could not be read, or the state in force could not be saved.
	// Guarded by mu.
	warning string
	// heldAtBoot: the saved setting could not be read, so boot left the
	// automations the stop held in place. The next successful save with
	// messaging on releases them. Guarded by mu.
	heldAtBoot bool
}

const (
	agentMessagingUnreadableWarning = "The saved agent-messaging setting couldn't be read, so agent messaging is on."
	agentMessagingPauseUnsaved      = "Paused for now, but the setting couldn't be saved: restarting SoluTerminal would lift the pause."
	agentMessagingResumeUnsaved     = "Resumed for now, but the setting couldn't be saved: restarting SoluTerminal would pause agent messaging again."
)

// agentMessagingWarningKind names a warning for the chip's short label:
// "unreadable" or "unsaved".
func agentMessagingWarningKind(w string) string {
	if w == agentMessagingUnreadableWarning {
		return "unreadable"
	}
	return "unsaved"
}

// agentMessagingWarning is the notice the person should see, or "".
func agentMessagingWarning() string {
	agentMessaging.mu.Lock()
	defer agentMessaging.mu.Unlock()
	return agentMessaging.warning
}

// agentMessagingPaused reports whether the person has stopped agent-to-agent
// messaging.
func agentMessagingPaused() bool { return agentMessaging.paused.Load() }

// agentMessagingSnapshot is the current state.
func agentMessagingSnapshot() agentMessagingState {
	if !agentMessaging.paused.Load() {
		return agentMessagingState{}
	}
	return agentMessagingState{Paused: true, Since: agentMessaging.since.Load()}
}

// heldClockStart is when a held message's expiry clock started: when it was
// held, or the last resume if later. Paused time never counts.
func heldClockStart(at int64) time.Time {
	if r := agentMessaging.resumedAt.Load(); r > at {
		return time.UnixMilli(r)
	}
	return time.UnixMilli(at)
}

// loadAgentMessaging reads the saved state. Anything but a readable file that
// says paused reads as ON (messages flow): a damaged file must never stop
// every agent with no visible cause.
func loadAgentMessaging(dataDir string) {
	agentMessaging.mu.Lock()
	defer agentMessaging.mu.Unlock()
	agentMessaging.paused.Store(false)
	agentMessaging.since.Store(0)
	agentMessaging.resumedAt.Store(0)
	agentMessaging.warning = ""
	agentMessaging.heldAtBoot = false
	if dataDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, agentMessagingFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("agent messaging: could not read %s (%v); messaging stays on", agentMessagingFile, err)
			agentMessaging.warning = agentMessagingUnreadableWarning
			agentMessaging.heldAtBoot = true
		}
		return
	}
	var st agentMessagingState
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("agent messaging: %s is not valid (%v); messaging stays on", agentMessagingFile, err)
		agentMessaging.warning = agentMessagingUnreadableWarning
		agentMessaging.heldAtBoot = true
		return
	}
	if st.Paused {
		since := st.Since
		if since <= 0 {
			since = time.Now().UnixMilli()
		}
		agentMessaging.since.Store(since)
		agentMessaging.paused.Store(true)
		log.Printf("agent messaging: paused since %s (saved)", time.UnixMilli(since).Format(time.RFC3339))
	}
}

// setAgentMessagingPaused applies the state at once, then saves it. A stop
// never silently fails: a pause (or resume) whose save fails still applies
// for as long as this daemon runs, and the returned warning, kept until a
// later save succeeds, tells the person a restart would undo it. It reports
// whether the state changed.
func setAgentMessagingPaused(dataDir string, paused bool, now time.Time) (bool, error) {
	agentMessaging.mu.Lock()
	defer agentMessaging.mu.Unlock()
	changed := agentMessaging.paused.Load() != paused
	if changed {
		since := int64(0)
		if paused {
			since = now.UnixMilli()
		} else {
			agentMessaging.resumedAt.Store(now.UnixMilli())
		}
		agentMessaging.since.Store(since)
		agentMessaging.paused.Store(paused)
	} else if agentMessaging.warning == "" {
		// Nothing new to save. A failed save is tried again below, and an
		// unreadable file is replaced: the person's answer to that notice.
		return false, nil
	}
	if dataDir == "" {
		agentMessaging.warning = ""
		return changed, nil
	}
	raw, _ := json.Marshal(agentMessagingState{Paused: paused, Since: agentMessaging.since.Load()})
	if err := writeFileAtomicSynced(filepath.Join(dataDir, agentMessagingFile), raw, 0600); err != nil {
		switch {
		case !changed && agentMessaging.warning == agentMessagingUnreadableWarning:
			// A Dismiss whose save failed: nothing changed, so the notice
			// (and its Dismiss, to try again) stays as it was.
		case paused:
			agentMessaging.warning = agentMessagingPauseUnsaved
		default:
			agentMessaging.warning = agentMessagingResumeUnsaved
		}
		return changed, fmt.Errorf("could not save the setting: %w", err)
	}
	agentMessaging.warning = ""
	return changed, nil
}

// agentMessagingRefusal is the tool error for what cannot be held.
func agentMessagingRefusal(what string) string {
	return fmt.Sprintf("agent messaging is paused by the person, so %s: nothing was sent or started. Do not retry in a loop; wait, or tell the user what you need. Messages sent with ask_pane to an agent pane are held, not refused, and go in when messaging resumes.", what)
}

// heldMessagesTotal counts the messages held on every pane.
func (a *apiServer) heldMessagesTotal() int {
	if a == nil || a.mgr == nil {
		return 0
	}
	n := 0
	for _, p := range a.mgr.List() {
		n += len(p.Snapshot().HeldMessages)
	}
	return n
}

func (a *apiServer) agentMessagingBody() map[string]any {
	st := agentMessagingSnapshot()
	body := map[string]any{"paused": st.Paused, "held": a.heldMessagesTotal()}
	if st.Since > 0 {
		body["since"] = st.Since
	}
	if w := agentMessagingWarning(); w != "" {
		body["warning"] = w
		body["warningKind"] = agentMessagingWarningKind(w)
	}
	return body
}

// agentMessagingBootDelay: held schedules released at boot run this long
// after it, once restored panes are back.
var agentMessagingBootDelay = 30 * time.Second

// resumeAgentMessagingHeld releases what the stop held outside the panes:
// the agents' schedules, after delay. Held pane messages drain by themselves
// (their drain polls) and queued workers start on the next queue tick.
func (a *apiServer) resumeAgentMessagingHeld(delay time.Duration) {
	if a != nil && a.scheds != nil {
		a.scheds.resumeAgentMessagingHeld(delay)
	}
}

// agentMessagingReleasesAtBoot: after loadAgentMessaging, whether boot
// releases the schedules the stop held. A pause lifted before a restart must
// not leave them held for good. But when the file could not be read, the
// person has not seen that notice yet: they stay held until the person
// resumes, or dismisses the notice (PUT with the state in force).
func agentMessagingReleasesAtBoot() bool {
	agentMessaging.mu.Lock()
	defer agentMessaging.mu.Unlock()
	return !agentMessaging.paused.Load() && !agentMessaging.heldAtBoot
}

// takeAgentMessagingBootHold reports, once, that the automations boot left
// held may now be released: messaging is on and its setting is saved.
func takeAgentMessagingBootHold() bool {
	agentMessaging.mu.Lock()
	defer agentMessaging.mu.Unlock()
	if !agentMessaging.heldAtBoot || agentMessaging.paused.Load() || agentMessaging.warning != "" {
		return false
	}
	agentMessaging.heldAtBoot = false
	return true
}

func init() {
	registerBootStep(bootStores, func(b *bootEnv) {
		loadAgentMessaging(b.dataDir)
		if agentMessagingReleasesAtBoot() && b.api != nil {
			b.api.resumeAgentMessagingHeld(agentMessagingBootDelay)
		}
	})
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/agent-messaging", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, a.agentMessagingBody())
		})
		mux.HandleFunc("PUT /api/agent-messaging", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Paused *bool `json:"paused"`
			}
			if !decodeJSON(w, r, &req, 256) {
				return
			}
			if req.Paused == nil {
				http.Error(w, `send {"paused": true} or {"paused": false}`, http.StatusBadRequest)
				return
			}
			action := "resume agent messaging"
			if *req.Paused {
				action = "pause agent messaging"
			}
			if !a.requireHuman(w, r, action) {
				return
			}
			noticeShown := agentMessagingWarning() == agentMessagingUnreadableWarning
			// Applied even when the save fails; the body's warning says so.
			changed, err := setAgentMessagingPaused(a.dataDir, *req.Paused, time.Now())
			if err != nil {
				log.Printf("agent messaging: %v (applied for now)", err)
			}
			if changed {
				outcome := "resumed"
				if *req.Paused {
					outcome = "paused"
				}
				coordLog.record(coordinationEvent{Kind: "messaging", By: "person", Outcome: outcome})
				log.Printf("agent messaging: %s by the person", outcome)
			}
			// A resume releases what the pause held, saved or not (the stop
			// applies in memory either way).
			release := changed && !*req.Paused
			// What boot held back for an unreadable setting is released by
			// the first successful save that leaves messaging on: a Dismiss
			// (or its retry), or a resume. A Dismiss whose save fails
			// releases nothing and keeps the notice.
			if !*req.Paused && takeAgentMessagingBootHold() {
				if !changed && noticeShown {
					coordLog.record(coordinationEvent{Kind: "messaging", By: "person", Outcome: "notice_dismissed"})
					log.Printf("agent messaging: unreadable-setting notice dismissed by the person; setting saved as on")
				}
				release = true
			}
			if release {
				a.resumeAgentMessagingHeld(0)
			}
			writeJSON(w, http.StatusOK, a.agentMessagingBody())
		})
	})
}
