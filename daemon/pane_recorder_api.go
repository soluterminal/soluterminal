package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"unicode"
)

// Pane recorder routes. No route serves a recording's bytes: there is a
// status, the switch for one pane or every pane of a workspace (on is the
// local person's only; off is anyone's) with the notice dismissal, the
// person's delete, and the settings (raising is the person's; lowering is
// anyone's).

// paneRecordingStatus reports on or off, when it started, the byte counts and
// why it stopped. Agents may read it: knowing a pane is recorded is good.
func (a *apiServer) paneRecordingStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d := *p.recordingEventDTO()
	d.Check = a.mgr.RecordingCheck(p)
	writeJSON(w, http.StatusOK, &d)
}

// paneRecordingReq is the one-pane switch. Hours and Override go with on:true
// only: the time limit and the rule codes the person chose to override in the
// start dialog. A request without them gets the defaults.
type paneRecordingReq struct {
	On       *bool    `json:"on,omitempty"`
	Dismiss  bool     `json:"dismiss,omitempty"`
	Hours    int      `json:"hours,omitempty"`
	Override []string `json:"override,omitempty"`
	// Pid is the pane process the person saw when they confirmed.
	Pid int `json:"pid,omitempty"`
}

// recordingRequestMaxBody bounds every recorder request body.
const recordingRequestMaxBody = 16 << 10

// decodeRecordingJSON decodes exactly one JSON object with no unknown fields,
// no duplicate keys and nothing after it.
func decodeRecordingJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err == nil {
		err = rejectDuplicateJSONKeys(raw)
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(dst); err == nil && dec.More() {
			err = errors.New("trailing data")
		}
		if err == nil {
			if _, tail := dec.Token(); tail != io.EOF {
				err = errors.New("trailing data")
			}
		}
	}
	if err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// rejectDuplicateJSONKeys walks raw JSON and refuses an object that names a
// key twice (encoding/json would silently keep the last). Keys compare
// case-folded as encoding/json matches field names (Unicode simple folding).
func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object  bool
		keys    map[string]bool
		wantKey bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				if top != nil && top.object {
					top.wantKey = true
				}
				stack = append(stack, &frame{object: t == '{', keys: map[string]bool{}, wantKey: t == '{'})
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		default:
			if top != nil && top.object {
				if top.wantKey {
					key, _ := t.(string)
					key = foldJSONKey(key)
					if top.keys[key] {
						return fmt.Errorf("duplicate key %q", key)
					}
					top.keys[key] = true
					top.wantKey = false
				} else {
					top.wantKey = true
				}
			}
		}
	}
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
	switch a.recordingPersonRefusal(r, id) {
	case "":
		a.logPersonAct(id, action)
		return true
	case recordingRefusedTransport:
		http.Error(w, "only the person at this computer can "+action, http.StatusForbidden)
	case recordingRefusedUnproven:
		http.Error(w, "approve this device first: only a proven person at this computer can "+action, http.StatusForbidden)
	default:
		// As requireHuman: the caller's own refusal message.
		a.refuseCaller(w, id, action, false)
	}
	return false
}

// Why recordingPersonRefusal refuses a caller.
const (
	recordingRefusedTransport = "transport"
	recordingRefusedUnproven  = "unproven"
	recordingRefusedNotHuman  = "not_human"
)

// recordingPersonRefusal is the one test of "the person at this computer"
// for the recorder (Addendum E 1): "" when r, from caller id, is the person,
// else why not. admitRecordingPerson answers with it; recordingPersonQuiet
// only asks it.
func (a *apiServer) recordingPersonRefusal(r *http.Request, id callerIdentity) string {
	signed := id.Device != ""
	switch {
	case requestViaRelay(r) || !serviceRequestLocal(r, a.backendID, signed) || !serviceOwnOrigin(r, signed):
		return recordingRefusedTransport
	case id.Kind == callerHuman && id.Unproven:
		return recordingRefusedUnproven
	case id.Kind != callerHuman:
		return recordingRefusedNotHuman
	}
	return ""
}

