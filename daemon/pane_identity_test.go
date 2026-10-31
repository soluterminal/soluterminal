package main

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func identityTestServer(t *testing.T) *apiServer {
	t.Helper()
	a := &apiServer{mgr: NewManager("")}
	lead := testPaneWithTail(t, "lead", "claude", "")
	lead.Agent = agentClaude
	worker := testPaneWithTail(t, "worker", "codex", "")
	worker.Agent = agentCodex
	worker.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead"}
	other := testPaneWithTail(t, "other", "bash", "")
	for _, p := range []*Pane{lead, worker, other} {
		a.mgr.panes[p.ID] = p
	}
	a.ws = &WorkspaceStore{data: map[string]*Workspace{"w": {Name: "w", PaneIds: []string{"lead", "worker", "other"}}}}
	return a
}

func strp(s string) *string { return &s }

func TestNormalizePaneHandle(t *testing.T) {
	for in, want := range map[string]string{
		"api-fix": "api-fix", "@API-Fix": "api-fix", "  @lead ": "lead", "r2": "r2", "": "",
	} {
		if got, err := normalizePaneHandle(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	// No letters reads as a pane number; spaces, underscores, a leading dash
	// and over-long names are refused.
	for _, in := range []string{"3", "#3", "api fix", "api_fix", "-x", strings.Repeat("a", 25), "é"} {
		if _, err := normalizePaneHandle(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestPaneHandleIsUniqueAndClearable(t *testing.T) {
	a := identityTestServer(t)
	lead, _ := a.mgr.Get("lead")
	worker, _ := a.mgr.Get("worker")
	now := time.Now()
	if _, err := a.updatePaneIdentity(lead, strp("@Lead"), nil, "user", now); err != nil {
		t.Fatal(err)
	}
	_, err := a.updatePaneIdentity(worker, strp("lead"), nil, "user", now)
	var taken paneHandleTakenError
	if err == nil || !strings.Contains(err.Error(), "#1 Claude Code") || paneIdentityErrStatus(err) != 409 {
		t.Fatalf("duplicate handle: %v", err)
	}
	if !strings.Contains(err.Error(), "taken") || !errorsAs(err, &taken) {
		t.Fatalf("want a taken error, got %v", err)
	}
	// Re-setting your own handle is not a clash.
	if _, err := a.updatePaneIdentity(lead, strp("lead"), nil, "user", now); err != nil {
		t.Fatal(err)
	}
	// Clearing frees it for another pane, and an empty identity is dropped.
	if id, err := a.updatePaneIdentity(lead, strp(""), nil, "user", now); err != nil || id != nil {
		t.Fatalf("clear: %+v, %v", id, err)
	}
	if lead.Snapshot().Identity != nil {
		t.Fatal("cleared identity should be nil")
	}
	if _, err := a.updatePaneIdentity(worker, strp("lead"), nil, "user", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.updatePaneIdentity(worker, nil, nil, "user", now); err == nil || paneIdentityErrStatus(err) != 400 {
		t.Fatalf("empty update: %v", err)
	}
}

func errorsAs(err error, target *paneHandleTakenError) bool {
	t, ok := err.(paneHandleTakenError)
	if ok {
		*target = t
	}
	return ok
}

func TestWorkingOnIsOneLineWithProvenance(t *testing.T) {
	a := identityTestServer(t)
	p, _ := a.mgr.Get("worker")
	now := time.UnixMilli(1_700_000_000_000)
	id, err := a.updatePaneIdentity(p, nil, strp("  Fixing the\n login  flow \t"), "#1 Claude", now)
	if err != nil {
		t.Fatal(err)
	}
	if id.WorkingOn != "Fixing the login flow" || id.WorkingOnBy != "#1 Claude" || id.WorkingOnAt != now.UnixMilli() {
		t.Fatalf("got %+v", id)
	}
	long, _ := a.updatePaneIdentity(p, nil, strp(strings.Repeat("x", 300)), "user", now)
	if n := len([]rune(long.WorkingOn)); n != paneWorkingOnMaxLen || !strings.HasSuffix(long.WorkingOn, "…") {
		t.Fatalf("long working_on kept %d runes: %q", n, long.WorkingOn)
	}
	// Setting a handle keeps the working-on line and its provenance.
	id, _ = a.updatePaneIdentity(p, strp("api-fix"), nil, "user", now.Add(time.Hour))
	if id.Handle != "api-fix" || id.WorkingOnBy != "user" || id.WorkingOnAt != now.UnixMilli() {
		t.Fatalf("handle change disturbed working_on: %+v", id)
	}
}

func TestPaneRefsAcceptHandles(t *testing.T) {
	a := identityTestServer(t)
	worker, _ := a.mgr.Get("worker")
	if _, err := a.updatePaneIdentity(worker, strp("api-fix"), nil, "user", time.Now()); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"@api-fix": "worker", "api-fix": "worker", "@API-FIX": "worker", "#2": "worker", "worker": "worker", "@nobody": ""} {
		if got := a.resolvePaneRef(ref, "lead"); got != want {
			t.Errorf("resolvePaneRef(%q) = %q, want %q", ref, got, want)
		}
	}
	// Every pane tool sees the id: the transport rewrites pane_id first.
	res := a.mcpCallToolTransport(json.RawMessage(`{"name":"get_pane_state","arguments":{"pane_id":"@api-fix"}}`), nil, "lead")
	if text, failed := mcpResultText(t, res); failed || strings.Contains(text, "no pane") {
		t.Fatalf("get_pane_state by handle: %q", text)
	}
}

func TestSetPaneInfoAuthority(t *testing.T) {
	a := identityTestServer(t)
	// Own pane by default.
	text, failed := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"handle": "lead", "working_on": "Coordinating the fix"}, "lead"))
	if failed || !strings.Contains(text, "@lead") {
		t.Fatalf("own pane: %q", text)
	}
	// A pane it started.
	if text, failed := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "worker", "handle": "api-fix"}, "lead")); failed {
		t.Fatalf("started pane: %q", text)
	}
	// Someone else's pane needs the user's ask.
	if text, failed := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "lead", "working_on": "hijack"}, "worker")); !failed || !strings.Contains(text, "user_requested") {
		t.Fatalf("foreign pane: %q", text)
	}
	if _, failed := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "lead", "working_on": "asked"}, "worker")); !failed {
		t.Fatal("still refused without user_requested")
	}
	// user_requested alone changes nothing: the person allows it in the app.
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	text, failed = mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "lead", "working_on": "asked", "user_requested": true}, "worker"))
	if failed || !strings.Contains(text, "waiting for your OK in the app") {
		t.Fatalf("user_requested: %q", text)
	}
	lead, _ := a.mgr.Get("lead")
	if got := lead.Snapshot().Identity; got != nil && got.WorkingOn == "asked" {
		t.Fatal("changed before the person allowed it")
	}
	pending := a.pendingPaneConfirms()
	if len(pending) != 1 || pending[0].Message != "@api-fix wants to change pane #1 “claude”: working on → “asked”." {
		t.Fatalf("pending = %+v", pending)
	}
	if res, ok := a.decidePaneConfirm(pending[0].ID, true); !ok {
		t.Fatal("no request to allow")
	} else if text, failed := mcpResultText(t, res); failed {
		t.Fatalf("allowed change: %q", text)
	}
	if got := lead.Snapshot().Identity; got.WorkingOn != "asked" || got.WorkingOnBy != "#2 Codex" {
		t.Fatalf("working_on provenance: %+v", got)
	}
}

