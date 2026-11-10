package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// workerAccount contains labels and conservative remaining quota, never tokens.
// Unknown/stale usage is excluded rather than interpreted as unused quota.
type workerAccount struct {
	Provider     string              `json:"provider"`
	Account      string              `json:"account"`
	TeamID       string              `json:"teamId,omitempty"`
	Headroom     int                 `json:"headroom"`
	Windows      []workerQuotaWindow `json:"-"`
	Capabilities map[string]bool     `json:"-"`
}

type workerInfo struct {
	ID                 string `json:"id,omitempty"`
	State              string `json:"state"`
	Priority           string `json:"priority,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Account            string `json:"account,omitempty"`
	TeamID             string `json:"teamId,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Reviewer           string `json:"reviewer,omitempty"`
	ReviewerAccount    string `json:"reviewerAccount,omitempty"`
	ConfigDir          string `json:"configDir,omitempty"`
	CredentialRevision string `json:"credentialRevision,omitempty"`
	LeaseID            string `json:"leaseId,omitempty"`
	StartStatus        string `json:"startStatus,omitempty"`
	StartHint          string `json:"startHint,omitempty"`
	ArchiveID          string `json:"archiveId,omitempty"`
	ArchivedAt         int64  `json:"archivedAt,omitempty"`
	Pinned             bool   `json:"pinned,omitempty"`
	// Plain marks a worker started with cmd claude or codex: it follows the
	// same finish/read/archive lifecycle and holds no account credentials
	// (pane_team.go workerHoldsCredentials).
	Plain bool `json:"plain,omitempty"`
}

// prefer breaks equal-headroom ties; it never overrides a healthier account.
func pickWorker(accounts []workerAccount, prefer string, guarded bool, requirements ...[]string) (workerInfo, error) {
	needs := requiredWorkerCapabilities(guarded, requirements...)
	candidates, err := filterWorkerCapabilities(accounts, needs)
	if err != nil {
		return workerInfo{}, err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		x, y := candidates[i], candidates[j]
		if x.Headroom != y.Headroom {
			return x.Headroom > y.Headroom
		}
		if (x.Provider == prefer) != (y.Provider == prefer) {
			return x.Provider == prefer
		}
		if x.Provider != y.Provider {
			return x.Provider < y.Provider
		}
		if x.Account != y.Account {
			return x.Account < y.Account
		}
		return x.TeamID < y.TeamID
	})
	var selected *workerAccount
	for i := range candidates {
		c := &candidates[i]
		if c.Headroom > 0 {
			selected = c
			break
		}
	}
	if selected == nil {
		return workerInfo{}, errWorkerQuota
	}
	w := workerInfo{State: "running", Provider: selected.Provider, Account: selected.Account, TeamID: selected.TeamID,
		Reason: fmt.Sprintf("most quota headroom: %d%% remaining in the tightest measured window; prefer breaks ties", selected.Headroom)}
	if guarded {
		w.Reason = "guarded: role-guard capability required; " + w.Reason
	}
	for _, c := range candidates {
		if c.Headroom > 0 && c.Provider != selected.Provider {
			w.Reviewer, w.ReviewerAccount = c.Provider, c.Account
			break
		}
	}
	if w.Reviewer == "" {
		for _, c := range candidates {
			if c.Headroom > 0 && c.Provider == selected.Provider && (c.Account != selected.Account || c.TeamID != selected.TeamID) {
				w.Reviewer, w.ReviewerAccount = c.Provider, c.Account
				break
			}
		}
	}
	return w, nil
}