// recordingPersonQuiet asks recordingPersonRefusal without answering or
// logging: whether the person at this computer made r (a lowering's reach).
func (a *apiServer) recordingPersonQuiet(r *http.Request) bool {
	return a.recordingPersonRefusal(r, a.identifyCallerQuietly(r)) == ""
}

func (a *apiServer) setPaneRecording(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req paneRecordingReq
	if !decodeRecordingJSON(w, r, &req, recordingRequestMaxBody) {
		return
	}
	on := req.On != nil && *req.On
	if !on && (req.Hours != 0 || len(req.Override) > 0 || req.Pid != 0) {
		http.Error(w, `"hours" and "override" go with {"on":true} only`, http.StatusBadRequest)
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
		opts := recordingStartOptions{Hours: req.Hours, Overrides: req.Override, Pid: req.Pid}
		if _, err := a.mgr.StartRecording(p, recordingTransport(r), opts); err != nil {
			http.Error(w, err.Error(), recordingStartStatus(err))
			return
		}
	default:
		http.Error(w, `send {"on":true}, {"on":false} or {"dismiss":true}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, p.recordingEventDTO())
}

// foldJSONKey maps each rune to the smallest rune of its simple case-folding
// orbit, so keys encoding/json would treat as one field compare equal.
func foldJSONKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}

// recordingStartStatus is the HTTP status for a refused switch-on.
func recordingStartStatus(err error) int {
	var refused recordingRefusedError
	var bad errRecordingBadRequest
	switch {
	case errors.Is(err, errRecordingPlatform):
		return http.StatusNotImplemented
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.As(err, &refused), errors.Is(err, errRecordingNotRunning), errors.Is(err, errRecordingPaneChanged),
		errors.Is(err, errRecordingFolder), errors.Is(err, errRecordingCap):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// recordingSkipCode names why a pane was skipped, as a code.
func recordingSkipCode(err error) string {
	var refused recordingRefusedError
	switch {
	case errors.As(err, &refused):
		return string(refused.rule)
	case errors.Is(err, errRecordingNotRunning):
		return "not_running"
	case errors.Is(err, errRecordingPaneChanged):
		return "pane_changed"
	case errors.Is(err, errRecordingPlatform):
		return "windows"
	case errors.Is(err, errRecordingFolder):
		return "recordings_folder"
	case errors.Is(err, errRecordingCap):
		return "disk_cap"
	}
	return "error"
}

// workspaceRecordingReq records, or stops, every pane of one workspace. On:
// Panes are the panes the person confirmed, each with its own overrides;
// a workspace pane not listed (opened after the confirm) is skipped.
type workspaceRecordingReq struct {
	Workspace string                      `json:"workspace"`
	On        *bool                       `json:"on"`
	Hours     int                         `json:"hours,omitempty"`
	Panes     []workspaceRecordingPaneReq `json:"panes,omitempty"`
}

type workspaceRecordingPaneReq struct {
	ID       string   `json:"id"`
	Override []string `json:"override,omitempty"`
	// Pid is the pane process shown in the confirm (refused if it changed).
	Pid int `json:"pid,omitempty"`
}

type workspaceRecordingSkip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type workspaceRecordingResp struct {
	Recorded []string                 `json:"recorded"`
	Stopped  []string                 `json:"stopped,omitempty"`
	Skipped  []workspaceRecordingSkip `json:"skipped"`
}

// recordingWorkspaceMaxPanes bounds one workspace request.
const recordingWorkspaceMaxPanes = 256

// setWorkspaceRecording is "record every pane in this workspace" (amendment
// section 4): one switch over the panes the person confirmed, taken at this
// moment only. Panes opened, moved in or respawned later are never added.
// Off stops every recording pane in the workspace and is anyone's.
func (a *apiServer) setWorkspaceRecording(w http.ResponseWriter, r *http.Request) {
	var req workspaceRecordingReq
	if !decodeRecordingJSON(w, r, &req, recordingRequestMaxBody) {
		return
	}
	if req.On == nil || req.Workspace == "" || len(req.Panes) > recordingWorkspaceMaxPanes {
		http.Error(w, `send {"workspace":…,"on":true,"panes":[…]} or {"workspace":…,"on":false}`, http.StatusBadRequest)
		return
	}
	if !*req.On && (req.Hours != 0 || len(req.Panes) > 0) {
		http.Error(w, `"hours" and "panes" go with "on":true only`, http.StatusBadRequest)
		return
	}
	if *req.On {
		if !recordingPlatformSupported() {
			http.Error(w, errRecordingPlatform.Error(), http.StatusNotImplemented)
			return
		}
		if !a.admitRecordingPerson(w, r, "start recording a workspace") {
			return
		}
	}
	if a.ws == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ws, ok := a.ws.Get(req.Workspace)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	via := recordingTransport(r)
	resp := workspaceRecordingResp{Recorded: []string{}, Skipped: []workspaceRecordingSkip{}}
	if !*req.On {
		who := via + " " + string(a.identifyCallerQuietly(r).Kind)
		for _, id := range ws.PaneIds {
			if p, ok := a.mgr.Get(id); ok && a.mgr.StopRecording(p, who) {
				resp.Stopped = append(resp.Stopped, id)
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// Validate the whole request before switching anything on.
	if _, err := (recordingStartOptions{Hours: req.Hours}).terms(recordingVerdict{}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	confirmed := map[string]workspaceRecordingPaneReq{}
	for _, pr := range req.Panes {
		if _, dup := confirmed[pr.ID]; dup {
			http.Error(w, "a pane is listed twice", http.StatusBadRequest)
			return
		}
		for _, code := range pr.Override {
			if !recordingOverridable[recordingRule(code)] {
				http.Error(w, fmt.Sprintf("%q can't be overridden", code), http.StatusBadRequest)
				return
			}
		}
		confirmed[pr.ID] = pr
	}
	members := map[string]bool{}
	for _, id := range ws.PaneIds {
		members[id] = true
		p, live := a.mgr.Get(id)
		if !live {
			// A browser or file tile, or a pane that is gone: a confirmed one
			// is answered as not running.
			if _, ok := confirmed[id]; ok {
				resp.Skipped = append(resp.Skipped, workspaceRecordingSkip{ID: id, Reason: "not_running"})
			}
			continue
		}
		pr, ok := confirmed[id]
		if !ok {
			resp.Skipped = append(resp.Skipped, workspaceRecordingSkip{ID: id, Reason: "not_confirmed"})
			continue
		}
		if p.recording.active.Load() != nil {
			// Already recording, under the terms it was switched on with.
			resp.Skipped = append(resp.Skipped, workspaceRecordingSkip{ID: id, Reason: "already_recording"})
			continue
		}
		if _, err := a.mgr.StartRecording(p, via, recordingStartOptions{Hours: req.Hours, Overrides: pr.Override, Pid: pr.Pid}); err != nil {
			resp.Skipped = append(resp.Skipped, workspaceRecordingSkip{ID: id, Reason: recordingSkipCode(err)})
			continue
		}
		resp.Recorded = append(resp.Recorded, id)
	}
	for _, pr := range req.Panes {
		if !members[pr.ID] {
			resp.Skipped = append(resp.Skipped, workspaceRecordingSkip{ID: pr.ID, Reason: "not_in_workspace"})
		}
	}
	log.Printf("recording: workspace switch-on by the person (%s): %d recorded, %d skipped", via, len(resp.Recorded), len(resp.Skipped))
	writeJSON(w, http.StatusOK, resp)
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

// recordingView takes a view record from the person's own window (amendment
// section 6, Addenda A and B): gated exactly as switch-on, strict, at most
// 4 KB, written only while something records (else 409, nothing written).
func (a *apiServer) recordingView(w http.ResponseWriter, r *http.Request) {
	// The same predicate as switch-on, without a log line per record (a window
	// sends several a second while a recording is on).
	if why := a.recordingPersonRefusal(r, a.identifyCallerQuietly(r)); why != "" {
		http.Error(w, "only the person at this computer can send a view record", http.StatusForbidden)
		return
	}
	var req recordingViewReq
	if !decodeRecordingJSON(w, r, &req, recordingViewMaxBytes) {
		return
	}
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	indexes, err := a.mgr.RecordingView(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": indexes})
}
