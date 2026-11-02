package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createWorkspaceFixture(t *testing.T) *apiServer {
	t.Helper()
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(t.TempDir())
	mgr.panes["lead"] = &Pane{ID: "lead", Status: PaneRunning}
	mgr.panes["other"] = &Pane{ID: "other", Status: PaneRunning}
	return &apiServer{ws: store, uiBus: newUIBus(), mgr: mgr, gate: &authGate{token: "owner-token"}}
}

func TestMCPCreateWorkspaceEmptyAndNoOverwrite(t *testing.T) {
	a := createWorkspaceFixture(t)
	events := a.uiBus.subscribe()
	text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "  New  ", "panes": []string{"untrusted"}}, paneCaller{ID: "lead"}))
	if failed {
		t.Fatal(text)
	}
	var workspace Workspace
	if err := json.Unmarshal([]byte(text), &workspace); err != nil || workspace.Name != "New" || workspace.Layout != nil || len(workspace.PaneIds) != 0 || len(workspace.Panes) != 0 {
		t.Fatalf("workspace = %s, err %v", text, err)
	}
	select {
	case event := <-events:
		if event.Type != "workspace.updated" || event.Workspace != "New" {
			t.Fatalf("event = %+v", event)
		}
	default:
		t.Fatal("missing workspace update")
	}
	for _, name := range []string{"New", "new", "NEW", "_current", "_CURRENT", "  ", "Bad\nname", "Bad\x00name", "Bad\u202ename", strings.Repeat("x", 121)} {
		text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": name}, paneCaller{ID: "lead"}))
		if !failed {
			t.Fatalf("accepted invalid/existing %q: %s", name, text)
		}
	}
	if len(a.ws.List()) != 1 || len(a.workspaceCreates.created["lead"]) != 1 {
		t.Fatal("failed creates changed store or consumed quota")
	}
	reloaded, err := NewWorkspaceStore(filepath.Dir(a.ws.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reloaded.Get("New"); !exists {
		t.Fatal("not persisted")
	}
}

func TestMCPCreateWorkspacePreservesExistingLayout(t *testing.T) {
	a := createWorkspaceFixture(t)
	original := &Workspace{Name: "Existing", PaneIds: []string{"p"}, Panes: []PaneDef{{ID: "p", Cmd: []string{"bash"}}}, Layout: LayoutNode{"type": "pane", "paneId": "p"}, Protected: true}
	if err := a.ws.Put(original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.ws.path)
	if err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "existing"}, paneCaller{ID: "lead"})); !failed {
		t.Fatal(text)
	}
	after, err := os.ReadFile(a.ws.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("existing workspace was rewritten")
	}
}

func TestMCPCreateWorkspaceRollingQuota(t *testing.T) {
	a := createWorkspaceFixture(t)
	for i := 0; i < 6; i++ {
		text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": fmt.Sprint(i)}, paneCaller{ID: "lead"}))
		if failed != (i == 5) {
			t.Fatalf("create %d: failed=%t %s", i, failed, text)
		}
		if i == 5 && !strings.Contains(text, "5 new workspaces") {
			t.Fatal(text)
		}
	}
	if _, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "Other agent"}, paneCaller{ID: "other"})); failed {
		t.Fatal("other agent lost its budget")
	}
	a.workspaceCreates.created["lead"][0] = time.Now().Add(-time.Hour - time.Second)
	if _, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "After expiry"}, paneCaller{ID: "lead"})); failed {
		t.Fatal("expired quota not released")
	}
}

func TestMCPCreateWorkspaceUnverifiedClaimsRefused(t *testing.T) {
	a := createWorkspaceFixture(t)
	for _, caller := range []paneCaller{{}, {Claimed: "lead"}, {Claimed: "other"}, {ID: "missing"}} {
		if text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "New"}, caller)); !failed || !strings.Contains(text, "verified connection") {
			t.Fatalf("caller %+v: %s", caller, text)
		}
	}
	a.mgr.panes["lead"].Status = PaneExited
	if _, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "New"}, paneCaller{ID: "lead"})); !failed {
		t.Fatal("ended pane created workspace")
	}
	if len(a.ws.List()) != 0 || len(a.workspaceCreates.created) != 0 {
		t.Fatal("refused caller changed store or quota")
	}
}

