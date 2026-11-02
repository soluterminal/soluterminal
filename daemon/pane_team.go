package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// pane_team.go owns explicit leads and workers.
//
// A LEAD is a role pane (security, CMO, social…): long-lived, owns an area,
// has a stable role name, and is never archived automatically. A lead another
// pane started (a sub-lead) keeps that starter in its origin and is still a
// lead; the sidebar shows every lead as a peer, with who started it.
// A WORKER has no role of its own: it is a task for the pane that started it,
// and once finished, read by its lead and pushed it is archived
// (worker_archives.go). A pane with no team kind is the user's own.
//
// The kind is saved in the daemon's PaneOrigin record (pane store). Like
// origin.worker it never comes from client-writable workspace data: a restore
// takes it only from the daemon's own record of the same conversation
// (validatedWorkerOrigin).
const (
	paneTeamLead   = "lead"
	paneTeamWorker = "worker"

	leadRoleMaxRunes = 32
)

var (
	errLeadRoleRequired = errors.New("a lead needs a role name, e.g. \"security\"")
	errLeadRoleInvalid  = errors.New("role name must be 1–32 characters on one line")
	errWorkerNeedsLead  = errors.New("only a pane another pane started can be a worker: a worker is a task for its lead")
)

// paneTeamKind is the pane's effective kind. #850 auto workers predate kinds
// and are workers by construction. An agent-started pane with no kind
// predates this change: the app still nests it, but it is never archived
// until someone marks it a worker.
func paneTeamKind(o *PaneOrigin) string {
	if o == nil {
		return ""
	}
	switch o.TeamKind {
	case paneTeamLead, paneTeamWorker:
		return o.TeamKind
	}
	// Any started pane with a worker record (an auto worker whose caller
	// had closed is an "api" pane), never the user's own.
	if o.Worker != nil && o.Source != paneOriginUser && o.Source != "" {
		return paneTeamWorker
	}
	return ""
}

// normalizeLeadRole cleans a role name: one line, no leading "@", 1–32 runes.
func normalizeLeadRole(raw string) (string, error) {
	r := strings.TrimSpace(strings.TrimPrefix(oneLine(raw), "@"))
	if r == "" {
		return "", errLeadRoleRequired
	}
	// Control and invisible format characters (bidi overrides, zero-width)
	// would disguise the role in the confirmation a person approves.
	if len([]rune(r)) > leadRoleMaxRunes || strings.IndexFunc(r, hiddenOrControlRune) >= 0 {
		return "", errLeadRoleInvalid
	}
	return r, nil
}

