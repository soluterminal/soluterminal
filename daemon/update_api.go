package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"
)

// getDebugFleet reports the binary + agent-protocol inventory of the serving
// daemon and this Manager's pane agents. Debug only — it walks /proc.
func (a *apiServer) getDebugFleet(w http.ResponseWriter, _ *http.Request) {
	if a.fleet == nil {
		writeJSON(w, http.StatusOK, FleetStatus{Supported: false, UnsupportedReason: "fleet inventory is unavailable", Components: []FleetComponent{}})
		return
	}
	writeJSON(w, http.StatusOK, a.fleet.Status())
}

// updateStatus returns the cached update status. The web UI hits this
// on mount to paint the initial badge state. Subsequent state changes
// arrive via the SSE bus's "update-available" event, so the UI doesn't
// need to poll this endpoint on a timer.
func (a *apiServer) updateStatus(w http.ResponseWriter, _ *http.Request) {
	st := &UpdateStatus{Current: a.ver}
	if a.updater != nil {
		st = a.updater.Status()
	}
	writeJSON(w, 200, withBuildProvenance(st))
}

// withBuildProvenance adds the running daemon's build stamp to a status
// payload, so a hand-installed build is visible next to the version.
func withBuildProvenance(st *UpdateStatus) any {
	raw, err := json.Marshal(st)
	if err != nil {
		return st
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return st
	}
	body["build"] = buildProvenance()
	return body
}

// updateCheck forces a fresh manifest fetch, bypassing the 30-second
// background poll. Used by a "Check for updates" button if we expose
// one, and for the local end-to-end test (which should not wait 30 seconds).
func (a *apiServer) updateCheck(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		http.Error(w, "updater disabled", 503)
		return
	}
	st, err := a.updater.Check(r.Context())
	if err != nil {
		// Return the status anyway — the UI still wants to know what
		// version it's on and surface the error string in a tooltip.
		writeJSON(w, 200, st)
		return
	}
	writeJSON(w, 200, st)
}

// updateApply triggers the download + verify + swap. It writes the
// HTTP response BEFORE the daemon exits — Apply()'s exit is deferred
// 400ms specifically so this response can flush. The web UI's
// daemonOnline poll handles the brief unavailability while the Tauri
// shell respawns the new binary; existing pane WebSockets reconnect
// via their built-in retry loop.
// ?force=1 is the user's explicit override of the Focus gate, workflow drain,
// and defer backoff. It WILL interrupt in-flight workflow invocations. A normal
// client retries typed 409 holds without force until the daemon admits apply.
func (a *apiServer) updateApply(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		http.Error(w, "updater disabled", 503)
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if err := a.updater.Apply(r.Context(), os.Exit, force); err != nil {
		writeUpdateApplyError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"applying": true})
}

