package main

import (
	"strings"
	"testing"
)

// HTTP /mcp caller verdicts, pinned to main's behaviour when the MCP socket
// was added: the socket is a separate transport and must not move any of
// these. Each row is (traced identity, X-Soluterminal-Pane header) → the
// caller pane the tools see.
func TestMCPHTTPCallerVerdictsUnchanged(t *testing.T) {
	prevRelay := peerIsRelayProxy
	peerIsRelayProxy = func(pid int) bool { return pid == 1 }
	t.Cleanup(func() { peerIsRelayProxy = prevRelay })
	a := traceTestServer(t)
	claimed := func(id string) string { return mcpClaimedCallerPrefix + id }
	for _, tc := range []struct {
		name   string
		id     callerIdentity
		header string
		want   string
	}{
		{"traced pane, no header", callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "", "lead"},
		{"traced pane, own header", callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "lead", "lead"},
		{"traced pane names another", callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "worker", "lead"},
		{"env-only pane", callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7, FromEnv: true}, "worker", claimed("lead")},
		{"daemon worker", callerIdentity{Kind: callerWorker, PID: 8}, "lead", ""},
		{"traced other program", callerIdentity{Kind: callerUnverified, PID: 4242}, "lead", ""},
		{"relay proxy", callerIdentity{Kind: callerUnverified, PID: 1}, "lead", claimed("lead")},
		{"untraced with header", callerIdentity{Kind: callerUnverified}, "lead", claimed("lead")},
		{"untraced without header", callerIdentity{Kind: callerUnverified}, "", ""},
		{"unproven person with header", callerIdentity{Kind: callerHuman, Unproven: true}, "lead", claimed("lead")},
		{"relay person", callerIdentity{Kind: callerHuman, Relay: true}, "", ""},
	} {
		if got := a.mcpCallerForIdentity(tc.header, tc.id); got != tc.want {
			t.Errorf("%s: caller %q, want %q", tc.name, got, tc.want)
		}
	}

	// And through the handler: the same verdicts decide what a call may do.
	lead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}
	for _, tc := range []struct {
		name       string
		traced     *callerIdentity
		header     string
		tool       string
		args       map[string]any
		wantFailed bool
		wantText   string
	}{
		{"claim can't default to its pane", nil, "lead", "set_pane_info", map[string]any{"working_on": "x"}, true, "pane_id is required"},
		{"claim can't kill without a request", nil, "lead", "kill_pane", map[string]any{"pane_id": "worker"}, true, ""},
		{"traced lead can't touch another's worker", lead, "lead", "kill_pane", map[string]any{"pane_id": "kid"}, true, ""},
		{"claimed list_panes has no you", nil, "lead", "list_panes", map[string]any{}, false, ""},
	} {
		text, failed := mcpAs(t, a, tc.traced, tc.header, tc.tool, tc.args)
		if failed != tc.wantFailed || !strings.Contains(text, tc.wantText) {
			t.Errorf("%s: failed=%t %q", tc.name, failed, text)
		}
		if tc.tool == "list_panes" && strings.Contains(text, `"you":true`) {
			t.Errorf("%s: %q", tc.name, text)
		}
	}
}