func guardedWorkerFolder(cwd string) bool {
	cwd = expandHome(strings.TrimSpace(cwd))
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	// Role guards belong to this cwd; the user's global ~/.claude hooks must
	// not turn every repository into a Claude-only role folder.
	if _, err := os.Lstat(filepath.Join(cwd, ".solu-no-autoanswer")); err == nil {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err == nil {
		var settings struct {
			Hooks map[string]json.RawMessage `json:"hooks"`
		}
		if json.Unmarshal(raw, &settings) != nil || len(settings.Hooks) > 0 {
			return true
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return true
	}

	return codexRoleGuard(cwd)
}

type workerSettings struct {
	MaxRunning               int    `json:"maxRunning"`
	FinishAfterMinutes       int    `json:"finishAfterMinutes"`
	WorkerLeadReservePct     int    `json:"workerLeadReservePct"`
	WorkerExpectedRunMinutes int    `json:"workerExpectedRunMinutes"`
	WorkerAutoArchive        bool   `json:"workerAutoArchive"`
	WorkerAccounts           string `json:"workerAccounts"`
	// WorkerMergedGraceMinutes: a finished worker whose work has landed on
	// the remote default branch closes this long after the merge is seen,
	// without its lead's read receipt (worker_archives.go).
	WorkerMergedGraceMinutes int `json:"workerMergedGraceMinutes"`
}

func defaultWorkerSettings() workerSettings {
	return workerSettings{MaxRunning: 4, FinishAfterMinutes: 5, WorkerLeadReservePct: 15, WorkerExpectedRunMinutes: 45, WorkerAutoArchive: true, WorkerAccounts: "active", WorkerMergedGraceMinutes: 10}
}

type workerRequest struct {
	Info                            workerInfo
	Args                            map[string]any
	Caller, Lead, PaneID, Workspace string
	Scope                           *mcpScope
	Created                         time.Time
	IdleSince                       time.Time
	LastInput                       int64
	Error                           string
	release                         func()
	QuotaRetryAt                    time.Time
}
type workerQueue struct {
	mu              sync.Mutex
	settings        workerSettings
	loaded          bool
	startedAt       time.Time
	archiveEnabled  atomic.Bool
	archivePolicyMu sync.RWMutex
	next            uint64
	requests        []*workerRequest
}

var workerQueueSlot extSlot[workerQueue]

func (a *apiServer) workers() *workerQueue { return workerQueueSlot.ensure(&a.ext) }
func (a *apiServer) loadWorkerSettingsLocked(q *workerQueue) {
	if q.loaded {
		return
	}
	q.loaded = true
	q.startedAt = time.Now()
	q.settings = defaultWorkerSettings()
	defer func() { q.archiveEnabled.Store(q.settings.WorkerAutoArchive) }()
	raw, err := os.ReadFile(filepath.Join(a.dataDir, "worker-settings.json"))
	if err != nil {
		return
	}
	s := defaultWorkerSettings()
	if json.Unmarshal(raw, &s) == nil && s.valid() {
		q.settings = s
	}
}
func workerPriority(p string) int {
	switch p {
	case "urgent":
		return 0
	case "later":
		return 2
	}
	return 1
}
func (q *workerQueue) sortLocked() {
	sort.SliceStable(q.requests, func(i, j int) bool {
		return workerPriority(q.requests[i].Info.Priority) < workerPriority(q.requests[j].Info.Priority)
	})
}
func (q *workerQueue) positionLocked(r *workerRequest) int {
	n := 0
	for _, v := range q.requests {
		if v.Info.State == "queued" && v.Lead == r.Lead {
			n++
			if v == r {
				return n
			}
		}
	}
	return 0
}
func workerResult(r *workerRequest, position int) any {
	out := map[string]any{"queue_id": r.Info.ID, "status": r.Info.State, "workspace": r.Workspace}
	if r.Error != "" {
		out["reason"] = r.Error
	}
	if position > 0 {
		out["queued"], out["position"] = true, position
	}
	if r.PaneID != "" {
		out["id"] = r.PaneID
	}
	if r.Info.Provider != "" {
		out["provider"], out["account"], out["reason"] = r.Info.Provider, r.Info.Account, r.Info.Reason
		out["reviewer"], out["reviewer_account"] = r.Info.Reviewer, r.Info.ReviewerAccount
	}
	b, _ := json.Marshal(out)
	return toolOK(string(b))
}

// Only cmd:auto enters this opt-in queue. Explicit commands retain their path.
func (a *apiServer) mcpToolAutoWorker(args map[string]any, caller string, scope *mcpScope) any {
	if caller == "" {
		return toolErr("cmd auto requires a live lead pane")
	}
	creator, ok := a.mgr.Get(caller)
	if !ok || creator.Snapshot().Status != PaneRunning {
		return toolErr("lead pane is no longer running")
	}
	prefer := mcpArgString(args, "prefer")
	if prefer != "" && prefer != "claude" && prefer != "codex" {
		return toolErr("prefer must be claude or codex")
	}
	priority := mcpArgString(args, "priority")
	if priority == "" {
		priority = "normal"
	}
	if priority != "urgent" && priority != "normal" && priority != "later" {
		return toolErr("priority must be urgent, normal or later")
	}
	workspace, err := a.resolveMCPSpawnWorkspace(mcpArgString(args, "workspace"), caller)
	if err != nil {
		return toolErr(err.Error())
	}
	if model := mcpArgString(args, "model"); model != "" && !spawnModelPattern.MatchString(model) {
		return toolErr("invalid model id")
	}
	if role := mcpArgString(args, "role"); !validPaneRole(role) {
		return toolErr("unknown role: " + role)
	}
	if strings.TrimSpace(mcpArgString(args, "lead")) != "" {
		return toolErr("lead can't be combined with cmd \"auto\": an auto worker is a worker")
	}
	if h := mcpArgString(args, "handle"); h != "" {
		if _, err := normalizePaneHandle(h); err != nil {
			return toolErr(err.Error())
		}
	}
	needs, _, err := workerCapabilityRequirements(args)
	if err != nil {
		return toolErr(err.Error())
	}
	origin := agentSpawnOrigin(creator, "", workspace, time.Now())
	copied := map[string]any{}
	for k, v := range args {
		copied[k] = v
	}
	copied["workspace"] = workspace
	copied["needs"] = needs
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	a.loadWorkerSettingsLocked(q)
	// Bound pending and historical entries. Pending requests never expire into a spawn.
	pending := 0
	kept := q.requests[:0]
	for _, r := range q.requests {
		if (r.Info.State != "closed" && r.Info.State != "archived") || time.Since(r.Created) < 24*time.Hour {
			kept = append(kept, r)
		}
		if r.Info.State == "queued" && r.Lead == origin.LeadPaneID {
			pending++
		}
	}
	q.requests = kept
	if pending >= 128 {
		return toolErr("lead queue is full (128 pending requests)")
	}
	q.next++
	r := &workerRequest{Info: workerInfo{ID: fmt.Sprintf("worker-%d-%d", time.Now().UnixNano(), q.next), State: "queued", Priority: priority}, Args: copied, Caller: caller, Lead: origin.LeadPaneID, Workspace: workspace, Scope: scope, Created: time.Now()}
	q.requests = append(q.requests, r)
	q.sortLocked()
	// Reserve a slot before releasing the lock, including concurrent ready waits.
	if q.positionLocked(r) == 1 && a.workerRunningLocked(q, r.Lead) < q.settings.MaxRunning+boolInt(priority == "urgent") {
		r.Info.State = "running"
		q.mu.Unlock()
		result := a.startAutoWorker(r)
		q.mu.Lock()
		return result
	}
	return workerResult(r, q.positionLocked(r))
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func (a *apiServer) workerRunningLocked(q *workerQueue, lead string) int {
	n := 0
	tracked := map[string]bool{}
	trackedWorkers := map[string]bool{}
	for _, r := range q.requests {
		if r.Info.State == "running" && r.Lead == lead {
			n++
			tracked[r.PaneID] = true
			trackedWorkers[r.Info.ID] = true
		}
	}
	for _, p := range a.mgr.List() {
		s := p.Snapshot()
		if tracked[s.ID] || (s.Origin != nil && s.Origin.Worker != nil && trackedWorkers[s.Origin.Worker.ID]) || s.Status != PaneRunning || s.Origin == nil || s.Origin.LeadPaneID != lead {
			continue
		}
		if s.Origin.Worker != nil && s.Origin.Worker.State == "finished" {
			continue
		}
		if agentFromCmd(s.Cmd) != "" || s.Agent != "" {
			n++
		}
	}
	return n
}
func (a *apiServer) workerAuthority(r *workerRequest) error {
	if p, ok := a.mgr.Get(r.Caller); !ok || p.Snapshot().Status != PaneRunning {
		return errors.New("requesting pane is no longer running")
	}
	if p, ok := a.mgr.Get(r.Lead); !ok || p.Snapshot().Status != PaneRunning {
		return errors.New("lead pane is no longer running")
	}
	if r.Scope != nil && !a.mcpScopes.contains(r.Scope) {
		return errors.New("spawn capability expired or was revoked")
	}
	if r.Scope != nil && !r.Scope.allows("spawn_pane") {
		return errors.New("spawn_pane is no longer in scope")
	}
	if _, denied := a.runMCPCallGuards("spawn_pane", r.Args, r.Scope); denied != nil {
		return errors.New("spawn authority denied by current policy")
	}
	return mcpCheckMutatorAllowed("spawn_pane")
}
func (a *apiServer) startAutoWorker(r *workerRequest) any {
	q := a.workers()
	fail := func(err error) any {
		q.mu.Lock()
		r.Info.State = "closed"
		r.Error = err.Error()
		r.release = nil
		q.mu.Unlock()
		return toolErr(err.Error())
	}
	if err := a.workerAuthority(r); err != nil {
		return fail(err)
	}
	if quotaWorkerAccounts == nil {
		return fail(errors.New("account usage data unavailable in this build; use an explicit cmd"))
	}
	q.mu.Lock()
	a.loadWorkerSettingsLocked(q)
	settings := q.settings
	q.mu.Unlock()
	now := time.Now()
	accounts, needs, err := a.workerCandidates(r.Args, now)
	if err != nil {
		return fail(err)
	}
	selection, err := pickWorkerWithPolicy(accounts, mcpArgString(r.Args, "prefer"), false, a.workerLeadCounts(), settings, now, needs)
	if errors.Is(err, errWorkerQuota) {
		q.mu.Lock()
		r.Info.State = "queued"
		r.Error = "waiting for qualifying quota headroom"
		r.QuotaRetryAt = now.Add(30 * time.Second)
		out := workerResult(r, q.positionLocked(r))
		q.mu.Unlock()
		return out
	}
	if err != nil {
		return fail(err)
	}
	if len(needs) > 0 {
		selection.Reason = "capabilities: " + strings.Join(needs, ", ") + "; " + selection.Reason
	}
	selection.ID, selection.Priority = r.Info.ID, r.Info.Priority
	opts := SpawnOpts{}
	if prepareQuotaWorker == nil {
		return fail(errors.New("account routing unavailable"))
	}
	release, err := prepareQuotaWorker(a, selection, mcpArgString(r.Args, "cwd"), &opts)
	if err != nil {
		return fail(err)
	}
	if selection.TeamID == "" {
		if selection.Provider == "claude" {
			selection.ConfigDir = envValueFromList(opts.Env, "CLAUDE_CONFIG_DIR")
		} else {
			selection.ConfigDir = envValueFromList(opts.Env, "CODEX_HOME")
		}
	}
	if opts.Origin != nil && opts.Origin.Worker != nil {
		selection.CredentialRevision = opts.Origin.Worker.CredentialRevision
		selection.LeaseID = opts.Origin.Worker.LeaseID
	}
	q.mu.Lock()
	r.Info = selection
	r.release = release
	q.mu.Unlock()
	args := map[string]any{}
	for k, v := range r.Args {
		args[k] = v
	}
	args["cmd"] = selection.Provider
	guarded, denied := a.runMCPCallGuards("spawn_pane", args, r.Scope)
	if denied != nil {
		if release != nil {
			release()
		}
		return fail(errors.New("spawn authority changed before launch"))
	}
	args = guarded
	if err := a.workerAuthority(r); err != nil {
		if release != nil {
			release()
		}
		return fail(err)
	}
	// The ordinary spawn validates model, role, workspace, handle and trust gates.
	result := a.mcpToolSpawnWorker(args, r.Caller, &selection, opts)
	raw, _ := json.Marshal(result)
	var env struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.IsError || len(env.Content) == 0 {
		if release != nil {
			release()
		}
		return fail(errors.New("worker spawn failed: " + string(raw)))
	}
	var out map[string]any
	if json.Unmarshal([]byte(env.Content[0].Text), &out) != nil {
		if release != nil {
			release()
		}
		return fail(errors.New("invalid spawn result"))
	}
	q.mu.Lock()
	r.PaneID, _ = out["id"].(string)
	r.Info.StartStatus, _ = out["status"].(string)
	r.Info.StartHint, _ = out["hint"].(string)
	info := r.Info
	q.mu.Unlock()
	if p, ok := a.mgr.Get(r.PaneID); ok {
		a.mgr.withCurrentPane(p, func() {
			p.mu.Lock()
			o := p.Origin.clone()
			if o != nil {
				o.Worker = &info
				p.Origin = o
			}
			p.mu.Unlock()
			if a.panes != nil {
				_ = a.panes.Put(p)
			}
		})
	}
	out["queue_id"], out["provider"], out["account"], out["reason"] = selection.ID, selection.Provider, selection.Account, selection.Reason
	out["reviewer"], out["reviewer_account"] = selection.Reviewer, selection.ReviewerAccount
	b, _ := json.Marshal(out)
	return toolOK(string(b))
}

func (a *apiServer) cancelQueuedWorker(args map[string]any, caller string) any {
	id := mcpArgString(args, "queue_id")
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.requests {
		if r.Info.ID != id {
			continue
		}
		if caller == "" || (r.Caller != caller && r.Lead != caller) {
			return toolErr("only the requesting pane or its lead can cancel this worker")
		}
		if r.Info.State != "queued" {
			return toolErr("worker is no longer queued")
		}
		r.Info.State = "closed"
		r.Error = "cancelled"
		return workerResult(r, 0)
	}
	return toolErr("queued worker not found")
}

// A final reply plus continuous idle is required. A trust/permission prompt or
// a never-started agent is not a completed worker. Any follow-up reopens it.
func nextWorkerState(current string, alive, idle, complete bool, input int64, now time.Time, since *time.Time, lastInput *int64, delay time.Duration) string {
	if !alive {
		return "closed"
	}
	if input != *lastInput || !idle || !complete {
		*since = time.Time{}
		*lastInput = input
		return "running"
	}
	if since.IsZero() {
		*since = now
	}
	if now.Sub(*since) >= delay {
		return "finished"
	}
	return "running"
}
func (a *apiServer) tickWorkers(now time.Time) {
	q := a.workers()
	q.mu.Lock()
	a.loadWorkerSettingsLocked(q)
	tracked := map[string]bool{}
	trackedWorkers := map[string]bool{}
	for _, r := range q.requests {
		tracked[r.PaneID] = true
		trackedWorkers[r.Info.ID] = true
	}
	var releases []func()
	// Plain workers join the queue without cmd auto, so bound history here
	// too: finished entries leave a day after they started.
	kept := q.requests[:0]
	for _, r := range q.requests {
		if (r.Info.State != "closed" && r.Info.State != "archived") || r.release != nil || now.Sub(r.Created) < 24*time.Hour {
			kept = append(kept, r)
		}
	}
	for i := len(kept); i < len(q.requests); i++ {
		q.requests[i] = nil
	}
	q.requests = kept
	for _, p := range a.mgr.List() {
		snap := p.Snapshot()
		if snap.Origin == nil || snap.Origin.Worker == nil {
			continue
		}
		if trackedWorkers[snap.Origin.Worker.ID] {
			for _, r := range q.requests {
				w := snap.Origin.Worker
				if r.Info.ID == w.ID && r.PaneID == "" && r.Info.State == "running" && r.Info.ConfigDir == w.ConfigDir && r.Info.LeaseID == w.LeaseID && r.Info.CredentialRevision == w.CredentialRevision {
					// Spawn publishes membership before its caller binds the id.
					// This is the initial process, still using r.release's lease.
					r.PaneID = snap.ID
					continue
				}
				if r.Info.ID == w.ID && snap.Status == PaneRunning && (r.Info.State == "closed" || r.PaneID != snap.ID || r.Info.ConfigDir != w.ConfigDir || r.Info.LeaseID != w.LeaseID || r.Info.CredentialRevision != w.CredentialRevision) {
					// A new process can reuse the pane id between ticks. Its credential
					// incarnation is authoritative even when no closed state was observed.
					if r.release != nil {
						releases = append(releases, r.release)
						r.release = nil
					}
					r.PaneID = snap.ID
					r.Info = *w
					r.Info.State = "running"
					r.Error = ""
					r.IdleSince = time.Time{}
				}
			}
			continue
		}
		if tracked[snap.ID] {
			continue
		}
		w := *snap.Origin.Worker
		if w.State == "closed" {
			continue
		}
		r := &workerRequest{Info: w, Caller: snap.Origin.ByPaneID, Lead: snap.Origin.LeadPaneID, PaneID: snap.ID, Workspace: snap.Origin.Workspace, Created: snap.CreatedAt}
		if snap.Status == PaneRunning && recoverQuotaWorker != nil && workerHoldsCredentials(&w) {
			release, err := recoverQuotaWorker(a, p, w)
			if err != nil {
				r.Error = "credential recovery: " + err.Error()
			} else {
				r.release = release
			}
		}
		q.requests = append(q.requests, r)
		trackedWorkers[w.ID] = true
	}
	for _, r := range q.requests {
		if r.Info.State == "queued" || r.Info.State == "closed" || r.Info.State == "archived" || r.PaneID == "" {
			continue
		}
		p, ok := a.mgr.Get(r.PaneID)
		alive := ok && p.Snapshot().Status == PaneRunning
		idle, complete, input := false, false, int64(0)
		if alive {
			state := p.computeStateCached()
			idle = state == StateIdle
			if idle {
				reply := a.paneLatestReply(p)
				complete = reply.Complete && strings.TrimSpace(reply.Reply) != ""
			}
			input = atomic.LoadInt64(&p.LastInputMs)
		}
		state := nextWorkerState(r.Info.State, alive, idle, complete, input, now, &r.IdleSince, &r.LastInput, time.Duration(q.settings.FinishAfterMinutes)*time.Minute)
		if state != r.Info.State {
			r.Info.State = state
			if ok {
				// Keep removal ordered after persistence: a tick holding an old
				// pointer must never recreate an archived pane's saved record.
				a.mgr.withCurrentPane(p, func() {
					p.mu.Lock()
					// A kind change may have dropped this record (pane_team.go).
					if p.Origin != nil && p.Origin.Worker != nil && p.Origin.Worker.ID == r.Info.ID {
						o := p.Origin.clone()
						w := r.Info
						o.Worker = &w
						p.Origin = o
					}
					p.mu.Unlock()
					if a.panes != nil {
						_ = a.panes.Put(p)
					}
				})
			}
		}
		if state == "closed" && r.release != nil {
			release := r.release
			r.release = nil
			releases = append(releases, release)
		}
	}
	var starts []*workerRequest
	for _, r := range q.requests {
		if r.Info.State == "queued" && !now.Before(r.QuotaRetryAt) && a.workerRunningLocked(q, r.Lead) < q.settings.MaxRunning+boolInt(r.Info.Priority == "urgent") {
			r.Info.State = "running"
			starts = append(starts, r)
		}
	}
	q.mu.Unlock()
	for _, release := range releases {
		release()
	}
	for _, r := range starts {
		go a.startAutoWorker(r)
	}
	a.archiveFinishedWorkers(now)
}
func (a *apiServer) runWorkerQueue(ctx context.Context) {
	archives := a.workerArchives()
	archives.mu.Lock()
	archives.context = ctx
	archives.mu.Unlock()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.tickWorkers(now)
		}
	}
}

func init() {
	registerBootStep(bootOwned, func(b *bootEnv) { go b.api.runWorkerQueue(b.ctx) })
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/worker-queue", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.workerQueueSnapshot()) })
		mux.HandleFunc("GET /api/worker-settings", func(w http.ResponseWriter, r *http.Request) {
			q := a.workers()
			q.mu.Lock()
			defer q.mu.Unlock()
			a.loadWorkerSettingsLocked(q)
			writeJSON(w, 200, q.settings)
		})
		mux.HandleFunc("PUT /api/worker-settings", func(w http.ResponseWriter, r *http.Request) {
			var patch workerSettingsPatch
			if !decodeJSON(w, r, &patch, 1024) {
				return
			}
			q := a.workers()
			q.mu.Lock()
			defer q.mu.Unlock()
			a.loadWorkerSettingsLocked(q)
			s, changed := patch.apply(q.settings)
			if !changed || !s.valid() {
				http.Error(w, "set maxRunning (1..32), finishAfterMinutes (1..1440), workerLeadReservePct (0..100), workerExpectedRunMinutes (1..1440), workerMergedGraceMinutes (0..1440), workerAutoArchive, or workerAccounts (active, saved, team)", 400)
				return
			}
			if s.widens(q.settings) && !a.requireHuman(w, r, "widen worker spending or automatic archiving") {
				return
			}
			raw, _ := json.Marshal(s)
			if err := writeFileAtomicSynced(filepath.Join(a.dataDir, "worker-settings.json"), raw, 0600); err != nil {
				http.Error(w, "could not save worker settings", 500)
				return
			}
			q.settings = s
			q.archivePolicyMu.Lock()
			q.archiveEnabled.Store(s.WorkerAutoArchive)
			q.archivePolicyMu.Unlock()
			q.loaded = true
			writeJSON(w, 200, s)
		})
	})
}

