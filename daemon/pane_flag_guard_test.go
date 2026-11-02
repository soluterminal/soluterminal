package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentFlagOwnership(t *testing.T) {
	p := testPaneWithTail(t, "target", "worker", "$ ")
	p.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "starter", LeadPaneID: "lead"}
	a := &apiServer{mgr: testManagerWithPane(p)}
	call := func(caller string, flagged bool) (string, bool) {
		raw, _ := json.Marshal(toolCallParams{Name: "set_pane_flag", Arguments: map[string]any{"pane_id": p.ID, "flagged": flagged, "memo": "note"}})
		return mcpResultText(t, a.mcpCallTool(raw, nil, caller))
	}
	for _, owner := range []string{"user", ""} {
		p.SetFlagAs(true, "bookmark", owner)
		for _, caller := range []string{"starter", "lead", "target", "stranger", ""} {
			for _, flagged := range []bool{true, false} {
				if msg, bad := call(caller, flagged); !bad || !strings.Contains(msg, "only the person") {
					t.Fatalf("%q changed %q bookmark: %s", caller, owner, msg)
				}
				if p.Snapshot().FlagMemo != "bookmark" {
					t.Fatal("bookmark changed")
				}
			}
		}
	}
	for _, caller := range []string{"starter", "lead", "target"} {
		p.SetFlagAs(false, "", "")
		if msg, bad := call(caller, true); bad {
			t.Fatal(msg)
		}
		if p.Snapshot().FlagOwner != "agent:"+caller {
			t.Fatal("caller provenance missing")
		}
		other := "lead"
		if caller == other {
			other = "starter"
		}
		if msg, bad := call(other, false); !bad {
			t.Fatalf("other agent cleared owned flag: %s", msg)
		}
		if msg, bad := call(caller, false); bad {
			t.Fatal(msg)
		}
	}
	p.SetFlagAs(false, "", "")
	for _, caller := range []string{"stranger", ""} {
		if msg, bad := call(caller, true); !bad {
			t.Fatalf("unrelated caller flagged pane: %s", msg)
		}
	}
	// Legacy agent notes can be adopted by an authorized starter/lead.
	p.SetFlagAs(true, "legacy", "agent")
	if msg, bad := call("lead", true); bad {
		t.Fatal(msg)
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