func TestMCPCreateWorkspaceConcurrentQuota(t *testing.T) {
	a := createWorkspaceFixture(t)
	var passed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": fmt.Sprint(i)}, paneCaller{ID: "lead"}))
			if !failed {
				passed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if passed.Load() != 5 || len(a.ws.List()) != 5 {
		t.Fatalf("created %d, stored %d", passed.Load(), len(a.ws.List()))
	}
}

func TestWorkspaceCreateEmptyAtomicNameCheck(t *testing.T) {
	a := createWorkspaceFixture(t)
	var passed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "Mixed"
			if i%2 == 0 {
				name = "MIXED"
			}
			if _, err := a.ws.CreateEmpty(name); err == nil {
				passed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if passed.Load() != 1 || len(a.ws.List()) != 1 {
		t.Fatalf("created %d, stored %d", passed.Load(), len(a.ws.List()))
	}
}

func TestMCPCreateWorkspacePersistenceFailurePreservesQuota(t *testing.T) {
	a := createWorkspaceFixture(t)
	oldPath := a.ws.path
	obstacle := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(obstacle, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ws.path = filepath.Join(obstacle, "workspaces.json")
	if _, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "New"}, paneCaller{ID: "lead"})); !failed {
		t.Fatal("persistence failure accepted")
	}
	if len(a.ws.List()) != 0 || len(a.workspaceCreates.created["lead"]) != 0 {
		t.Fatal("failed write changed store or quota")
	}
	a.ws.path = oldPath
	if _, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "New"}, paneCaller{ID: "lead"})); failed {
		t.Fatal("could not retry")
	}
}

// The ordinary MCP header fallback is not proof for a rate-limit identity.
func TestMCPCreateWorkspaceRequiresTracedConnection(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	for _, who := range []string{"traced pane", "traced pane with another header", "detached", "worker", "unverified", "untraceable header", "relay human"} {
		t.Run(who, func(t *testing.T) {
			a := createWorkspaceFixture(t)
			prev := identifyCallerHook
			identifyCallerHook = nil
			t.Cleanup(func() { identifyCallerHook = prev })
			stubPeerPID(t, 0, false)
			header := "lead"
			switch who {
			case "traced pane", "traced pane with another header":
				child := startSleeper(t)
				a.mgr.panes["lead"].Pid = child.Process.Pid
				stubPeerPID(t, child.Process.Pid, true)
				if who == "traced pane with another header" {
					header = "other"
				}
			case "detached":
				child := startSleeper(t, "SOLU_PANE_ID=lead")
				stubPeerPID(t, child.Process.Pid, true)
			case "worker":
				stubPeerPID(t, os.Getpid(), true)
			case "unverified":
				stubPeerPID(t, 2147483647, true)
			}
			req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_workspace","arguments":{"name":"New"}}}`))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("X-Soluterminal-Pane", header)
			req.Header.Set("Authorization", "Bearer owner-token")
			if who == "relay human" {
				req = req.WithContext(context.WithValue(req.Context(), relayTransportKey{}, true))
			}
			rec := httptest.NewRecorder()
			a.mcp(rec, req)
			var response rpcResp
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != 200 || response.Error != nil {
				t.Fatalf("response %d %s, err %v", rec.Code, rec.Body.String(), err)
			}
			text, failed := mcpResultText(t, response.Result)
			allowed := strings.HasPrefix(who, "traced pane")
			if failed == allowed {
				t.Fatalf("caller %s: %s", who, text)
			}
			if allowed {
				if len(a.ws.List()) != 1 || len(a.workspaceCreates.created["lead"]) != 1 || len(a.workspaceCreates.created["other"]) != 0 {
					t.Fatal("traced caller did not own the quota")
				}
			} else if len(a.ws.List()) != 0 || len(a.workspaceCreates.created) != 0 || !strings.Contains(text, "verified connection") {
				t.Fatalf("unverified caller changed state or got wrong message: %s", text)
			}
		})
	}
}

func TestCreateWorkspaceNamePolicyMatchesStoreAndMCP(t *testing.T) {
	for _, path := range []string{"store", "MCP"} {
		t.Run(path, func(t *testing.T) {
			a := createWorkspaceFixture(t)
			create := func(name string) bool {
				if path == "store" {
					_, err := a.ws.CreateEmpty(name)
					return err == nil
				}
				_, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": name}, paneCaller{ID: "lead"}))
				return !failed
			}
			for _, name := range []string{"_current", "_CuRrEnT", "Bad\nname", "Bad\rname", "Bad\tname", "Bad\x00name", "Bad\u202ename", "Bad\u2066name", strings.Repeat("a", 121), strings.Repeat("界", 121)} {
				if create(name) {
					t.Fatalf("accepted invalid name %q", name)
				}
			}
			if len(a.ws.List()) != 0 || len(a.workspaceCreates.created["lead"]) != 0 {
				t.Fatal("invalid name changed store or quota")
			}
			name := strings.Repeat("界", 120)
			if !create(name) {
				t.Fatal("120 Unicode runes must be allowed")
			}
			workspace, exists := a.ws.Get(name)
			if !exists || workspace.Name != name || len(a.ws.List()) != 1 {
				t.Fatal("accepted name was not persisted verbatim")
			}
		})
	}
}