// updateApplyShell downloads the platform-specific Tauri installer
// (e.g. NSIS .exe on Windows) and launches it. Unlike updateApply,
// solud does NOT exit afterward — NSIS handles closing the running
// shell + replacing it. The web UI flips a "shellApplying" flag so
// the badge shows feedback while the user clicks through the UAC +
// installer prompts.
// ?force=1 is reserved for an explicit request to allow an otherwise-refused
// bundled-daemon downgrade; normal and automatic clients never send it.
//
// LOCAL-ONLY: an installer runs on the computer whose daemon launches it, so a
// request that arrived over the cloud relay (another computer, or a browser
// session on the web) is refused before anything is downloaded.
func (a *apiServer) updateApplyShell(w http.ResponseWriter, r *http.Request) {
	if refuseInstallerOverRelay(w, r) {
		return
	}
	if a.updater == nil {
		http.Error(w, "updater disabled", 503)
		return
	}
	// Keep the override deliberately narrow: only the exact documented query
	// authorizes a downgrade. Extra/duplicated parameters must not accidentally
	// turn an otherwise-normal automatic request into a forced one. The
	// unattended update is its own exact query and never forces.
	forceDowngrade := r.URL.RawQuery == "force=1"
	silent := r.URL.RawQuery == "silent=1"
	var path string
	var err error
	if silent {
		appPid, pidErr := silentAppPidFromRequest(w, r)
		if pidErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "shell_silent_bad_app_pid", "message": pidErr.Error()})
			return
		}
		path, err = a.updater.ApplyShellSilent(r.Context(), a.dataDir, appPid)
	} else {
		path, err = a.updater.ApplyShell(r.Context(), a.dataDir, forceDowngrade)
	}
	if err != nil {
		var paused *silentShellPausedError
		if errors.As(err, &paused) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code": "shell_silent_paused", "paused": true, "reason": paused.reason, "message": err.Error(),
			})
			return
		}
		if errors.Is(err, errSilentAppNotFound) {
			// The courier started, found no desktop app to wait for and installed
			// nothing. Its own code, so the page can retry sooner than after a
			// real failure.
			writeJSON(w, http.StatusConflict, map[string]any{
				"code": "shell_silent_app_not_found", "message": redactUpdateURLs(err.Error()),
			})
			return
		}
		if errors.Is(err, errSilentShellUnsupported) || errors.Is(err, errSilentCourierWaiting) {
			code := "shell_silent_unsupported"
			if errors.Is(err, errSilentCourierWaiting) {
				code = "shell_silent_waiting"
			}
			writeJSON(w, http.StatusConflict, map[string]any{"code": code, "message": err.Error()})
			return
		}
		var held *updateHeldError
		if errors.As(err, &held) {
			writeUpdateHeld(w, held)
			return
		}
		var downgrade *shellDaemonDowngradeError
		if errors.As(err, &downgrade) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code":             "shell_daemon_downgrade_refused",
				"installedVersion": downgrade.InstalledVersion,
				"installerVersion": downgrade.InstallerVersion,
				"message":          downgrade.Error(),
				"hint":             "Retry with ?force=1 only to explicitly allow this daemon downgrade.",
			})
			return
		}
		http.Error(w, redactUpdateURLs(err.Error()), 500)
		return
	}
	// silent:true tells the page the installer will run on its own once the
	// app exits. A daemon without the silent mode treats ?silent=1 as an
	// ordinary request, opens the installer window and omits it, and the page
	// then must not close the app as if nothing were left to do.
	writeJSON(w, 200, map[string]any{"applying": true, "installerPath": path, "silent": silent})
}