// plainWorker is the lifecycle record of a worker started with cmd claude or
// codex: it lets #850's finish, read and archive logic follow it, and holds no
// account credentials (workerHoldsCredentials).
func plainWorker(provider string) (*workerInfo, error) {
	if provider != agentClaude && provider != agentCodex {
		return nil, nil
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &workerInfo{ID: "worker-" + id, State: "running", Provider: provider, Plain: true}, nil
}

// workerHoldsCredentials reports whether a worker record carries an account
// lane, lease or team capability. Only a record that claims none of them is a
// plain worker: everything else takes the credential paths and their gates.
func workerHoldsCredentials(w *workerInfo) bool {
	if w == nil {
		return false
	}
	return !w.Plain || w.Account != "" || w.TeamID != "" || w.ConfigDir != "" || w.LeaseID != "" || w.CredentialRevision != ""
}

// leadsWorkers reports whether a pane with this origin leads the workers it
// starts: a lead, or the user's own pane. A worker passes its workers to its
// own lead, and so does an agent-started pane with no kind (as before kinds).
func leadsWorkers(o *PaneOrigin) bool {
	switch paneTeamKind(o) {
	case paneTeamLead:
		return true
	case paneTeamWorker:
		return false
	}
	return o == nil || o.Source != paneOriginAgent
}

// workerLeadFor is the nearest lead: the pane that reads a worker's result
// and is flagged when it is kept. It is the first running pane up the chain
// of starters that leads workers (a sub-lead, a lead or the user's pane),
// else the top of the chain recorded at spawn (LeadPaneID). It only decides
// who reads and is told; spend caps and close authority stay on LeadPaneID.
func (a *apiServer) workerLeadFor(o *PaneOrigin) string {
	if o == nil {
		return ""
	}
	id := o.ByPaneID
	for hops := 0; id != "" && hops < 32; hops++ {
		p, ok := a.mgr.Get(id)
		if !ok {
			break
		}
		snap := p.Snapshot()
		if snap.Status != PaneRunning {
			break
		}
		if leadsWorkers(snap.Origin) {
			return id
		}
		if snap.Origin == nil {
			break
		}
		id = snap.Origin.ByPaneID
	}
	return o.LeadPaneID
}

// applySpawnTeam sets the kind of a pane an agent starts: a lead when the
// spawn names a role, otherwise a worker for its starter. Unattributed
// callers (api) start plain panes: a worker needs a lead.
func applySpawnTeam(o *PaneOrigin, leadRole, provider string) error {
	if o == nil {
		return nil
	}
	if leadRole != "" {
		o.TeamKind, o.LeadRole = paneTeamLead, leadRole
		return nil
	}
	if o.Source != paneOriginAgent {
		return nil
	}
	o.TeamKind = paneTeamWorker
	if o.Worker == nil {
		w, err := plainWorker(provider)
		if err != nil {
			return err
		}
		o.Worker = w
	}
	return nil
}

// nextTeamOrigin is origin after a kind change, or an error the caller
// reports. kind is "lead", "worker" or "" (back to the user's own pane).
func nextTeamOrigin(prev *PaneOrigin, kind, role, provider string, now time.Time) (*PaneOrigin, error) {
	next := prev.clone()
	if next == nil {
		next = &PaneOrigin{At: now.UnixMilli()}
	}
	switch kind {
	case paneTeamLead:
		r, err := normalizeLeadRole(role)
		if err != nil {
			return nil, err
		}
		next.TeamKind, next.LeadRole = paneTeamLead, r
		// A plain worker record only drives automatic archiving; a lead has
		// none. An account worker keeps its lease record: the kind alone stops
		// archiving.
		if next.Worker != nil && !workerHoldsCredentials(next.Worker) {
			next.Worker = nil
		}
	case paneTeamWorker:
		if next.Source != paneOriginAgent || (next.ByPaneID == "" && next.LeadPaneID == "") {
			return nil, errWorkerNeedsLead
		}
		next.TeamKind, next.LeadRole = paneTeamWorker, ""
		if next.Worker == nil {
			w, err := plainWorker(provider)
			if err != nil {
				return nil, err
			}
			next.Worker = w
		}
	case "":
		if next.Source == paneOriginAgent {
			return nil, errors.New("an agent-started pane is a lead or a worker")
		}
		next.TeamKind, next.LeadRole = "", ""
		if next.Worker != nil && !workerHoldsCredentials(next.Worker) {
			next.Worker = nil
		}
	default:
		return nil, fmt.Errorf("kind must be %q or %q", paneTeamLead, paneTeamWorker)
	}
	return next, nil
}

// setPaneTeam applies a kind change once it is allowed, persisting it and
// reverting on a failed write. A plain worker record dropped by the change is
// also dropped from the worker queue, so the tick can't restore it.
func (a *apiServer) setPaneTeam(p *Pane, kind, role string, now time.Time) (*PaneOrigin, error) {
	var (
		next, prev *PaneOrigin
		err        error
	)
	current := a.mgr.withCurrentPane(p, func() {
		// The archive commit holds this lock while it re-checks the kind and
		// marks the pane archiving: a change either lands before it (and the
		// commit sees the new kind) or is refused after it.
		p.agentWriteMu.Lock()
		defer p.agentWriteMu.Unlock()
		snap := p.Snapshot()
		if snap.Status != PaneRunning {
			err = errors.New("the pane is no longer running")
			return
		}
		if p.workerArchiving.Load() {
			err = errors.New("the worker is being archived")
			return
		}
		provider := paneSummaryAgent(p, snap)
		// Read and replace the origin in one step: the worker tick updates
		// Worker.State under the same lock, and must not be overwritten.
		p.mu.Lock()
		prev = p.Origin
		next, err = nextTeamOrigin(prev, kind, role, provider, now)
		if err == nil {
			p.Origin = next
		}
		p.mu.Unlock()
		if err != nil {
			return
		}
		if a.panes != nil {
			if perr := a.panes.Put(p); perr != nil {
				p.mu.Lock()
				if p.Origin == next {
					p.Origin = prev
				}
				p.mu.Unlock()
				err = fmt.Errorf("persist pane: %w", perr)
			}
		}
	})
	if !current {
		return nil, errors.New("the pane has closed or restarted; nothing was changed")
	}
	if err != nil {
		return nil, err
	}
	if prev != nil && prev.Worker != nil && (next.Worker == nil || next.Worker.ID != prev.Worker.ID) {
		a.forgetPlainWorker(prev.Worker.ID)
	}
	return next.clone(), nil
}

// forgetPlainWorker removes the queue entry of a plain worker record that a
// kind change replaced, so the pane is tracked afresh if it becomes a worker
// again. Account workers are never dropped this way.
func (a *apiServer) forgetPlainWorker(id string) {
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.requests[:0]
	for _, r := range q.requests {
		if r.Info.ID != id || workerHoldsCredentials(&r.Info) {
			kept = append(kept, r)
		}
	}
	for i := len(kept); i < len(q.requests); i++ {
		q.requests[i] = nil
	}
	q.requests = kept
}

// paneTeamChangeErrStatus maps setPaneTeam errors onto HTTP codes.
func paneTeamChangeErrStatus(err error) int {
	switch {
	case errors.Is(err, errWorkerNeedsLead):
		return http.StatusConflict
	case errors.Is(err, errLeadRoleRequired), errors.Is(err, errLeadRoleInvalid):
		return http.StatusBadRequest
	case strings.HasPrefix(err.Error(), "persist pane"):
		return http.StatusInternalServerError
	default:
		return http.StatusConflict
	}
}

type paneTeamReq struct {
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// setPaneTeamHTTP handles POST /api/panes/{id}/team {kind, role} from the
// pane menu ("Make lead" / "Make worker"). Only a person may call it: it is
// how existing role panes, including ones the user opened, become leads.
func (a *apiServer) setPaneTeamHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "change whether a pane is a lead or a worker") {
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req paneTeamReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "none" {
		kind = ""
	}
	origin, err := a.setPaneTeam(p, kind, req.Role, time.Now())
	if err != nil {
		http.Error(w, err.Error(), paneTeamChangeErrStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"origin": visibleWorkerOrigin(origin)})
}

// paneTeamAgentAuthority decides an agent's set_pane_info kind change. The
// pane's starter or lead may change it; anyone else, the pane itself
// included, needs the person's OK. A pane the user opened may only be
// promoted to a lead, at the user's request and with their OK in the app.
func (a *apiServer) paneTeamAgentAuthority(target *PaneOrigin, paneID, callerPane, kind string, userRequested bool) (paneAgentDecision, string) {
	if target == nil || target.Source != paneOriginAgent {
		if kind != paneTeamLead {
			return paneAgentRefused, "it is a user-opened pane; agents can only ask to make it a lead, never make it a worker or clear its kind"
		}
		if !userRequested {
			return paneAgentRefused, "it is a user-opened pane. If the user asked to make it a lead, call set_pane_info again with kind: \"lead\", a valid lead_role and user_requested: true; they will be asked to allow it in the SoluTerminal app"
		}
		// Standing agent authority never covers this promotion either.
		return paneAgentNeedsOK, ""
	}
	caller := strings.TrimSpace(callerPane)
	if caller != "" && caller != paneID && paneStartedByCaller(target, caller) {
		return paneAgentAllowed, ""
	}
	if !userRequested {
		if caller == paneID {
			return paneAgentRefused, "a pane can't change its own kind. If the user asked for it, call set_pane_info again with user_requested: true and they will be asked to allow it in the SoluTerminal app"
		}
		return paneAgentRefused, "it was not started by you. If the user asked for it, call set_pane_info again with user_requested: true and they will be asked to allow it in the SoluTerminal app"
	}
	// The person's standing agent authority does not cover kind changes:
	// promotion opts a worker out of automatic archiving.
	return paneAgentNeedsOK, ""
}

func paneTeamChangeText(kind, role string) string {
	switch kind {
	case paneTeamLead:
		if r, err := normalizeLeadRole(role); err == nil {
			return "make it a lead (role “" + r + "”)"
		}
		return "make it a lead"
	case paneTeamWorker:
		return "make it a worker (closed automatically once finished, read and pushed)"
	}
	return "change its kind"
}

func init() {
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/panes/{id}/team", a.setPaneTeamHTTP)
	})
}

