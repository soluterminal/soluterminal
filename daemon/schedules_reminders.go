package main

// Reminder insurance: one-time schedules with NotifyOnRun exist to reach the
// user's phone at a specific moment — but the local scheduler only runs while
// this machine is on. The daemon therefore mirrors its UPCOMING one-time
// reminders to the cloud (POST /api/internal/reminders, replace semantics),
// and a cloud sweeper delivers the push at run-at time if this daemon didn't.
//
// The contract is deliberately confirmation-free: after a local fire the
// schedule auto-disables, drops out of the upcoming set, and the very next
// sync removes its cloud row — so "row still present past run-at" IS the
// "daemon didn't handle it" signal. A daemon that fires and crashes before
// syncing produces at worst one duplicate push, which the clients coalesce
// by tag ("sched:<id>").
//
// Sync triggers piggyback on notifyChanged (every create/update/delete/
// enable/run) behind a debounce, plus SetPusher (cloud connect) so a fresh
// pairing uploads the existing set. A set the cloud already accepted is not
// re-sent: every run notifies, so on a machine whose built-in agents fire
// every few minutes this used to POST the same (usually empty) set about 700
// times a day. reminderResyncEvery still re-sends an unchanged set now and
// then, which keeps the replace-semantics self-healing if the cloud loses it.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"soluterminal-shared/machineauth"
)

// reminderSyncDebounce batches bursts (e.g. a run recording + auto-disable +
// save all notify within milliseconds) into one POST.
var reminderSyncDebounce = 2 * time.Second

// reminderResyncEvery bounds how long an unchanged reminder set goes without
// being re-sent.
var reminderResyncEvery = 6 * time.Hour

type reminderSyncItem struct {
	SchedID string    `json:"schedId"`
	Name    string    `json:"name"`
	RunAt   time.Time `json:"runAt"`
}

// SetBackendID records which daemon this is when several share one machine
// claim (e.g. "wsl" + "win") so their cloud reminder rows don't clobber each
// other. Safe for concurrent use; set once at boot.
func (s *ScheduleStore) SetBackendID(id string) {
	s.mu.Lock()
	s.backendID = id
	s.mu.Unlock()
}

// collectUpcomingReminders snapshots the one-time, enabled, notify-on-run
// schedules whose moment is still ahead. Caller must hold s.mu.
func (s *ScheduleStore) collectUpcomingRemindersUnlocked(now time.Time) []reminderSyncItem {
	out := []reminderSyncItem{} // non-nil: an empty sync CLEARS the cloud set
	for _, id := range s.order {
		sched, ok := s.items[id]
		if !ok || !sched.oneTime() || !sched.Enabled || !sched.NotifyOnRun {
			continue
		}
		if sched.RunAt == nil || !sched.RunAt.After(now) {
			continue
		}
		out = append(out, reminderSyncItem{SchedID: sched.ID, Name: sched.Name, RunAt: *sched.RunAt})
	}
	return out
}

// scheduleReminderSync arms (or re-arms) the debounced sync. Called from
// notifyChanged and SetPusher; cheap no-op when no cloud is connected.
func (s *ScheduleStore) scheduleReminderSync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pusher == nil {
		return
	}
	if s.reminderSyncTimer != nil {
		s.reminderSyncTimer.Stop()
	}
	s.reminderSyncTimer = time.AfterFunc(reminderSyncDebounce, s.syncRemindersNow)
}

func (s *ScheduleStore) syncRemindersNow() {
	now := time.Now()
	s.mu.Lock()
	pusher := s.pusher
	backendID := s.backendID
	items := s.collectUpcomingRemindersUnlocked(now)
	s.mu.Unlock()
	if pusher == nil {
		return
	}
	// What the cloud would receive, privacy substitution included, so turning
	// "Only devices I approve" on or off still re-sends the names.
	key, _ := json.Marshal(map[string]any{"backendId": backendID, "reminders": pusher.reminderPayload(items)})
	s.mu.Lock()
	unchanged := string(key) == s.lastReminderSync && now.Sub(s.lastReminderSyncAt) < reminderResyncEvery
	s.mu.Unlock()
	if unchanged {
		return
	}
	if pusher.SyncReminders(backendID, items) {
		s.mu.Lock()
		s.lastReminderSync, s.lastReminderSyncAt = string(key), now
		s.mu.Unlock()
	}
}

// reminderPayload is what the cloud may see of the reminders: with only
// approved devices allowed (Pusher.private), the name the person gave the
// reminder is replaced, like push text (minimisePushBody); the cloud only
// needs the id and the time to deliver the safety-net push.
func (p *Pusher) reminderPayload(items []reminderSyncItem) []reminderSyncItem {
	if p.private == nil || !p.private() {
		return items
	}
	out := make([]reminderSyncItem, len(items))
	for i, it := range items {
		it.Name = "Reminder"
		out[i] = it
	}
	return out
}

// SyncReminders POSTs the machine's full upcoming-reminder set to the cloud
// (replace semantics — idempotent and self-healing after either side loses
// state). Best-effort: a failed sync just means the cloud safety net lags
// until the next schedules change; the local scheduler is unaffected.
// Reports whether the cloud took the set (an older cloud's 404 counts: it
// will not want the same set again either).
func (p *Pusher) SyncReminders(backendID string, items []reminderSyncItem) bool {
	token := p.getToken()
	if token == "" {
		return false
	}
	body, _ := json.Marshal(map[string]any{
		"backendId": backendID,
		"reminders": p.reminderPayload(items),
	})
	req, err := http.NewRequest("POST", p.cloudBase+"/api/internal/reminders", bytes.NewReader(body))
	if err != nil {
		return false
	}
	machineauth.SignRequest(req, token, body, time.Now())
	resp, err := p.client.Do(req)
	if err != nil {
		slog.Warn("reminder sync failed", "err", err)
		return false
	}
	defer resp.Body.Close()
	// 404 = older cloud without the endpoint — fine, feature simply absent.
	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		slog.Warn("reminder sync rejected", "http", resp.StatusCode)
		return false
	}
	slog.Debug("reminder sync ok", "count", len(items), "http", resp.StatusCode)
	return true
}
