package main

import (
	"errors"
	"net/http"
)

// Pane recorder routes. No route serves a recording's bytes: there is a
// status, the switch (on is the local person's only; off is anyone's) with the
// notice dismissal, and the person's delete.

// paneRecordingStatus reports on or off, when it started, the byte counts and
// why it stopped. Agents may read it: knowing a pane is recorded is good.
func (a *apiServer) paneRecordingStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, p.recordingEventDTO())
}

type paneRecordingReq struct {
	On      *bool `json:"on,omitempty"`
	Dismiss bool  `json:"dismiss,omitempty"`
}

// recordingTransport names the caller's transport for the daemon log.
func recordingTransport(r *http.Request) string {
	if requestViaRelay(r) {
		return "relay"
	}
	return "local"
}

// admitRecordingPerson is the switch-on, dismiss and delete gate, restartService's:
// refused over the relay and from anything but this computer's own app page,
// then human-only (callerHuman). A person judged on browser headers alone
// (untraced, no approved device's signature) is refused too, whatever the
// human-proof mode. Agent permissions never admit an agent here.
func (a *apiServer) admitRecordingPerson(w http.ResponseWriter, r *http.Request, action string) bool {
	id := a.identifyCaller(r)
	signed := id.Device != ""
	if requestViaRelay(r) || !serviceRequestLocal(r, a.backendID, signed) || !serviceOwnOrigin(r, signed) {
		http.Error(w, "only the person at this computer can "+action, http.StatusForbidden)
		return false
	}
	if id.Kind == callerHuman && id.Unproven {
		http.Error(w, "approve this device first: only a proven person at this computer can "+action, http.StatusForbidden)
		return false
	}
	return a.requireHuman(w, r, action)
}

func (a *apiServer) setPaneRecording(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req paneRecordingReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	switch {
	case req.Dismiss && req.On == nil:
		if !a.admitRecordingPerson(w, r, "dismiss a recording notice") {
			return
		}
		a.mgr.DismissRecordingNotice(p)
	case req.On != nil && !req.Dismiss && !*req.On:
		// Off is the safe direction: anyone, agents and the phone included.
		a.mgr.StopRecording(p, recordingTransport(r)+" "+string(a.identifyCallerQuietly(r).Kind))
	case req.On != nil && !req.Dismiss && *req.On:
		if !recordingPlatformSupported() {
			http.Error(w, errRecordingPlatform.Error(), http.StatusNotImplemented)
			return
		}
		if !a.admitRecordingPerson(w, r, "start recording a pane") {
			return
		}
		if _, err := a.mgr.StartRecording(p, recordingTransport(r)); err != nil {
			status := http.StatusInternalServerError
			var refused recordingRefusedError
			switch {
			case errors.Is(err, errRecordingPlatform):
				status = http.StatusNotImplemented
			case errors.As(err, &refused), errors.Is(err, errRecordingNotRunning),
				errors.Is(err, errRecordingFolder), errors.Is(err, errRecordingCap):
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	default:
		http.Error(w, `send {"on":true}, {"on":false} or {"dismiss":true}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, p.recordingEventDTO())
}

// deletePaneRecordings deletes every recording of one pane, stopping a live
// one first. The person's only.
func (a *apiServer) deletePaneRecordings(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !a.admitRecordingPerson(w, r, "delete a pane's recordings") {
		return
	}
	n, err := a.mgr.DeletePaneRecordings(p, recordingTransport(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}