// mcpSetPaneTeam is set_pane_info's kind change (paneTeamAgentAuthority).
func (a *apiServer) mcpSetPaneTeam(args map[string]any, c paneCaller, p *Pane) any {
	for _, other := range []string{"handle", "working_on"} {
		if _, ok := args[other]; ok {
			return toolErr("set kind in its own set_pane_info call, without handle or working_on")
		}
	}
	kind := strings.TrimSpace(mcpArgString(args, "kind"))
	if kind != paneTeamLead && kind != paneTeamWorker {
		return toolErr("kind must be \"lead\" or \"worker\"")
	}
	role := mcpArgString(args, "lead_role")
	if kind == paneTeamLead {
		r, err := normalizeLeadRole(role)
		if err != nil {
			return toolErr("lead_role: " + err.Error())
		}
		role = r
	} else if strings.TrimSpace(role) != "" {
		return toolErr("lead_role goes with kind: \"lead\"")
	}
	name := a.paneConfirmTarget(p)
	asked, _ := args["user_requested"].(bool)
	decision, why := a.paneTeamAgentAuthority(p.Snapshot().Origin, p.ID, c.ID, kind, asked)
	apply := func(target *Pane) map[string]any {
		if _, err := a.setPaneTeam(target, kind, role, time.Now()); err != nil {
			return toolErr("couldn't change " + name + ": " + err.Error())
		}
		if kind == paneTeamLead {
			return toolOK(name + " is now a lead (role “" + role + "”). It is never closed automatically.")
		}
		return toolOK(name + " is now a worker. It is archived automatically once it has finished, its lead has read its result, and its work is committed and pushed.")
	}
	switch decision {
	case paneAgentRefused:
		return toolErr("You can't change whether " + name + " is a lead or a worker: " + why + ".")
	case paneAgentNeedsOK:
		caller := a.paneConfirmCaller(c)
		key := paneConfirmChange + "\x00" + c.bucket() + "\x00" + p.ID + "\x00team\x00" + kind + "\x00" + role
		req, err := a.requestPaneConfirmFor(key, c.bucket(), paneConfirmView{
			Action:       paneConfirmChange,
			CallerPaneID: strings.TrimSpace(c.ID),
			Caller:       caller,
			PaneID:       p.ID,
			Pane:         name,
			Title:        "Change " + name + "?",
			Message:      caller + " wants to " + paneTeamChangeText(kind, role) + ": " + name + ".",
		}, func() map[string]any {
			target, ok := a.mgr.Get(p.ID)
			if !ok {
				return toolErr(name + " has closed; nothing was changed.")
			}
			return apply(target)
		})
		if err != nil {
			return toolErr(err.Error())
		}
		return awaitPaneConfirm(req, paneConfirmWaitingText(name+" is not changed yet"))
	}
	return apply(p)
}

func paneLeadRole(o *PaneOrigin) string {
	if paneTeamKind(o) != paneTeamLead {
		return ""
	}
	return o.LeadRole
}
