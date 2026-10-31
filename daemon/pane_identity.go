package main

// A pane's identity beyond its title: a short handle and what it is working on.
//
// The handle ("api-fix", shown as @api-fix) is a name agents and the user can
// address a pane by. Pane numbers move when panes close and restart; a handle
// stays with its pane, so it is unique among the daemon's panes and persisted.
// "Working on" is one line about the pane's current work, set by the user or
// by the agent in it, and kept with who set it and when. Origin.Purpose is
// what the pane was started for; WorkingOn is what it is doing now.
//
//   POST /api/panes/{id}/identity {handle?, workingOn?}  — absent keeps, "" clears
//   MCP set_pane_info {pane_id?, handle?, working_on?}    — your own pane or one you started

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type PaneIdentity struct {
	Handle      string `json:"handle,omitempty"`
	WorkingOn   string `json:"workingOn,omitempty"`
	WorkingOnAt int64  `json:"workingOnAt,omitempty"`
	// WorkingOnBy is "user", or the label of the agent pane that set it
	// ("#2 Claude"), as it was at that moment.
	WorkingOnBy string `json:"workingOnBy,omitempty"`
}

func (i *PaneIdentity) clone() *PaneIdentity {
	if i == nil {
		return nil
	}
	c := *i
	return &c
}

func (i *PaneIdentity) empty() bool {
	return i == nil || (i.Handle == "" && i.WorkingOn == "")
}

const (
	paneHandleMaxLen    = 24
	paneWorkingOnMaxLen = 140
)

// A handle is lowercase letters, digits and dashes, starts with a letter or
// digit, and has at least one letter so it can never read as a pane number.
var paneHandleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var (
	errPaneHandleInvalid = errors.New("a handle is 1–24 lowercase letters, digits or dashes, with at least one letter (e.g. api-fix)")
	errPaneIdentityEmpty = errors.New("nothing to set: pass handle and/or working_on")
)

type paneHandleTakenError struct{ owner string }

func (e paneHandleTakenError) Error() string {
	return "that handle is taken by " + e.owner + "; choose another"
}

// normalizePaneHandle accepts "@Api-Fix" or "api-fix" and returns "api-fix".
// Empty input is a valid clear.
func normalizePaneHandle(raw string) (string, error) {
	h := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
	if h == "" {
		return "", nil
	}
	if len(h) > paneHandleMaxLen || !paneHandleRe.MatchString(h) || !strings.ContainsAny(h, "abcdefghijklmnopqrstuvwxyz") {
		return "", errPaneHandleInvalid
	}
	return h, nil
}

// normalizeWorkingOn keeps one line of at most paneWorkingOnMaxLen runes.
func normalizeWorkingOn(raw string) string {
	s := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(s) <= paneWorkingOnMaxLen {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:paneWorkingOnMaxLen-1])) + "…"
}

// paneHandleMu serializes every check-then-claim of a handle: an identity
// edit, and the reservation a spawn or restore holds until its pane is in the
// manager. paneHandleReserved holds those in-flight claims.
var (
	paneHandleMu       sync.Mutex
	paneHandleReserved = map[string]bool{}
)

// reservePaneHandle claims a free handle for a pane about to be created. The
// caller releases it once the pane exists (it then holds the handle itself)
// or the creation failed.
func (a *apiServer) reservePaneHandle(handle string) (release func(), err error) {
	paneHandleMu.Lock()
	defer paneHandleMu.Unlock()
	if err := a.checkPaneHandleFreeLocked(handle, ""); err != nil {
		return nil, err
	}
	paneHandleReserved[handle] = true
	return func() {
		paneHandleMu.Lock()
		delete(paneHandleReserved, handle)
		paneHandleMu.Unlock()
	}, nil
}