func TestMCPCreateWorkspaceScopeEnforcement(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	a := createWorkspaceFixture(t)
	params, err := json.Marshal(toolCallParams{Name: "create_workspace", Arguments: map[string]any{"name": "Scoped workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	scope := &mcpScope{Identity: "creation-test", Tools: map[string]bool{"list_workspaces": true}, Expires: time.Now().Add(time.Minute)}
	text, failed := mcpResultText(t, a.mcpCallTool(params, scope, "lead"))
	if !failed || !strings.Contains(text, "not in this session's scope") {
		t.Fatalf("out-of-scope call: %s", text)
	}
	if len(a.ws.List()) != 0 || len(a.workspaceCreates.created) != 0 {
		t.Fatal("scope refusal changed state or quota")
	}
	for _, tool := range mcpToolsForScope(scope) {
		if tool.Name == "create_workspace" {
			t.Fatal("out-of-scope tool advertised")
		}
	}
	scope.Tools["create_workspace"] = true
	text, failed = mcpResultText(t, a.mcpCallTool(params, scope, "lead"))
	if failed || len(a.ws.List()) != 1 || len(a.workspaceCreates.created["lead"]) != 1 {
		t.Fatalf("explicit scope grant failed: %s", text)
	}
	found := false
	for _, tool := range mcpToolsForScope(scope) {
		found = found || tool.Name == "create_workspace"
	}
	if !found {
		t.Fatal("explicitly scoped tool missing from tools/list")
	}
}

func TestMCPCreateWorkspacePublicSurface(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	a := createWorkspaceFixture(t)
	a.entitlements = publicAPIServer(t).entitlements
	list := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}, nil, "lead")
	names := mcpToolNames(t, list)
	if !strings.Contains(","+strings.Join(names, ",")+",", ",create_workspace,") {
		t.Fatalf("public tools missing creation: %v", names)
	}
	init := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "initialize"}, nil, "lead")
	instructions, _ := init.Result.(map[string]any)["instructions"].(string)
	if instructions != mcpPublicServerInstructions || len(instructions) > 1200 || !strings.Contains(instructions, "create_workspace") {
		t.Fatalf("public brief invalid: %d chars", len(instructions))
	}
	params, err := json.Marshal(toolCallParams{Name: "create_workspace", Arguments: map[string]any{"name": "Public workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	reply := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "tools/call", Params: params}, nil, "lead")
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	if text, failed := mcpResultText(t, reply.Result); failed {
		t.Fatalf("public tool refused: %s", text)
	}
	workspace, exists := a.ws.Get("Public workspace")
	if !exists || workspace.Layout != nil || len(workspace.PaneIds) != 0 {
		t.Fatal("public creation was not empty")
	}
}

func TestMCPCreateWorkspaceGlobalRollingQuota(t *testing.T) {
	a := createWorkspaceFixture(t)
	// Five distinct panes each stay within their own quota while racing for
	// the shared budget. A new pane must not grant a fresh global budget.
	for i := 0; i < 5; i++ {
		id := fmt.Sprint("pane-", i)
		a.mgr.panes[id] = &Pane{ID: id, Status: PaneRunning}
	}
	var passed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": fmt.Sprint(i)}, paneCaller{ID: fmt.Sprint("pane-", i%5)}))
			if !failed {
				passed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if passed.Load() != 20 || len(a.ws.List()) != 20 {
		t.Fatalf("created %d, stored %d", passed.Load(), len(a.ws.List()))
	}
	text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "Fresh pane"}, paneCaller{ID: "other"}))
	if !failed || !strings.Contains(text, "20 new workspaces") {
		t.Fatalf("global quota refusal: %s", text)
	}
	for pane, times := range a.workspaceCreates.created {
		a.workspaceCreates.created[pane][0] = time.Now().Add(-time.Hour - time.Second)
		if len(times) > 5 {
			t.Fatal("per-pane quota exceeded")
		}
		break
	}
	if text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "After global expiry"}, paneCaller{ID: "other"})); failed {
		t.Fatal(text)
	}
	if text, failed := mcpResultText(t, a.mcpToolCreateWorkspace(map[string]any{"name": "Full again"}, paneCaller{ID: "other"})); !failed || !strings.Contains(text, "20 new workspaces") {
		t.Fatalf("global quota after refill: %s", text)
	}
}
