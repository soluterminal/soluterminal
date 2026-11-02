package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMCPUntracedCallerChangesNeedOK(t *testing.T) {
	shortenPaneConfirmWait(t, time.Millisecond)
	for _, target := range []string{"lead", "worker"} {
		for _, tool := range []string{"kill_pane", "rename_pane", "set_pane_info", "set_pane_info_kind"} {
			t.Run(target+"/"+tool, func(t *testing.T) {
				a := traceTestServer(t)
				// Even standing Agent permissions cannot turn a claim into ownership.
				if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"pane_id": target}
				name := tool
				switch tool {
				case "rename_pane":
					args["title"] = "Changed"
				case "set_pane_info":
					args["working_on"] = "Changed"
				case "set_pane_info_kind":
					name = "set_pane_info"
					args["kind"], args["lead_role"] = "lead", "review"
				}
				if text, failed := mcpAs(t, a, nil, "lead", name, args); !failed {
					t.Fatalf("claim changed %s without a request: %q", target, text)
				}
				args["user_requested"] = true
				if text, _ := mcpAs(t, a, nil, "lead", name, args); !strings.Contains(text, "waiting for your OK in the app") {
					t.Fatalf("claim did not need OK: %q", text)
				}
				view := onlyPendingConfirm(t, a)
				if view.CallerPaneID != "" || !strings.Contains(view.Caller, "unverified") || view.PaneID != target {
					t.Fatalf("confirmation = %+v", view)
				}
				c := splitMCPCaller(mcpClaimedCallerPrefix + "lead")
				p, ok := a.mgr.Get(target)
				if !ok {
					t.Fatal("target closed before Allow")
				}
				if got := a.paneAgentAuthority(p.Snapshot().Origin, target, c.ID, true); got != paneAgentNeedsOK {
					t.Fatalf("claimed authority = %v, want NeedsOK", got)
				}
				if got, _ := a.paneTeamAgentAuthority(p.Snapshot().Origin, target, c.ID, paneTeamLead, true); got != paneAgentNeedsOK {
					t.Fatalf("claimed team authority = %v, want NeedsOK", got)
				}
				// The actual operation runs only after the person allows it.
				if _, ok := a.decidePaneConfirm(view.ID, true); !ok {
					t.Fatal("missing request")
				}
				if tool == "kill_pane" {
					if _, ok := a.mgr.Get(target); ok {
						t.Fatal("pane still open after Allow")
					}
				} else {
					snap := p.Snapshot()
					switch tool {
					case "rename_pane":
						if snap.Title != "Changed" {
							t.Fatalf("title = %q", snap.Title)
						}
					case "set_pane_info":
						if snap.Identity == nil || snap.Identity.WorkingOn != "Changed" {
							t.Fatalf("identity = %+v", snap.Identity)
						}
					case "set_pane_info_kind":
						if snap.Origin.TeamKind != paneTeamLead {
							t.Fatalf("origin = %+v", snap.Origin)
						}
					}
				}
			})
		}
	}
}

func TestClaimedMessageLabels(t *testing.T) {
	a := traceTestServer(t)
	a.mgr.panes["lead"].Agent = agentClaude
	for _, tc := range []struct{ caller, target, want string }{
		{"lead", "worker", "#1 Claude"},
		{mcpClaimedCallerPrefix + "lead", "worker", "#1 Claude (unverified)"},
		{mcpClaimedCallerPrefix + "lead", "lead", "#1 Claude (unverified)"},
		{mcpClaimedCallerPrefix + "missing", "worker", "an unverified agent"},
	} {
		if got := a.paneMessageSender(tc.caller, tc.target); got != tc.want {
			t.Fatalf("sender %q = %q, want %q", tc.caller, got, tc.want)
		}
		if got := a.labelPaneMessage("Please review", tc.caller, tc.target); got != "[from "+tc.want+"] Please review" {
			t.Fatalf("label = %q", got)
		}
	}
	if got := a.paneConfirmCaller(paneCaller{Claimed: "missing"}); got != "An unverified agent" {
		t.Fatalf("unknown claim = %q", got)
	}
}

func TestMCPClaimedMessageCapUsesCallerIdentity(t *testing.T) {
	a := traceTestServer(t)
	p, frames, screen := newDraftGuardPane(t, "target")
	screen.Store(deliveryCodexScreen("the person's draft"))
	a.mgr.panes[p.ID] = p
	a.mgr.panes["lead"].Agent = agentClaude
	send := func(traced *callerIdentity, claim, body string) string {
		t.Helper()
		text, failed := mcpAs(t, a, traced, claim, "send_input", map[string]any{"pane_id": p.ID, "text": body + "\n"})
		if !failed {
			t.Fatalf("expected held or refused: %q", text)
		}
		return text
	}
	for i := 0; i < heldPerSenderMax; i++ {
		if text := send(nil, "lead", fmt.Sprintf("message %d", i)); !strings.Contains(text, "held") {
			t.Fatal(text)
		}
	}
	held := p.Snapshot().HeldMessages
	if len(held) != heldPerSenderMax || held[0].Sender != "#1 Claude (unverified)" {
		t.Fatalf("held = %+v", held)
	}
	// Changing the pane's display name does not reset its claimed share.
	a.mgr.panes["lead"].Identity = &PaneIdentity{Handle: "renamed"}
	if text := send(nil, "lead", "one too many"); !strings.Contains(text, "NOT sent and NOT kept") {
		t.Fatal(text)
	}
	// A resend after relabeling still refers to the same held message.
	if text := send(nil, "lead", "message 0"); !strings.Contains(text, "held") {
		t.Fatal(text)
	}
	// Verified callers have their own share, independent of matching claims.
	traced := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}
	if text := send(traced, "lead", "verified message"); !strings.Contains(text, "held") {
		t.Fatal(text)
	}
	// Every claim, known or unknown, and anonymous MCP callers share the
	// same unverified quota. Verified senders retain separate allowances.
	for _, claim := range []string{"worker", "missing-a", "missing-b", ""} {
		if text := send(nil, claim, "another unverified message"); !strings.Contains(text, "NOT sent and NOT kept") {
			t.Fatalf("claim %q: %s", claim, text)
		}
	}
	p.mu.Lock()
	if p.heldDeliveries[0].senderKey != (paneCaller{Claimed: "lead"}).messageKey() {
		t.Errorf("held key = %q", p.heldDeliveries[0].senderKey)
	}
	p.mu.Unlock()
	expectNoFrame(t, frames, "claims remain held while the person types")
}