func TestHandleShowsInSenderLabelAndListPanes(t *testing.T) {
	a := identityTestServer(t)
	lead, _ := a.mgr.Get("lead")
	if got := a.paneMessageSender("lead", "worker"); got != "#1 Claude" {
		t.Fatalf("no handle: %q", got)
	}
	a.updatePaneIdentity(lead, strp("lead"), strp("Coordinating"), "user", time.Now())
	if got := a.paneMessageSender("lead", "worker"); got != "@lead · #1 Claude" {
		t.Fatalf("with handle: %q", got)
	}
	text, _ := mcpResultText(t, a.mcpToolListPanes(nil, "worker"))
	var rows []mcpPaneSummary
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == "lead" && (r.Handle != "lead" || r.WorkingOn != "Coordinating") {
			t.Fatalf("list_panes row: %+v", r)
		}
	}
}

func TestIdentitySurvivesPersistAndRestore(t *testing.T) {
	a := identityTestServer(t)
	lead, _ := a.mgr.Get("lead")
	a.updatePaneIdentity(lead, strp("lead"), strp("Coordinating"), "user", time.Now())
	pp := persistedFromSnapshot(lead.Snapshot())
	b, _ := json.Marshal(pp)
	var back PersistedPane
	if err := json.Unmarshal(b, &back); err != nil || back.Identity == nil || back.Identity.Handle != "lead" {
		t.Fatalf("persisted identity: %s", b)
	}
	// A restore keeps the working-on line but not a handle another pane holds.
	got, release := a.restoredPaneIdentity(createPaneReq{Restore: true, Identity: &PaneIdentity{Handle: "lead", WorkingOn: "Old work"}})
	release()
	if got == nil || got.Handle != "" || got.WorkingOn != "Old work" {
		t.Fatalf("restore with a taken handle: %+v", got)
	}
	got, release = a.restoredPaneIdentity(createPaneReq{Restore: true, Identity: &PaneIdentity{Handle: "@Free-One"}})
	if got == nil || got.Handle != "free-one" {
		t.Fatalf("restore with a free handle: %+v", got)
	}
	// Until the restored pane exists, nobody else can take its handle.
	worker, _ := a.mgr.Get("worker")
	if _, err := a.updatePaneIdentity(worker, strp("free-one"), nil, "user", time.Now()); err == nil {
		t.Fatal("a reserved handle was claimed")
	}
	release()
	if _, err := a.updatePaneIdentity(worker, strp("free-one"), nil, "user", time.Now()); err != nil {
		t.Fatalf("released handle: %v", err)
	}
	if got, _ := a.restoredPaneIdentity(createPaneReq{Identity: &PaneIdentity{Handle: "x1"}}); got != nil {
		t.Fatal("only a Restore brings an identity back")
	}
}