func originWorker(o *PaneOrigin) *workerInfo {
	if o == nil || o.Worker == nil {
		return nil
	}
	w := *o.Worker
	return &w
}
func (a *apiServer) queuedWorkerSummaries() []mcpPaneSummary {
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []mcpPaneSummary
	for _, r := range q.requests {
		if r.PaneID != "" || r.Info.State == "archived" {
			continue
		}
		w := *visibleWorkerInfo(&r.Info)
		o := &PaneOrigin{Source: paneOriginAgent, ByPaneID: r.Caller, LeadPaneID: r.Lead, Workspace: r.Workspace, Purpose: spawnPurpose(mcpArgString(r.Args, "purpose"), mcpArgString(r.Args, "prompt")), At: r.Created.UnixMilli(), Worker: &w}
		out = append(out, mcpPaneSummary{ID: r.Info.ID, Title: mcpArgString(r.Args, "title"), Status: r.Info.State, Kind: "terminal", Cwd: mcpArgString(r.Args, "cwd"), Workspace: r.Workspace, Origin: o, Worker: &w, QueuePosition: q.positionLocked(r), WorkerError: r.Error})
	}
	return out
}

// Includes running/finished workers as well as queued and terminal requests,
// so a sidebar can follow queue_id to the eventual pane id.
func (a *apiServer) workerQueueSnapshot() []map[string]any {
	// Read before q.mu: the archive state's lock is never taken under it.
	decisions := a.workerArchiveDecisions()
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []map[string]any{}
	for _, r := range q.requests {
		w := r.Info
		entry := map[string]any{"queue_id": w.ID, "id": r.PaneID, "worker": visibleWorkerInfo(&w), "callerPaneId": r.Caller, "leadPaneId": r.Lead, "workspace": r.Workspace, "position": q.positionLocked(r), "error": r.Error}
		if d, ok := decisions[r.PaneID]; ok && r.PaneID != "" {
			entry["archiveDecision"] = d
		}
		out = append(out, entry)
	}
	return out
}