// panesByHandle returns the panes holding handle, other than exceptID. More
// than one only if something bypassed the reservation; callers treat that as
// ambiguous rather than pick one.
func (a *apiServer) panesByHandle(handle, exceptID string) []*Pane {
	if handle == "" || a.mgr == nil {
		return nil
	}
	var out []*Pane
	for _, p := range a.mgr.List() {
		if p.ID == exceptID {
			continue
		}
		if id := p.Snapshot().Identity; id != nil && id.Handle == handle {
			out = append(out, p)
		}
	}
	return out
}

func (a *apiServer) paneByHandle(handle, exceptID string) (*Pane, bool) {
	if ps := a.panesByHandle(handle, exceptID); len(ps) > 0 {
		return ps[0], true
	}
	return nil, false
}

func (a *apiServer) paneLabelFor(p *Pane) string {
	snap := p.Snapshot()
	return paneLabel(paneSlotsByID(a.ws)[snap.ID], snap.Title, paneSummaryAgent(p, snap))
}

// checkPaneHandleFreeLocked reports a taken or reserved handle as an error
// naming its holder. Callers hold paneHandleMu.
func (a *apiServer) checkPaneHandleFreeLocked(handle, exceptID string) error {
	if handle == "" {
		return nil
	}
	if paneHandleReserved[handle] {
		return paneHandleTakenError{owner: "a pane being started"}
	}
	if owner, taken := a.paneByHandle(handle, exceptID); taken {
		return paneHandleTakenError{owner: a.paneLabelFor(owner)}
	}
	// A pane being resumed, refreshed or switched is briefly out of the
	// manager but keeps its saved record (only closing a pane removes it), so
	// its handle stays its own across the gap.
	if a.panes != nil {
		for _, rec := range a.panes.List() {
			if rec.ID != exceptID && rec.Identity != nil && rec.Identity.Handle == handle {
				if _, live := a.mgr.Get(rec.ID); !live {
					return paneHandleTakenError{owner: "the pane " + strconvQuote(rec.Title) + " while it restarts"}
				}
			}
		}
	}
	return nil
}

func strconvQuote(s string) string {
	if strings.TrimSpace(s) == "" {
		return `""`
	}
	return `"` + s + `"`
}

func (p *Pane) SetIdentity(i *PaneIdentity) {
	p.mu.Lock()
	p.Identity = i
	p.mu.Unlock()
}

// updatePaneIdentity applies a change (nil keeps a field, "" clears it),
// enforces handle uniqueness, and persists, reverting on a failed write.
func (a *apiServer) updatePaneIdentity(p *Pane, handle, workingOn *string, by string, now time.Time) (*PaneIdentity, error) {
	if handle == nil && workingOn == nil {
		return nil, errPaneIdentityEmpty
	}
	paneHandleMu.Lock()
	defer paneHandleMu.Unlock()
	prev := p.Snapshot().Identity
	next := prev.clone()
	if next == nil {
		next = &PaneIdentity{}
	}
	if handle != nil {
		h, err := normalizePaneHandle(*handle)
		if err != nil {
			return nil, err
		}
		if err := a.checkPaneHandleFreeLocked(h, p.ID); err != nil {
			return nil, err
		}
		next.Handle = h
	}
	if workingOn != nil {
		next.WorkingOn = normalizeWorkingOn(*workingOn)
		next.WorkingOnAt, next.WorkingOnBy = 0, ""
		if next.WorkingOn != "" {
			next.WorkingOnAt, next.WorkingOnBy = now.UnixMilli(), by
		}
	}
	if next.empty() {
		next = nil
	}
	p.SetIdentity(next)
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.SetIdentity(prev)
			return nil, fmt.Errorf("persist pane: %w", err)
		}
	}
	return next, nil
}

type paneIdentityReq struct {
	Handle    *string `json:"handle"`
	WorkingOn *string `json:"workingOn"`
}

