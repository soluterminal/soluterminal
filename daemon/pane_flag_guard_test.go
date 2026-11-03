package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Flags are the person's bookmarks: an agent in a pane (any full-access
// caller) can neither set nor clear one, its own or anyone's, and is pointed
// at post_user_ask instead. The tool is not advertised to it either.
func TestAgentFlagOwnership(t *testing.T) {
	p := testPaneWithTail(t, "target", "worker", "$ ")
	p.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "starter", LeadPaneID: "lead"}
	a := &apiServer{mgr: testManagerWithPane(p)}
	call := func(caller string, flagged bool) (string, bool) {
		raw, _ := json.Marshal(toolCallParams{Name: "set_pane_flag", Arguments: map[string]any{"pane_id": p.ID, "flagged": flagged, "memo": "note"}})
		return mcpResultText(t, a.mcpCallTool(raw, nil, caller))
	}
	for _, owner := range []string{"user", "", "agent:target", "agent"} {
		p.SetFlagAs(true, "bookmark", owner)
		for _, caller := range []string{"starter", "lead", "target", "stranger", ""} {
			for _, flagged := range []bool{true, false} {
				if msg, bad := call(caller, flagged); !bad || !strings.Contains(msg, "post_user_ask") {
					t.Fatalf("%q changed a %q flag: %s", caller, owner, msg)
				}
				if snap := p.Snapshot(); !snap.Flagged || snap.FlagMemo != "bookmark" || snap.FlagOwner != owner {
					t.Fatal("flag changed")
				}
			}
		}
	}
	p.SetFlagAs(false, "", "")
	if msg, bad := call("target", true); !bad || p.Snapshot().Flagged {
		t.Fatalf("agent flagged its own pane: %s", msg)
	}
	resp := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/list"}, nil, "target")
	listed, _ := json.Marshal(resp.Result)
	if strings.Contains(string(listed), `"set_pane_flag"`) {
		t.Fatal("set_pane_flag is still advertised to agents in panes")
	}
}

func TestAgentFlagOwnerRemapsWithRestoredPaneIDs(t *testing.T) {
	p := testPaneWithTail(t, "target", "worker", "$ ")
	p.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "old", LeadPaneID: "old"}
	p.SetFlagAs(true, "owned", "agent:old")
	if !p.remapOrigin(map[string]string{"old": "new"}) {
		t.Fatal("owner/starter not remapped")
	}
	a := &apiServer{mgr: testManagerWithPane(p)}
	if msg, bad := mcpResultText(t, a.mcpToolSetPaneFlag(map[string]any{"pane_id": p.ID, "flagged": false}, "agent", nil, "new")); bad {
		t.Fatal("restored owner lost its flag:", msg)
	}
	p.SetFlag(true, "person")
	p.remapOrigin(map[string]string{"user": "new"})
	if p.Snapshot().FlagOwner != "user" {
		t.Fatal("human provenance remapped")
	}
}