func (a *apiServer) bindAutoWorker(id, paneID string) {
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.requests {
		if r.Info.ID == id {
			r.PaneID = paneID
			return
		}
	}
}

// Resumes reacquire isolation from the named account, never from an arbitrary
// configDir supplied over HTTP. The hook is bound to this manager at boot.
type workerCredentialHooks struct {
	prepare func(*SpawnOpts) (func(), error)
}

var workerCredentialSlot extSlot[workerCredentialHooks]

func (a *apiServer) validatedWorkerOrigin(origin *PaneOrigin, req createPaneReq) *PaneOrigin {
	// A client-provided baseline must never exempt dirty work from archiving.
	// Restores below may recover the immutable daemon-owned record instead.
	if origin != nil {
		origin = origin.clone()
		origin.WorkerGit = nil
	}
	if !req.Restore && (origin == nil || (origin.Worker == nil && origin.TeamKind == "" && origin.LeadRole == "")) {
		return origin
	}
	var supplied *workerInfo
	if origin != nil {
		supplied = origin.Worker
		origin = origin.clone()
		origin.Worker, origin.TeamKind, origin.LeadRole = nil, "", ""
	}
	if !req.Restore {
		return origin
	}
	matches := func(o *PaneOrigin, claude, codex string) bool {
		return supplied != nil && o != nil && o.Worker != nil && o.Worker.ID == supplied.ID && ((o.Worker.Provider == "claude" && req.ClaudeSessionID != "" && req.ClaudeSessionID == claude) || (o.Worker.Provider == "codex" && req.CodexThreadID != "" && req.CodexThreadID == codex))
	}
	sameConversation := func(claude, codex string) bool {
		return (req.ClaudeSessionID != "" && req.ClaudeSessionID == claude) || (req.CodexThreadID != "" && req.CodexThreadID == codex)
	}
	var kindFrom *PersistedPane
	if a.panes != nil {
		stored := a.panes.List()
		for _, p := range stored {
			if matches(p.Origin, p.ClaudeSessionID, p.CodexThreadID) {
				restored := p.Origin.clone()
				// A plain worker gets a fresh lifecycle record: two panes must
				// never share one queue entry.
				if !workerHoldsCredentials(restored.Worker) {
					if fresh, err := plainWorker(restored.Worker.Provider); err == nil {
						restored.Worker = fresh
					} else {
						restored.Worker = nil
					}
				}
				return restored
			}
		}
		// The team kind of the same conversation, newest record first.
		for i := range stored {
			p := &stored[i]
			if p.Origin != nil && sameConversation(p.ClaudeSessionID, p.CodexThreadID) && (kindFrom == nil || p.CreatedAt.After(kindFrom.CreatedAt)) {
				kindFrom = p
			}
		}
	}
	if kindFrom != nil {
		// Saved layouts may predate the kind (or carry no origin at all): the
		// daemon's record of the conversation decides.
		if origin == nil {
			origin = kindFrom.Origin.clone()
			origin.Worker = nil
		}
		origin.TeamKind, origin.LeadRole = kindFrom.Origin.TeamKind, kindFrom.Origin.LeadRole
		origin.WorkerGit = kindFrom.Origin.WorkerGit
		// Only an agent-started pane can be a worker (nextTeamOrigin).
		if origin.TeamKind == paneTeamWorker && origin.Source != paneOriginAgent {
			origin.TeamKind = ""
			return origin
		}
		// A plain worker gets a fresh lifecycle record, so the lifecycle
		// continues without two panes sharing one queue entry. Account
		// records only ever come back whole, above.
		if w := kindFrom.Origin.Worker; w != nil && !workerHoldsCredentials(w) {
			if fresh, err := plainWorker(w.Provider); err == nil {
				origin.Worker = fresh
			}
		}
	}
	// Workspace definitions are client-writable layout data, never credential
	// or kind authority. Only the daemon's pane store may authenticate them.
	return origin
}

// Credential ownership is persisted in the daemon origin; coordination views
// need only lifecycle and selection labels.
func visibleWorkerInfo(w *workerInfo) *workerInfo {
	if w == nil {
		return nil
	}
	copy := *w
	copy.ConfigDir = ""
	copy.LeaseID = ""
	copy.CredentialRevision = ""
	return &copy
}
func visibleWorkerOrigin(o *PaneOrigin) *PaneOrigin {
	o = o.clone()
	if o != nil {
		o.Worker = visibleWorkerInfo(o.Worker)
		// The start baseline carries up to 2000 per-path digests; only the
		// daemon's archive checks read it.
		o.WorkerGit = nil
	}
	return o
}
func quotaNativeCodexArgv(opts SpawnOpts) []string {
	cmd := nativeCodexArgv()
	if opts.workerAccount != "" && opts.workerTeamID == "" {
		return routeChildCommand(cmd, []string{"-c", `model_provider="openai"`, "-c", `cli_auth_credentials_store="file"`})
	}
	return cmd
}