func TestMCPClaimedAskWaitFollowsItsHeldMessage(t *testing.T) {
	a := traceTestServer(t)
	p, frames, screen := newDraftGuardPane(t, "target")
	screen.Store(deliveryCodexScreen("the person's draft"))
	a.mgr.panes[p.ID] = p
	a.mgr.panes["lead"].Agent = agentClaude
	text, failed := mcpAs(t, a, nil, "lead", "ask_pane", map[string]any{"pane_id": p.ID, "text": "Please review", "timeout_seconds": 0.001})
	if failed || !strings.Contains(text, `"delivery":"held"`) {
		t.Fatalf("ask = %q", text)
	}
	c := paneCaller{Claimed: "lead"}
	base, ok := a.queuedAsks.get(p.ID, c.bucket())
	if !ok || base.HeldID == "" {
		t.Fatalf("baseline = %+v, found %v", base, ok)
	}
	if _, ok := a.queuedAsks.get(p.ID, "lead"); ok {
		t.Fatal("claim shared a verified caller's baseline")
	}
	if _, ok := a.queuedAsks.get(p.ID, ""); ok {
		t.Fatal("claim shared an anonymous baseline")
	}
	// A person's discard is reported by the same claim's wait_for_pane.
	h := p.heldDeliveryByID(base.HeldID)
	if h == nil || !p.resolveHeld(h, "discarded", nil) {
		t.Fatal("held message missing")
	}
	text, failed = mcpAs(t, a, nil, "lead", "wait_for_pane", map[string]any{"pane_id": p.ID, "timeout_seconds": 0.001})
	if failed || !strings.Contains(text, `"status":"unsent"`) || !strings.Contains(text, "discarded") {
		t.Fatalf("wait = %q", text)
	}
	expectNoFrame(t, frames, "ask did not type into the person's draft")
}

func TestMCPUnverifiedCallersShareConfirmationLimit(t *testing.T) {
	shortenPaneConfirmWait(t, time.Millisecond)
	a := traceTestServer(t)
	firstArgs := map[string]any{"pane_id": "worker", "title": "One", "user_requested": true}
	if text, _ := mcpAs(t, a, nil, "lead", "rename_pane", firstArgs); !strings.Contains(text, "waiting for your OK") {
		t.Fatal(text)
	}
	first := onlyPendingConfirm(t, a)
	// Environment-only claims, untraced headers, and anonymous calls share
	// one quota across all tools, while keeping distinct request identities.
	env := &callerIdentity{Kind: callerPane, PaneID: "worker", PID: 7, FromEnv: true}
	if text, _ := mcpAs(t, a, env, "rogue", "set_pane_info", map[string]any{"pane_id": "lead", "working_on": "Two", "user_requested": true}); !strings.Contains(text, "waiting for your OK") {
		t.Fatal(text)
	}
	if text, _ := mcpAs(t, a, nil, "", "kill_pane", map[string]any{"pane_id": "social", "user_requested": true}); !strings.Contains(text, "waiting for your OK") {
		t.Fatal(text)
	}
	for _, header := range []string{"lead", "worker", "missing"} {
		text, failed := mcpAs(t, a, nil, header, "kill_pane", map[string]any{"pane_id": "worker", "user_requested": true})
		if !failed || !strings.Contains(text, "3 of your requests") {
			t.Fatalf("claim %q = %q", header, text)
		}
	}
	if n := len(a.pendingPaneConfirms()); n != paneConfirmMaxPerCaller {
		t.Fatalf("open requests = %d", n)
	}
	if text, _ := mcpAs(t, a, nil, "lead", "rename_pane", firstArgs); !strings.Contains(text, "waiting for your OK") {
		t.Fatalf("resend = %q", text)
	}
	if n := len(a.pendingPaneConfirms()); n != paneConfirmMaxPerCaller {
		t.Fatalf("resend stacked %d requests", n)
	}
	// A real pane's allowance cannot be consumed by claims naming it.
	traced := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}
	if text, _ := mcpAs(t, a, traced, "rogue", "rename_pane", map[string]any{"pane_id": "social", "title": "Verified", "user_requested": true}); !strings.Contains(text, "waiting for your OK") {
		t.Fatalf("verified caller = %q", text)
	}
	if n := len(a.pendingPaneConfirms()); n != paneConfirmMaxPerCaller+1 {
		t.Fatalf("verified share = %d requests", n)
	}
	if _, ok := a.decidePaneConfirm(first.ID, false); !ok {
		t.Fatal("request missing")
	}
	if text, _ := mcpAs(t, a, nil, "missing", "kill_pane", map[string]any{"pane_id": "worker", "user_requested": true}); !strings.Contains(text, "waiting for your OK") {
		t.Fatalf("freed unverified slot = %q", text)
	}
}
