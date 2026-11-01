package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type mcpSpawnPaneMetadata struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
}

func putMCPSpawnWorkspace(t *testing.T, store *WorkspaceStore, workspace *Workspace, activeAt time.Time) {
	t.Helper()
	if err := store.Put(workspace); err != nil {
		t.Fatalf("Put workspace %q: %v", workspace.Name, err)
	}
	store.mu.Lock()
	store.data[workspace.Name].ActiveAt = activeAt
	store.mu.Unlock()
}

func newMCPSpawnWorkspaceAPI(t *testing.T, store *WorkspaceStore) (*apiServer, *int) {
	t.Helper()
	mgr := NewManager(t.TempDir())
	spawnCalls := 0
	api := &apiServer{
		mgr:   mgr,
		ws:    store,
		panes: &failingPaneStore{},
		spawnPane: func(opts SpawnOpts) (*Pane, error) {
			spawnCalls++
			p := transactionTestPane(fmt.Sprintf("pane_mcp_workspace_%d", spawnCalls))
			p.Origin = opts.Origin.clone()
			mgr.mu.Lock()
			mgr.panes[p.ID] = p
			mgr.mu.Unlock()
			return p, nil
		},
	}
	return api, &spawnCalls
}

func callMCPSpawnPane(t *testing.T, api *apiServer, callerPane string, args map[string]any) (string, bool) {
	t.Helper()
	params, err := json.Marshal(toolCallParams{Name: "spawn_pane", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	resp := api.mcpDispatch(rpcReq{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  params,
	}, nil, callerPane)
	if resp.Error != nil {
		t.Fatalf("mcpDispatch RPC error: %+v", resp.Error)
	}
	return mcpResultText(t, resp.Result)
}

func requireMCPSpawnSuccess(t *testing.T, api *apiServer, callerPane string, args map[string]any) mcpSpawnPaneMetadata {
	t.Helper()
	text, isErr := callMCPSpawnPane(t, api, callerPane, args)
	if isErr {
		t.Fatalf("spawn_pane returned error: %s", text)
	}
	var metadata mcpSpawnPaneMetadata
	if err := json.Unmarshal([]byte(text), &metadata); err != nil {
		t.Fatalf("decode spawn_pane metadata %q: %v", text, err)
	}
	if metadata.ID == "" || metadata.Workspace == "" {
		t.Fatalf("spawn_pane metadata = %+v, want non-empty id and workspace", metadata)
	}
	// Only new MCP spawns promise a recorded destination. The membership
	// helper below is also used by save/restore tests with legacy origins.
	workspace, ok := api.ws.Get(metadata.Workspace)
	if !ok {
		t.Fatalf("spawned workspace %q not found", metadata.Workspace)
	}
	found := false
	for _, pane := range workspace.Panes {
		if pane.ID == metadata.ID {
			found = true
			if pane.Origin == nil || pane.Origin.Workspace != metadata.Workspace {
				t.Fatalf("spawned pane origin = %+v, want workspace %q", pane.Origin, metadata.Workspace)
			}
			break
		}
	}
	if !found {
		t.Fatalf("spawned pane %q missing from workspace %q", metadata.ID, metadata.Workspace)
	}
	return metadata
}

func requireMCPPaneInWorkspace(t *testing.T, store *WorkspaceStore, workspaceName, paneID string) {
	t.Helper()
	workspace, ok := store.Get(workspaceName)
	if !ok {
		t.Fatalf("workspace %q not found", workspaceName)
	}
	if !mcpWorkspaceHasPaneID(workspace.PaneIds, paneID) {
		t.Fatalf("workspace %q PaneIds = %#v, want %q", workspaceName, workspace.PaneIds, paneID)
	}
	foundDef := false
	for _, pane := range workspace.Panes {
		if pane.ID == paneID {
			foundDef = true
			break
		}
	}
	if !foundDef {
		t.Fatalf("workspace %q Panes = %#v, want definition for %q", workspaceName, workspace.Panes, paneID)
	}
	if !mcpLayoutHasPane(workspace.Layout, paneID) {
		t.Fatalf("workspace %q Layout = %#v, want pane %q", workspaceName, workspace.Layout, paneID)
	}
}

func mcpWorkspaceHasPaneID(ids []string, paneID string) bool {
	for _, id := range ids {
		if id == paneID {
			return true
		}
	}
	return false
}

func mcpLayoutHasPane(value any, paneID string) bool {
	var node map[string]any
	switch typed := value.(type) {
	case LayoutNode:
		node = typed
	case map[string]any:
		node = typed
	default:
		return false
	}
	if id, _ := node["paneId"].(string); id == paneID {
		return true
	}
	return mcpLayoutHasPane(node["a"], paneID) || mcpLayoutHasPane(node["b"], paneID)
}

func TestMCPSpawnPaneWorkspaceResolution(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	baseTime := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	t.Run("explicit workspace is canonicalized and overrides caller", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{
			Name:    "Caller Desk",
			PaneIds: []string{"pane_caller"},
			Panes:   []PaneDef{{ID: "pane_caller"}},
			Layout:  LayoutNode{"type": "pane", "paneId": "pane_caller"},
		}, baseTime.Add(time.Hour))
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Target Desk"}, baseTime)
		api, spawnCalls := newMCPSpawnWorkspaceAPI(t, store)

		metadata := requireMCPSpawnSuccess(t, api, "pane_caller", map[string]any{
			"cmd":       "bash",
			"workspace": "  target desk  ",
		})
		if metadata.Workspace != "Target Desk" {
			t.Fatalf("workspace metadata = %q, want canonical Target Desk", metadata.Workspace)
		}
		if *spawnCalls != 1 {
			t.Fatalf("spawn calls = %d, want 1", *spawnCalls)
		}
		requireMCPPaneInWorkspace(t, store, "Target Desk", metadata.ID)
		caller, _ := store.Get("Caller Desk")
		if mcpWorkspaceHasPaneID(caller.PaneIds, metadata.ID) {
			t.Fatalf("explicit pane %q also attached to caller workspace", metadata.ID)
		}
	})

	t.Run("omitted workspace inherits caller workspace", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{
			Name:    "Caller Desk",
			PaneIds: []string{"pane_caller"},
			Panes:   []PaneDef{{ID: "pane_caller"}},
			Layout:  LayoutNode{"type": "pane", "paneId": "pane_caller"},
		}, baseTime)
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Fresher Desk"}, baseTime.Add(time.Hour))
		api, _ := newMCPSpawnWorkspaceAPI(t, store)

		metadata := requireMCPSpawnSuccess(t, api, "pane_caller", map[string]any{"cmd": "bash"})
		if metadata.Workspace != "Caller Desk" {
			t.Fatalf("workspace metadata = %q, want Caller Desk", metadata.Workspace)
		}
		requireMCPPaneInWorkspace(t, store, "Caller Desk", metadata.ID)
	})

	t.Run("absent caller falls back deterministically to freshest visible workspace", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "_current"}, baseTime.Add(24*time.Hour))
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Older"}, baseTime)
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Beta"}, baseTime.Add(time.Hour))
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Alpha"}, baseTime.Add(time.Hour))
		api, _ := newMCPSpawnWorkspaceAPI(t, store)

		metadata := requireMCPSpawnSuccess(t, api, "", map[string]any{"cmd": "bash"})
		if metadata.Workspace != "Alpha" {
			t.Fatalf("workspace metadata = %q, want deterministic tie winner Alpha", metadata.Workspace)
		}
		requireMCPPaneInWorkspace(t, store, "Alpha", metadata.ID)
	})

	t.Run("unmapped caller falls back to freshest visible workspace", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Older"}, baseTime)
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Freshest"}, baseTime.Add(time.Hour))
		api, _ := newMCPSpawnWorkspaceAPI(t, store)

		metadata := requireMCPSpawnSuccess(t, api, "pane_unmapped", map[string]any{"cmd": "bash"})
		if metadata.Workspace != "Freshest" {
			t.Fatalf("workspace metadata = %q, want Freshest", metadata.Workspace)
		}
		requireMCPPaneInWorkspace(t, store, "Freshest", metadata.ID)
	})
}

