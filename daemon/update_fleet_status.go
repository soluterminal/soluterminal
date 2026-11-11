package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"soluterminal-shared/machineauth"
	"soluterminal-shared/updatestatus"
)

// Never send raw errors: HTTP errors can contain authenticated URLs, response
// bodies and local paths. Even UI-suppressed transient failures are reported.
func fleetCheckError(err error) string {
	if err == nil {
		return ""
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "network"
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "signature") {
		return "signature"
	}
	if strings.Contains(s, "manifest") || strings.Contains(s, "parse") {
		return "manifest"
	}
	return "check-failed"
}

func (u *Updater) fleetUpdateStatus() updatestatus.Status {
	// The preference and busy callbacks acquire their own locks. Never call
	// them holding the updater lock (Apply uses the same lock ordering).
	enabled := u.autoApplyWhenIdle != nil && u.autoApplyWhenIdle()
	u.mu.Lock()
	s := u.statusCopy()
	ctrl := u.ctrlEffective
	r := updatestatus.Status{AutoUpdate: enabled || s.Required, Channel: "stable",
		LastCheckMs: s.LastCheckedMs, LastCheckError: u.fleetLastCheckError,
		LastSuccessMs: u.fleetLastSuccessMs, LatestVersion: s.Latest,
		RunningFlavor: s.RunningFlavor, UpdateChannel: s.UpdateChannel}
	if _, ahead := previewChangesAhead(u.current); ahead > 0 || strings.Count(u.current, ".") >= 3 {
		r.Channel = "preview"
	}
	if !r.AutoUpdate {
		r.DisabledReason = "preference"
		if os.Getenv("SOLU_AUTO_UPDATE") == "0" {
			r.DisabledReason = "environment"
		}
	}
	if u.current == "dev" {
		r.AutoUpdate, r.DisabledReason = false, "dev-build"
	}
	if u.fleetPollDisabled != "" {
		r.AutoUpdate, r.DisabledReason = false, u.fleetPollDisabled
	}
	if isNewer(s.Latest, u.current) && s.Latest != "" {
		if blocked, _ := ctrl.blocksApply(s.Latest, u.current, u.machineID); blocked {
			switch {
			case ctrl.Paused:
				r.Reason = "kill-switch"
			case ctrl.HaltAtOrAbove != "" && compareVersions(s.Latest, ctrl.HaltAtOrAbove) >= 0:
				r.Reason = "quarantined"
			case ctrl.HaltBelow != "" && compareVersions(s.Latest, ctrl.HaltBelow) < 0:
				r.Reason = "floor"
			default:
				r.Reason = "rollout-cohort"
			}
		} else if s.HoldVersion == s.Latest {
			r.Reason = "version-hold"
		}
	}
	if r.Reason == "" {
		switch {
		case s.WorkflowDeferred:
			r.Reason = "drain"
		case u.swapped:
			r.Reason = "restart-pending"
		case u.applying:
			r.Reason = "applying"
		case u.restartGate != nil && u.restartGate.Load():
			r.Reason = "restart-pending"
		}
	}
	u.mu.Unlock()
	if r.Reason == "" && s.Available && u.busyCheck != nil {
		if busy := u.checkBusy(); busy != nil {
			r.Reason = "busy"
		}
	}
	return r.Sanitize()
}

// Ignore timestamp-only changes: checks run every 30s. Transitions are sampled
// every 30s, with a five-minute heartbeat; failures retry at that same bounded
// cadence. A long-lived relay need not reconnect to refresh fleet diagnostics.
type fleetStatusReporter struct {
	last          updatestatus.Status
	lastAttempt   time.Time
	lastDelivered bool
}

func (r *fleetStatusReporter) due(s updatestatus.Status, now time.Time) bool {
	a, b := s, r.last
	a.LastCheckMs, b.LastCheckMs = 0, 0
	if !r.lastAttempt.IsZero() && now.Sub(r.lastAttempt) < 30*time.Second {
		return false
	}
	return r.lastAttempt.IsZero() || now.Sub(r.lastAttempt) >= 5*time.Minute || (r.lastDelivered && a != b)
}

func (u *Updater) startFleetStatusReporter(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		u.mu.Lock()
		if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") == "1" && u.fleetLastSuccessMs == 0 {
			u.fleetLastSuccessMs = time.Now().UnixMilli()
		}
		u.mu.Unlock()
		var reporter fleetStatusReporter
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			s := u.fleetUpdateStatus()
			now := time.Now()
			if reporter.due(s, now) {
				u.cloudCredsMu.Lock()
				base, token := u.cloudBase, u.cloudToken
				u.cloudCredsMu.Unlock()
				if base != "" && token != "" {
					reporter.lastAttempt, reporter.last = now, s
					reporter.lastDelivered = postFleetStatus(ctx, base, token, s)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func postFleetStatus(ctx context.Context, base, token string, s updatestatus.Status) bool {
	body, _ := json.Marshal(s.Sanitize())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/internal/update-status", bytes.NewReader(body))
	if err != nil {
		return false
	}
	machineauth.SignRequest(req, token, body, time.Now())
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