// Concurrent claims of one handle: exactly one wins, whichever path it takes.
func TestPaneHandleClaimsAreAtomic(t *testing.T) {
	a := identityTestServer(t)
	lead, _ := a.mgr.Get("lead")
	worker, _ := a.mgr.Get("worker")
	other, _ := a.mgr.Get("other")
	var wins atomic.Int32
	var releases []func()
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Six claimants: three reservations and an edit from each distinct pane.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 2 {
			case 0:
				if r, err := a.reservePaneHandle("shared"); err == nil {
					wins.Add(1)
					mu.Lock()
					releases = append(releases, r)
					mu.Unlock()
				}
			default:
				p := []*Pane{lead, worker, other}[i%3]
				if _, err := a.updatePaneIdentity(p, strp("shared"), nil, "user", time.Now()); err == nil {
					wins.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()
	for _, r := range releases {
		r()
	}
	if wins.Load() != 1 {
		t.Fatalf("%d claims of one handle succeeded", wins.Load())
	}
}

// If two panes ever held one handle, @handle resolves to neither: a pane tool
// must not act on whichever duplicate came first.
func TestDuplicateHandleResolvesToNothing(t *testing.T) {
	a := identityTestServer(t)
	lead, _ := a.mgr.Get("lead")
	worker, _ := a.mgr.Get("worker")
	lead.SetIdentity(&PaneIdentity{Handle: "dup"})
	worker.SetIdentity(&PaneIdentity{Handle: "dup"})
	if got := a.resolvePaneRef("@dup", "other"); got != "" {
		t.Fatalf("ambiguous handle resolved to %q", got)
	}
}

// A pane mid-restart (out of the manager, still saved) keeps its handle; a
// closed one (record removed) frees it.
func TestRestartingPaneKeepsItsHandle(t *testing.T) {
	a := identityTestServer(t)
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	lead, _ := a.mgr.Get("lead")
	worker, _ := a.mgr.Get("worker")
	if _, err := a.updatePaneIdentity(lead, strp("lead"), nil, "user", time.Now()); err != nil {
		t.Fatal(err)
	}
	delete(a.mgr.panes, "lead") // the respawn gap
	if _, err := a.updatePaneIdentity(worker, strp("lead"), nil, "user", time.Now()); err == nil || !strings.Contains(err.Error(), "while it restarts") {
		t.Fatalf("claimed a restarting pane's handle: %v", err)
	}
	if _, err := a.reservePaneHandle("lead"); err == nil {
		t.Fatal("reserved a restarting pane's handle")
	}
	if err := store.Remove("lead"); err != nil { // closed for real
		t.Fatal(err)
	}
	if _, err := a.updatePaneIdentity(worker, strp("lead"), nil, "user", time.Now()); err != nil {
		t.Fatalf("a closed pane's handle stayed taken: %v", err)
	}
}