func TestMCPSpawnPaneRejectsInvisibleWorkspaceBeforeSpawn(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")

	t.Run("workspace store unavailable", func(t *testing.T) {
		api, spawnCalls := newMCPSpawnWorkspaceAPI(t, nil)
		text, isErr := callMCPSpawnPane(t, api, "", map[string]any{"cmd": "bash"})
		if !isErr || !strings.Contains(text, "workspace store not initialized") {
			t.Fatalf("result = %q error=%v", text, isErr)
		}
		if *spawnCalls != 0 {
			t.Fatalf("spawn calls = %d, want 0", *spawnCalls)
		}
	})

	t.Run("hidden workspace explicitly requested", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "_current"}, time.Now().UTC())
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "Visible"}, time.Now().UTC())
		api, spawnCalls := newMCPSpawnWorkspaceAPI(t, store)
		text, isErr := callMCPSpawnPane(t, api, "", map[string]any{"cmd": "bash", "workspace": " _CURRENT "})
		if !isErr || !strings.Contains(text, "_current is hidden") {
			t.Fatalf("result = %q error=%v", text, isErr)
		}
		if *spawnCalls != 0 {
			t.Fatalf("spawn calls = %d, want 0", *spawnCalls)
		}
	})

	t.Run("no visible workspace exists", func(t *testing.T) {
		store, err := NewWorkspaceStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		putMCPSpawnWorkspace(t, store, &Workspace{Name: "_current"}, time.Now().UTC())
		api, spawnCalls := newMCPSpawnWorkspaceAPI(t, store)
		text, isErr := callMCPSpawnPane(t, api, "", map[string]any{"cmd": "bash"})
		if !isErr || !strings.Contains(text, "no visible named workspace") {
			t.Fatalf("result = %q error=%v", text, isErr)
		}
		if *spawnCalls != 0 {
			t.Fatalf("spawn calls = %d, want 0", *spawnCalls)
		}
	})
}