// silentAppPidFromRequest reads the optional {"appPid": N} body of a silent
// request: the desktop app's own process id, which the courier then waits on
// instead of matching by name. No body (an app too old to know its pid) is 0.
// Anything other than a whole number in 1..2^31-1 is refused, never guessed.
func silentAppPidFromRequest(w http.ResponseWriter, r *http.Request) (int, error) {
	if r.Body == nil {
		return 0, nil
	}
	var body struct {
		AppPid *int64 `json:"appPid"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, errors.New("the request body must be {\"appPid\": <the app's process id>}")
	}
	if body.AppPid == nil {
		return 0, nil
	}
	if *body.AppPid < 1 || *body.AppPid > maxSilentAppPid {
		return 0, errors.New("appPid must be a process id between 1 and 2147483647")
	}
	return int(*body.AppPid), nil
}

// refuseInstallerOverRelay answers 403 and reports true when r arrived over
// the cloud relay. Only a request made on this computer may launch the
// desktop installer (or open its folder) here; the web UI already sends a
// relayed session's shell update elsewhere, and this makes the daemon enforce
// it rather than trust the UI.
func refuseInstallerOverRelay(w http.ResponseWriter, r *http.Request) bool {
	if !requestViaRelay(r) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"code":    "installer_local_only",
		"message": "the desktop installer can only be launched from this computer, not over the cloud relay",
	})
	return true
}

func writeUpdateHeld(w http.ResponseWriter, held *updateHeldError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"code": "update_held", "heldVersion": held.version,
		"holdExpiresMs": held.expiresAtMs, "holdReason": held.reason,
	})
}

func writeUpdateAdmissionError(w http.ResponseWriter, err error) bool {
	var deferred *updateDeferredError
	if errors.As(err, &deferred) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": deferred.Error(), "deferred": true,
			"inflight": deferred.inflight, "nextRetryMs": deferred.nextRetryAt.UnixMilli(),
			"attempts": deferred.attempts,
		})
		return true
	}
	var busy *updateBusyError
	if errors.As(err, &busy) {
		body := map[string]any{"busy": true, "cause": busy.cause, "reason": busy.reason}
		if busy.paneID != "" {
			body["paneId"] = busy.paneID
		}
		if busy.inflight != 0 {
			body["workflowInflight"] = busy.inflight
		}
		if busy.deadlineMs != 0 {
			body["workflowDeferDeadlineMs"] = busy.deadlineMs
		}
		writeJSON(w, http.StatusConflict, body)
		return true
	}
	return false
}

func (a *apiServer) updateRollbackTargets(w http.ResponseWriter, _ *http.Request) {
	if a.updater == nil || a.rollback == nil {
		http.Error(w, "rollback unavailable", http.StatusServiceUnavailable)
		return
	}
	snapshot := a.rollback.Snapshot()
	targets := make([]map[string]any, 0, len(snapshot.History))
	for _, item := range snapshot.History {
		if item.Version == a.updater.current || !isNewer(a.updater.current, item.Version) {
			continue
		}
		eligible := true
		reason := ""
		a.updater.mu.Lock()
		ctrl := a.updater.ctrlEffective
		a.updater.mu.Unlock()
		if blocked, why := ctrl.blocksRollback(item.Version, a.updater.current); blocked {
			eligible, reason = false, why
		}
		targets = append(targets, map[string]any{
			"version": item.Version, "ranAtMs": item.RanAtMs, "eligible": eligible, "reason": reason,
		})
	}
	var hold any
	if active, ok := a.rollback.ActiveHold(time.Now()); ok {
		hold = active
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": a.updater.current, "targets": targets, "hold": hold, "lastAttempt": snapshot.LastAttempt,
	})
}

func (a *apiServer) updateRollback(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil || a.rollback == nil {
		http.Error(w, "rollback unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		TargetVersion string `json:"targetVersion"`
		HoldHours     int    `json:"holdHours"`
		Force         bool   `json:"force"`
		Confirm       bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !body.Confirm {
		http.Error(w, "explicit confirm:true is required", http.StatusBadRequest)
		return
	}
	if !validReleaseVersion(body.TargetVersion) {
		http.Error(w, "invalid targetVersion", http.StatusBadRequest)
		return
	}
	req := rollbackRequest{TargetVersion: body.TargetVersion, HoldHours: body.HoldHours, Force: body.Force}
	if err := a.updater.Rollback(r.Context(), req, os.Exit); err != nil {
		if writeUpdateAdmissionError(w, err) {
			return
		}
		var held *updateHeldError
		if errors.As(err, &held) {
			writeUpdateHeld(w, held)
			return
		}
		var incompatible *rollbackIncompatibleError
		if errors.As(err, &incompatible) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code": "rollback_incompatible", "store": incompatible.Store,
				"requiredReader": incompatible.RequiredReader, "targetReader": incompatible.TargetReader,
				"targetVersion": incompatible.TargetVersion, "message": incompatible.Error(),
			})
			return
		}
		http.Error(w, redactUpdateURLs(err.Error()), http.StatusInternalServerError)
		return
	}
	snapshot := a.rollback.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"applying": true, "targetVersion": body.TargetVersion, "hold": snapshot.Hold,
	})
}

func (a *apiServer) updateHoldRelease(w http.ResponseWriter, _ *http.Request) {
	if a.updater == nil || a.rollback == nil {
		http.Error(w, "rollback unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := a.rollback.ClearHold("released by user"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a.updater.Status())
}

// updateShellReveal opens the containing folder for the last staged installer.
// The path comes exclusively from updater state; clients cannot turn this into
// an arbitrary Explorer launch endpoint. LOCAL-ONLY, like apply-shell: it opens
// a window on this computer's desktop.
func (a *apiServer) updateShellReveal(w http.ResponseWriter, r *http.Request) {
	if refuseInstallerOverRelay(w, r) {
		return
	}
	if a.updater == nil {
		http.Error(w, "updater disabled", http.StatusServiceUnavailable)
		return
	}
	path, err := a.updater.revealStagedInstaller()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}

// writeUpdateApplyError maps an Apply/ApplyLocal failure to the response the
// web client already understands (deferred / busy / held / plain error).
func writeUpdateApplyError(w http.ResponseWriter, err error) {
	var deferred *updateDeferredError
	if errors.As(err, &deferred) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": deferred.Error(), "deferred": true,
			"inflight": deferred.inflight, "nextRetryMs": deferred.nextRetryAt.UnixMilli(),
			"attempts": deferred.attempts,
		})
		return
	}
	var busy *updateBusyError
	if errors.As(err, &busy) {
		body := map[string]any{"busy": true, "cause": busy.cause, "reason": busy.reason}
		if busy.paneID != "" {
			body["paneId"] = busy.paneID
		}
		if busy.inflight != 0 {
			body["workflowInflight"] = busy.inflight
		}
		if busy.deadlineMs != 0 {
			body["workflowDeferDeadlineMs"] = busy.deadlineMs
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	var held *updateHeldError
	if errors.As(err, &held) {
		writeUpdateHeld(w, held)
		return
	}
	http.Error(w, redactUpdateURLs(err.Error()), 500)
}