// setPaneIdentity handles POST /api/panes/{id}/identity from the app.
func (a *apiServer) setPaneIdentity(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req paneIdentityReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	id, err := a.updatePaneIdentity(p, req.Handle, req.WorkingOn, "user", time.Now())
	if err != nil {
		http.Error(w, err.Error(), paneIdentityErrStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": id})
}

func paneIdentityErrStatus(err error) int {
	var taken paneHandleTakenError
	switch {
	case errors.As(err, &taken):
		return http.StatusConflict
	case errors.Is(err, errPaneHandleInvalid), errors.Is(err, errPaneIdentityEmpty):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// mcpToolSetPaneInfo sets a handle and/or working-on line on the caller's own
// pane (the default) or on a pane the caller started. Another pane's identity
// is the user's to change, so it needs user_requested like kill_pane.
func (a *apiServer) mcpToolSetPaneInfo(args map[string]any, callerPane string) any {
	return a.mcpToolSetPaneInfoAs(args, paneCaller{ID: callerPane})
}

// mcpToolSetPaneInfoAs is mcpToolSetPaneInfo for a caller that may only claim a pane
// (paneCaller.Claimed): it owns nothing, and the prompt says so.
func (a *apiServer) mcpToolSetPaneInfoAs(args map[string]any, c paneCaller) any {
	callerPane := c.ID
	target := strings.TrimSpace(mcpArgString(args, "pane_id"))
	if target == "" {
		target = strings.TrimSpace(callerPane)
	}
	if target == "" {
		return toolErr("pane_id is required (your own pane id is in SOLU_PANE_ID)")
	}
	p, ok := a.mgr.Get(target)
	if !ok {
		return toolErr("no pane " + target + " (call list_panes for ids)")
	}
	var handle, workingOn *string
	if v, ok := args["handle"].(string); ok {
		handle = &v
	}
	if v, ok := args["working_on"].(string); ok {
		workingOn = &v
	}
	// An agent names itself and the panes it started; anyone else's pane
	// (including one without an origin record) only with the person's OK.
	asked, _ := args["user_requested"].(bool)
	switch a.paneAgentAuthority(p.Snapshot().Origin, p.ID, callerPane, asked) {
	case paneAgentRefused:
		return toolErr(a.paneConfirmTarget(p) + " was not started by you, so you can't change its handle or working_on. If the user asked you to, call set_pane_info again with user_requested: true and they will be asked to allow it in the SoluTerminal app. Otherwise leave it: they can change it themselves.")
	case paneAgentNeedsOK:
		if handle == nil && workingOn == nil {
			return toolErr("set handle and/or working_on")
		}
		name := a.paneConfirmTarget(p)
		caller := a.paneConfirmCaller(c)
		change := paneInfoChangeText(handle, workingOn)
		key := paneConfirmChange + "\x00" + c.bucket() + "\x00" + p.ID + "\x00" + paneInfoChangeKey(handle, workingOn)
		req, err := a.requestPaneConfirmFor(key, c.bucket(), paneConfirmView{
			Action:       paneConfirmChange,
			CallerPaneID: strings.TrimSpace(callerPane),
			Caller:       caller,
			PaneID:       p.ID,
			Pane:         name,
			Title:        "Change " + name + "?",
			Message:      caller + " wants to change " + name + ": " + change + ".",
		}, func() map[string]any {
			target, ok := a.mgr.Get(p.ID)
			if !ok {
				return toolErr(name + " has closed; nothing was changed.")
			}
			return a.applyPaneInfo(target, handle, workingOn, callerPane)
		})
		if err != nil {
			return toolErr(err.Error())
		}
		return awaitPaneConfirm(req, paneConfirmWaitingText(name+" is not changed yet"))
	}
	return a.applyPaneInfo(p, handle, workingOn, callerPane)
}

// paneInfoChangeText describes a set_pane_info change for the person:
// `handle → @api-fix; working on → “Fixing the API”`.
func paneInfoChangeText(handle, workingOn *string) string {
	var parts []string
	if handle != nil {
		if h, err := normalizePaneHandle(*handle); err == nil && h != "" {
			parts = append(parts, "handle → @"+h)
		} else if strings.TrimSpace(*handle) == "" {
			parts = append(parts, "clear the handle")
		} else {
			parts = append(parts, "handle → "+strconvQuote(clipRunes(oneLine(*handle), 40)))
		}
	}
	if workingOn != nil {
		if w := clipRunes(oneLine(*workingOn), paneWorkingOnMaxLen); w != "" {
			parts = append(parts, "working on → “"+w+"”")
		} else {
			parts = append(parts, "clear the working-on line")
		}
	}
	return strings.Join(parts, "; ")
}

func paneInfoChangeKey(handle, workingOn *string) string {
	k := func(s *string) string {
		if s == nil {
			return "\x01"
		}
		return *s
	}
	return k(handle) + "\x00" + k(workingOn)
}

// applyPaneInfo sets the handle and/or working-on line once allowed.
func (a *apiServer) applyPaneInfo(p *Pane, handle, workingOn *string, callerPane string) map[string]any {
	by := "agent"
	if c, ok := a.mgr.Get(callerPane); ok {
		by = a.paneLabelFor(c)
	}
	id, err := a.updatePaneIdentity(p, handle, workingOn, by, time.Now())
	if err != nil {
		return toolErr(err.Error())
	}
	label := a.paneLabelFor(p)
	if id == nil {
		return toolOK("Cleared the handle and working_on of " + label + ".")
	}
	parts := []string{}
	if id.Handle != "" {
		parts = append(parts, "handle @"+id.Handle)
	}
	if id.WorkingOn != "" {
		parts = append(parts, "working on: "+id.WorkingOn)
	}
	return toolOK(label + " now has " + strings.Join(parts, "; ") + ".")
}

// resolvePaneHandleRef maps "@handle" (or a bare handle) to its pane id.
func (a *apiServer) resolvePaneHandleRef(ref string) string {
	h, err := normalizePaneHandle(ref)
	if err != nil || h == "" {
		return ""
	}
	if ps := a.panesByHandle(h, ""); len(ps) == 1 {
		return ps[0].ID
	}
	return ""
}

// resolvePaneIDArgument rewrites a tool's pane_id from "#3" or "@api-fix" to
// the pane's id before any guard or handler reads it, so every pane tool
// accepts the same references. An unresolvable value is left for the handler
// to report.
func (a *apiServer) resolvePaneIDArgument(args map[string]any, callerPane string) {
	ref, ok := args["pane_id"].(string)
	if !ok || strings.TrimSpace(ref) == "" {
		return
	}
	if id := a.resolvePaneRef(ref, callerPane); id != "" && id != ref {
		args["pane_id"] = id
	}
}

// restoredPaneIdentity is the identity a Restore brings back: only on Restore,
// normalized, and without a handle another pane holds by now. A kept handle
// is reserved; call release once the pane exists or failed to start.
func (a *apiServer) restoredPaneIdentity(req createPaneReq) (id *PaneIdentity, release func()) {
	release = func() {}
	if !req.Restore || req.Identity == nil {
		return nil, release
	}
	id = req.Identity.clone()
	id.WorkingOn = normalizeWorkingOn(id.WorkingOn)
	if h, err := normalizePaneHandle(id.Handle); err != nil {
		id.Handle = ""
	} else {
		id.Handle = h
	}
	if id.Handle != "" {
		if r, err := a.reservePaneHandle(id.Handle); err != nil {
			id.Handle = ""
		} else {
			release = r
		}
	}
	if id.empty() {
		return nil, release
	}
	return id, release
}

func identityHandle(i *PaneIdentity) string {
	if i == nil {
		return ""
	}
	return i.Handle
}

func identityWorkingOn(i *PaneIdentity) string {
	if i == nil {
		return ""
	}
	return i.WorkingOn
}
