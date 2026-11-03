package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuotaWorkerPicking(t *testing.T) {
	accounts := []workerAccount{{Provider: "claude", Account: "c1", Headroom: 20, Capabilities: map[string]bool{"role-guard": true}}, {Provider: "codex", Account: "x1", Headroom: 75}, {Provider: "claude", Account: "c2", Headroom: 40, Capabilities: map[string]bool{"role-guard": true}}}
	for _, tc := range []struct {
		name, prefer                string
		guarded                     bool
		provider, account, reviewer string
	}{
		{"headroom", "", false, "codex", "x1", "claude"},
		{"prefer cannot exhaust a provider", "claude", false, "codex", "x1", "claude"},
		{"guarded capability filters providers", "codex", true, "claude", "c2", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickWorker(accounts, tc.prefer, tc.guarded)
			if err != nil || got.Provider != tc.provider || got.Account != tc.account || got.Reviewer != tc.reviewer || got.Reason == "" {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	accounts[0].Headroom = 75
	got, _ := pickWorker(accounts, "claude", false)
	if got.Account != "c1" {
		t.Fatalf("preference tie: %+v", got)
	}
	got, _ = pickWorker([]workerAccount{{Provider: "claude", Account: "one", Headroom: 90}, {Provider: "claude", Account: "two", Headroom: 80}, {Provider: "codex", Account: "dry", Headroom: 0}}, "", false)
	if got.Reviewer != "claude" || got.ReviewerAccount != "two" {
		t.Fatalf("same-provider fallback: %+v", got)
	}
	if _, err := pickWorker([]workerAccount{{Provider: "codex", Account: "x", Headroom: 90}}, "", true); err == nil {
		t.Fatal("guarded folder fell back to Codex")
	}
	if _, err := pickWorker([]workerAccount{{Provider: "claude", Account: "dry", Headroom: 0}}, "", false); err == nil {
		t.Fatal("exhausted account selected")
	}
}
func TestQuotaWorkerGuardedFolder(t *testing.T) {
	for _, kind := range []string{"hooks", "marker", "plain", "empty-hooks", "broken-settings"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			want := false
			switch kind {
			case "hooks", "empty-hooks", "broken-settings":
				if err := os.Mkdir(filepath.Join(dir, ".claude"), 0700); err != nil {
					t.Fatal(err)
				}
				raw := `{"hooks":{"PreToolUse":[{}]}}`
				want = true
				if kind == "empty-hooks" {
					raw = `{"hooks":{}}`
					want = false
				}
				if kind == "broken-settings" {
					raw = `{`
				}
				if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker":
				want = true
				if err := os.WriteFile(filepath.Join(dir, ".solu-no-autoanswer"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := guardedWorkerFolder(dir); got != want {
				t.Fatalf("guard=%v want %v", got, want)
			}
		})
	}
}

func newQuotaWorkerAPI(t *testing.T) (*apiServer, *[]SpawnOpts) {
	t.Helper()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Put(&Workspace{Name: "Work", PaneIds: []string{"lead"}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(t.TempDir())
	lead := transactionTestPane("lead")
	mgr.panes[lead.ID] = lead
	a := &apiServer{mgr: mgr, ws: ws, panes: &failingPaneStore{}, dataDir: t.TempDir(), mcpScopes: newMcpScopeStore()}
	a.asks = newAskStore(t.TempDir(), nil)
	var calls []SpawnOpts
	a.spawnPane = func(o SpawnOpts) (*Pane, error) {
		calls = append(calls, o)
		p := transactionTestPane(fmt.Sprintf("child-%d", len(calls)))
		p.Cmd, p.Cwd, p.Origin, p.Title = o.Cmd, o.Cwd, o.Origin, o.Title
		mgr.mu.Lock()
		mgr.panes[p.ID] = p
		mgr.mu.Unlock()
		return p, nil
	}
	oldAccounts, oldPrepare := quotaWorkerAccounts, prepareQuotaWorker
	quotaWorkerAccounts = func(*apiServer, time.Time) []workerAccount {
		return []workerAccount{{Provider: "codex", Account: "test", Headroom: 90}, {Provider: "claude", Account: "review", Headroom: 80}}
	}
	prepareQuotaWorker = func(*apiServer, workerInfo, string, *SpawnOpts) (func(), error) { return nil, nil }
	t.Cleanup(func() { quotaWorkerAccounts, prepareQuotaWorker = oldAccounts, oldPrepare })
	return a, &calls
}
func workerCall(t *testing.T, a *apiServer, priority string) map[string]any {
	t.Helper()
	text, isErr := callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "priority": priority, "workspace": "Work", "cwd": t.TempDir(), "wait_ready": false, "title": "test"})
	if isErr {
		t.Fatal(text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestQuotaWorkerCapOrderingUrgentCancel(t *testing.T) {
	a, calls := newQuotaWorkerAPI(t)
	for i := 0; i < 4; i++ {
		out := workerCall(t, a, "normal")
		if out["id"] == nil || out["reviewer"] != "claude" {
			t.Fatalf("running result: %+v", out)
		}
	}
	later := workerCall(t, a, "later")
	normal := workerCall(t, a, "normal")
	if later["queued"] != true || normal["position"] != float64(1) || len(*calls) != 4 {
		t.Fatalf("queue: later=%v normal=%v calls=%d", later, normal, len(*calls))
	}
	urgent := workerCall(t, a, "urgent")
	if urgent["id"] == nil || len(*calls) != 5 {
		t.Fatalf("urgent extra slot: %+v", urgent)
	}
	extra := workerCall(t, a, "urgent")
	if extra["queued"] != true || extra["position"] != float64(1) {
		t.Fatalf("second urgent must queue: %+v", extra)
	}
	text, isErr := mcpResultText(t, a.cancelQueuedWorker(map[string]any{"queue_id": normal["queue_id"]}, "stranger"))
	if !isErr {
		t.Fatal("foreign cancellation accepted: " + text)
	}
	_, isErr = mcpResultText(t, a.cancelQueuedWorker(map[string]any{"queue_id": normal["queue_id"]}, "lead"))
	if isErr {
		t.Fatal("own cancellation failed")
	}
	q := a.workers()
	q.mu.Lock()
	var pending []string
	for _, r := range q.requests {
		if r.Info.State == "queued" {
			pending = append(pending, r.Info.Priority)
		}
	}
	q.mu.Unlock()
	if strings.Join(pending, ",") != "urgent,later" {
		t.Fatalf("order after cancellation %v", pending)
	}
	summaries := a.queuedWorkerSummaries()
	if len(summaries) != 3 || summaries[0].Worker == nil {
		t.Fatalf("queue not exposed: %+v", summaries)
	}
	// Closing one worker frees the urgent overflow slot before normal/later.
	a.mgr.mu.Lock()
	delete(a.mgr.panes, "child-1")
	a.mgr.mu.Unlock()
	// Reserve exactly the worker the production tick would dispatch, then start
	// synchronously so the test makes no timing assumptions about readiness.
	q.mu.Lock()
	var next *workerRequest
	for _, r := range q.requests {
		if r.Info.State == "queued" && a.workerRunningLocked(q, r.Lead) < q.settings.MaxRunning+boolInt(r.Info.Priority == "urgent") {
			next = r
			r.Info.State = "running"
			break
		}
	}
	q.mu.Unlock()
	// The tick first observes closed panes and updates the running record.
	if next != nil {
		t.Fatal("closed worker record should still hold reservation before reconciliation")
	}
	a.tickWorkers(time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		started := false
		for _, r := range q.requests {
			if r.Info.ID == extra["queue_id"] {
				started = r.PaneID != ""
			}
		}
		q.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued urgent worker never started after a slot freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(*calls) != 6 {
		t.Fatalf("expected only urgent to start: %d", len(*calls))
	}

}
func TestQuotaWorkerAuthorityRechecked(t *testing.T) {
	for _, kind := range []string{"mutators-disabled", "scope-revoked", "scope-expired", "caller-closed", "lead-closed", "guard-denied"} {
		t.Run(kind, func(t *testing.T) {
			a, calls := newQuotaWorkerAPI(t)
			r := &workerRequest{Info: workerInfo{ID: "pending", State: "running", Priority: "normal"}, Caller: "lead", Lead: "lead", Args: map[string]any{"cmd": "auto", "workspace": "Work", "wait_ready": false}, Created: time.Now()}
			switch kind {
			case "mutators-disabled":
				t.Setenv(mcpMutationPolicyEnv, "deny")
			case "scope-revoked", "scope-expired":
				token := a.mcpScopes.Mint("test", []string{"spawn_pane"}, time.Hour)
				r.Scope, _ = a.mcpScopes.Lookup(token)
				if kind == "scope-revoked" {
					a.mcpScopes.Revoke(token)
				} else {
					r.Scope.Expires = time.Now().Add(-time.Second)
				}
			case "caller-closed":
				r.Caller = "missing"
			case "lead-closed":
				r.Lead = "missing"
			case "guard-denied":
				old := mcpCallGuards
				mcpCallGuards = append(append([]func(*apiServer, string, map[string]any, *mcpScope) (map[string]any, any){}, old...), func(_ *apiServer, _ string, args map[string]any, _ *mcpScope) (map[string]any, any) {
					return nil, toolErr("new policy")
				})
				t.Cleanup(func() { mcpCallGuards = old })
			}
			_, isErr := mcpResultText(t, a.startAutoWorker(r))
			if !isErr || len(*calls) != 0 || r.Info.State != "closed" {
				t.Fatalf("unauthorized queued spawn ran: %+v calls %d", r, len(*calls))
			}
		})
	}
}
func TestQuotaWorkerConcurrentReservations(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	q := a.workers()
	q.loaded = true
	q.settings = workerSettings{MaxRunning: 1, FinishAfterMinutes: 5}
	// Block the first start after its slot is reserved; later requests must queue.
	entered, release := make(chan struct{}), make(chan struct{})
	quotaWorkerAccounts = func(*apiServer, time.Time) []workerAccount { close(entered); <-release; return nil }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.mcpToolAutoWorker(map[string]any{"cmd": "auto", "workspace": "Work", "cwd": t.TempDir()}, "lead", nil)
	}()
	<-entered
	result := a.mcpToolAutoWorker(map[string]any{"cmd": "auto", "workspace": "Work", "cwd": t.TempDir()}, "lead", nil)
	text, isErr := mcpResultText(t, result)
	if isErr || !strings.Contains(text, `"queued":true`) {
		t.Fatalf("reservation lost: %s", text)
	}
	close(release)
	wg.Wait()
}
func TestQuotaWorkerLifecycle(t *testing.T) {
	now := time.Now()
	var since time.Time
	var input int64
	step := func(alive, idle, complete bool, in int64, at time.Time) string {
		return nextWorkerState("running", alive, idle, complete, in, at, &since, &input, 5*time.Minute)
	}
	if got := step(true, true, false, 1, now); got != "running" {
		t.Fatal(got)
	}
	if got := step(true, true, true, 1, now); got != "running" {
		t.Fatal(got)
	}
	if got := step(true, true, true, 1, now.Add(5*time.Minute)); got != "finished" {
		t.Fatal(got)
	}
	if got := step(true, true, true, 2, now.Add(6*time.Minute)); got != "running" || !since.IsZero() {
		t.Fatal("follow-up failed to reopen worker")
	}
	if got := step(true, false, true, 2, now.Add(7*time.Minute)); got != "running" {
		t.Fatal("permission/busy state marked finished")
	}
	if got := step(false, false, false, 2, now.Add(8*time.Minute)); got != "closed" {
		t.Fatal(got)
	}
}
func TestQuotaWorkerMCPSchema(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range mcpCoreTools() {
		if tool.Name != "spawn_pane" && tool.Name != "cancel_queued_worker" {
			continue
		}
		seen[tool.Name] = true
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
			AdditionalProperties bool `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.AdditionalProperties {
			t.Fatal("schema accepts arbitrary fields")
		}
		if tool.Name == "spawn_pane" {
			if strings.Join(schema.Properties["prefer"].Enum, ",") != "claude,codex" || strings.Join(schema.Properties["priority"].Enum, ",") != "urgent,normal,later" || !strings.Contains(tool.Description, "reviewer") {
				t.Fatalf("missing auto schema: %+v", schema)
			}
		} else {
			if _, ok := schema.Properties["queue_id"]; !ok {
				t.Fatal("missing queue id")
			}
			if !mcpToolRequiresMutationPolicy(tool.Name) || !mcpPublicToolNames[tool.Name] {
				t.Fatal("cancel tool bypasses mutation policy or is hidden")
			}
		}
	}
	if len(seen) != 2 {
		t.Fatal("missing worker tools")
	}
}

func TestQuotaWorkerStartingPaneIsNotAdoptedTwice(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	released := 0
	prepareQuotaWorker = func(_ *apiServer, _ workerInfo, _ string, opts *SpawnOpts) (func(), error) {
		opts.Env = append(opts.Env, "CODEX_HOME=frozen-worker-home")
		opts.Origin = &PaneOrigin{Worker: &workerInfo{LeaseID: "initial-lease", CredentialRevision: "initial-revision"}}
		return func() { released++ }, nil
	}
	original := a.spawnPane
	a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
		p, err := original(opts)
		// This is the readiness window: the pane exists, but the spawn has not
		// returned or bound its pane id yet. A scheduler tick must not adopt it.
		a.tickWorkers(time.Now())
		q := a.workers()
		q.mu.Lock()
		defer q.mu.Unlock()
		if len(q.requests) != 1 || a.workerRunningLocked(q, "lead") != 1 {
			t.Fatalf("starting worker counted twice: %+v", q.requests)
		}
		if released != 0 || q.requests[0].release == nil {
			t.Fatal("initial pane registration released its live credential lease")
		}
		return p, err
	}
	workerCall(t, a, "normal")
	if released != 0 {
		t.Fatal("spawn released its live credential lease")
	}
}
func TestQuotaWorkerUntrustedHTTPOriginCannotPinAccount(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	forged := &PaneOrigin{Source: paneOriginAgent, Worker: &workerInfo{ID: "forged", Provider: "codex", Account: "other", ConfigDir: "/arbitrary"}}
	for _, restore := range []bool{true, false} {
		got := a.validatedWorkerOrigin(forged, createPaneReq{Restore: restore, CodexThreadID: "unknown"})
		if got.Worker != nil {
			t.Fatalf("untrusted worker pin survived: %+v", got.Worker)
		}
	}
	if forged.Worker == nil {
		t.Fatal("mutated supplied metadata")
	}
	// Reproduce the loopback workspace-write/restore chain. Layout metadata
	// cannot turn a forged origin into authority to lease a saved/team account.
	forged.Worker.ID = "saved"
	forged.LeadPaneID = "forged-lead"
	wp := Workspace{Name: "Saved", Panes: []PaneDef{{ID: "old", CodexThreadID: "thread", Origin: forged}}}
	raw, _ := json.Marshal(wp)
	w := httptest.NewRecorder()
	a.putWorkspace(w, httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(string(raw))))
	if w.Code != 200 {
		t.Fatalf("save layout: %d %s", w.Code, w.Body.String())
	}
	raw, _ = json.Marshal(createPaneReq{Restore: true, Cmd: []string{"codex"}, CodexThreadID: "thread", Origin: forged, Workspace: "Saved", Cwd: t.TempDir()})
	w = httptest.NewRecorder()
	a.createPane(w, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(string(raw))))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "matching daemon-saved conversation") {
		t.Fatalf("forged restore: %d %s", w.Code, w.Body.String())
	}
	trusted := &PaneOrigin{Source: paneOriginAgent, LeadPaneID: "real-lead", Worker: &workerInfo{ID: "saved", Provider: "codex", Account: "owned", ConfigDir: "/real"}}
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	p := transactionTestPane("old")
	p.Origin, p.CodexThreadID = trusted, "thread"
	if err := store.Put(p); err != nil {
		t.Fatal(err)
	}
	got := a.validatedWorkerOrigin(forged, createPaneReq{Restore: true, CodexThreadID: "thread"})
	if got.Worker == nil || got.Worker.Account != "owned" || got.Worker.ConfigDir != "/real" || got.LeadPaneID != "real-lead" {
		t.Fatalf("restore did not use daemon-owned metadata: %+v", got)
	}
	oldHook := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "lead"} }
	t.Cleanup(func() { identifyCallerHook = oldHook })
	w = httptest.NewRecorder()
	a.createPane(w, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(string(raw))))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "human_required") {
		t.Fatalf("pane restored saved worker credentials: %d %s", w.Code, w.Body.String())
	}

}

func TestQuotaWorkerQuickRespawnRefreshesCredentialIncarnation(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	out := workerCall(t, a, "normal")
	id := out["id"].(string)
	q := a.workers()
	q.mu.Lock()
	r := q.requests[0]
	r.Info.ConfigDir = "old-home"
	r.Info.LeaseID = "old-lease"
	r.Info.CredentialRevision = "old-revision"
	released := 0
	r.release = func() { released++ }
	q.mu.Unlock()
	p, _ := a.mgr.Get(id)
	p.mu.Lock()
	o := p.Origin.clone()
	o.Worker.ConfigDir = "new-home"
	o.Worker.LeaseID = "new-lease"
	o.Worker.CredentialRevision = "new-revision"
	p.Origin = o
	p.mu.Unlock()
	// No exited sample occurs: this is a switch-mode/quick-resume incarnation.
	a.tickWorkers(time.Now())
	q.mu.Lock()
	got := r.Info
	q.mu.Unlock()
	if got.ConfigDir != "new-home" || got.LeaseID != "new-lease" || got.CredentialRevision != "new-revision" || released != 1 {
		t.Fatalf("stale incarnation survived: %+v, releases=%d", got, released)
	}
	// A subsequent lifecycle write must preserve the new credential ownership.
	p.mu.Lock()
	p.Status = PaneExited
	p.mu.Unlock()
	a.tickWorkers(time.Now())
	got = *p.Snapshot().Origin.Worker
	if got.State != "closed" || got.ConfigDir != "new-home" || got.LeaseID != "new-lease" || got.CredentialRevision != "new-revision" || released != 1 {
		t.Fatalf("lifecycle overwrote new incarnation: %+v releases=%d", got, released)
	}
}
func TestQuotaWorkerCoordinationRedactionAndNativeCodex(t *testing.T) {
	w := &workerInfo{ID: "worker", Provider: "codex", Account: "named", ConfigDir: "private-dir", LeaseID: "private-lease", CredentialRevision: "private-revision", State: "running"}
	got := visibleWorkerInfo(w)
	if got.ConfigDir != "" || got.LeaseID != "" || got.CredentialRevision != "" || got.Account != "named" || w.ConfigDir == "" {
		t.Fatalf("bad coordination projection: %+v", got)
	}
	plain := strings.Join(quotaNativeCodexArgv(SpawnOpts{}), " ")
	if plain != "codex app-server --stdio" {
		t.Fatal(plain)
	}
	auto := strings.Join(quotaNativeCodexArgv(SpawnOpts{workerAccount: "named"}), " ")
	if !strings.Contains(auto, `model_provider="openai"`) || !strings.Contains(auto, `cli_auth_credentials_store="file"`) || !strings.Contains(auto, "app-server --stdio") {
		t.Fatal(auto)
	}
}