func TestMCPSpawnPaneToolDocumentsWorkspaceDefaultAndResponse(t *testing.T) {
	for _, tool := range mcpTools() {
		if tool.Name != "spawn_pane" {
			continue
		}
		if !strings.Contains(tool.Description, "{id, workspace}") || !strings.Contains(tool.Description, "calling pane's named workspace") {
			t.Fatalf("spawn_pane description does not document response/default: %q", tool.Description)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		workspaceDescription := schema.Properties["workspace"].Description
		if !strings.Contains(workspaceDescription, "case-insensitively") || !strings.Contains(workspaceDescription, "canonical stored workspace") {
			t.Fatalf("workspace schema description = %q", workspaceDescription)
		}
		return
	}
	t.Fatal("spawn_pane tool missing")
}

func TestMCPSpawnPaneWorkspaceOriginAndLead(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	for _, tt := range []struct {
		name, explicit, layout, workspace, source, lead, leadLayout, leadWorkspace, want string
	}{
		{name: "unsaved user caller origin beats active", source: paneOriginUser, workspace: "Caller", want: "Caller"},
		{name: "layout beats historical origin", layout: "Caller", workspace: "Active", want: "Caller"},
		{name: "explicit beats layout and origin", explicit: "Active", layout: "Caller", workspace: "Caller", want: "Active"},
		{name: "explicit beats unsaved caller origin", explicit: "Active", workspace: "Caller", want: "Active"},
		{name: "agent origin beats lead", source: paneOriginAgent, workspace: "Caller", lead: "lead", leadLayout: "Active", want: "Caller"},
		{name: "lead layout", source: paneOriginAgent, lead: "lead", leadLayout: "Caller", leadWorkspace: "Active", want: "Caller"},
		{name: "lead origin without layout", source: paneOriginAgent, lead: "lead", leadWorkspace: " caller ", want: "Caller"},
		{name: "stale origin uses lead", source: paneOriginAgent, workspace: "Deleted", lead: "lead", leadWorkspace: "Caller", want: "Caller"},
		{name: "hidden origin uses lead", source: paneOriginAgent, workspace: "_current", lead: "lead", leadWorkspace: "Caller", want: "Caller"},
		{name: "user does not inherit lead", source: paneOriginUser, lead: "lead", leadWorkspace: "Caller", want: "Active"},
		{name: "missing lead falls back", source: paneOriginAgent, lead: "missing", want: "Active"},
		{name: "lead cycle falls back", source: paneOriginAgent, lead: "caller", want: "Active"},
		{name: "no attribution falls back", want: "Active"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewWorkspaceStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Caller", "Active", "_current"} {
				w := &Workspace{Name: name}
				for id, layout := range map[string]string{"caller": tt.layout, "lead": tt.leadLayout} {
					if layout == name {
						w.PaneIds = append(w.PaneIds, id)
						w.Layout = LayoutNode{"type": "pane", "paneId": id}
					}
				}
				at := time.Unix(1, 0)
				if name == "Active" {
					at = at.Add(time.Hour)
				}
				putMCPSpawnWorkspace(t, store, w, at)
			}
			api, _ := newMCPSpawnWorkspaceAPI(t, store)
			caller := transactionTestPane("caller")
			caller.Origin = &PaneOrigin{Source: tt.source, Workspace: tt.workspace, LeadPaneID: tt.lead}
			lead := transactionTestPane("lead")
			lead.Origin = &PaneOrigin{Source: paneOriginUser, Workspace: tt.leadWorkspace}
			api.mgr.panes[caller.ID], api.mgr.panes[lead.ID] = caller, lead
			metadata := requireMCPSpawnSuccess(t, api, caller.ID, map[string]any{"cmd": "bash", "workspace": tt.explicit})
			if metadata.Workspace != tt.want {
				t.Fatalf("workspace = %q, want %q", metadata.Workspace, tt.want)
			}
			requireMCPPaneInWorkspace(t, store, tt.want, metadata.ID)
			spawned, _ := api.mgr.Get(metadata.ID)
			if o := spawned.Snapshot().Origin; o == nil || o.Workspace != tt.want {
				t.Fatalf("live origin = %+v", o)
			}
			for _, w := range store.List() {
				if w.Name != tt.want && (mcpWorkspaceHasPaneID(w.PaneIds, metadata.ID) || mcpLayoutHasPane(w.Layout, metadata.ID)) {
					t.Fatalf("spawn also attached to %q", w.Name)
				}
			}
		})
	}
}
