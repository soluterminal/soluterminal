package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// retraceFixture stubs the two lookups identifyMCPCaller makes: the trace
// (traces answers in order, the last one repeating) and whether the caller's
// socket is local to this host, held by owner.
func retraceFixture(t *testing.T, local bool, owner int, traces ...callerIdentity) *int {
	t.Helper()
	prevDelay, prevSocket, prevHook := mcpCallerRetraceDelay, mcpPeerSocket, identifyCallerHook
	t.Cleanup(func() {
		mcpCallerRetraceDelay, mcpPeerSocket, identifyCallerHook = prevDelay, prevSocket, prevHook
	})
	mcpCallerRetraceDelay = 0
	mcpPeerSocket = func(*http.Request) (bool, int) { return local, owner }
	calls := 0
	identifyCallerHook = func(*http.Request) callerIdentity {
		id := traces[min(calls, len(traces)-1)]
		calls++
		return id
	}
	return &calls
}

// mcpRaw is mcpAs without its own trace stub.
func mcpRaw(t *testing.T, a *apiServer, header, tool string, args map[string]any) (string, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-token")
	if header != "" {
		req.Header.Set("X-Soluterminal-Pane", header)
	}
	rec := httptest.NewRecorder()
	a.mcp(rec, req)
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Result == nil {
		t.Fatalf("%s: %d %s", tool, rec.Code, rec.Body.String())
	}
	return mcpResultText(t, resp.Result)
}

var missedTrace = callerIdentity{Kind: callerUnverified}

func spawnArgs() map[string]any {
	return map[string]any{"cmd": "bash", "title": "Worker", "wait_ready": false}
}

func TestMCPCallerTraceMissRetriedOnce(t *testing.T) {
	a, spawned := envClaimSpawnServer(t)
	calls := retraceFixture(t, true, os.Getpid(), missedTrace, callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7})
	if text, failed := mcpRaw(t, a, "lead", "spawn_pane", spawnArgs()); failed {
		t.Fatalf("spawn_pane after a retried trace: %q", text)
	}
	if *calls != 2 {
		t.Fatalf("traced %d times, want 2", *calls)
	}
	if len(*spawned) != 1 {
		t.Fatalf("spawned %d panes", len(*spawned))
	}
	if o := (*spawned)[0].Origin; o == nil || o.Source != paneOriginAgent || o.ByPaneID != "lead" {
		t.Fatalf("origin = %+v, want started by lead", o)
	}
}

func TestMCPUnconfirmedLocalCallerGetsRetryableError(t *testing.T) {
	a, spawned := envClaimSpawnServer(t)
	calls := retraceFixture(t, true, os.Getpid(), missedTrace)
	before := len(a.mgr.List())
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"spawn_pane", spawnArgs()},
		{"spawn_pane", map[string]any{"cmd": "auto", "prompt": "fix it"}},
		{"create_schedule", map[string]any{"prompt": "check", "every": "1h"}},
		{"set_pane_info", map[string]any{"working_on": "Changed"}},
	} {
		*calls = 0
		text, failed := mcpRaw(t, a, "lead", tc.tool, tc.args)
		if !failed || text != mcpUnconfirmedCallerNotice {
			t.Fatalf("%s %v from an unconfirmed local caller: %q", tc.tool, tc.args, text)
		}
		if *calls != 2 {
			t.Fatalf("%s traced %d times, want 2", tc.tool, *calls)
		}
	}
	if len(*spawned) != 0 || len(a.mgr.List()) != before {
		t.Fatalf("panes created: spawned=%d, panes %d -> %d", len(*spawned), before, len(a.mgr.List()))
	}
	if snap := a.mgr.panes["lead"].Snapshot(); snap.Identity != nil && snap.Identity.WorkingOn == "Changed" {
		t.Fatal("set_pane_info changed the claimed pane")
	}
	// Calls that name their target or only read keep the claim treatment.
	if text, _ := mcpRaw(t, a, "lead", "set_pane_info", map[string]any{"pane_id": "lead", "working_on": "Changed"}); text == mcpUnconfirmedCallerNotice {
		t.Fatal("set_pane_info with pane_id refused as unconfirmed")
	}
	if text, failed := mcpRaw(t, a, "lead", "list_panes", nil); failed {
		t.Fatalf("list_panes: %q", text)
	}
}

// Where the caller's socket isn't on this host (macOS, a Windows program
// reaching a WSL daemon), its holder can't be read or is a relay (WSL's NAT
// localhost relay runs as root), there is no claimed pane, or the trace found
// a process, a miss is today's anonymous or claimed caller: a retry could
// never do better.
func TestMCPUntraceableCallerKeepsClaimedTreatment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		local     bool
		owner     int
		relay     bool
		header    string
		trace     callerIdentity
		wantCalls int
	}{
		{"not a local socket", false, 0, false, "lead", missedTrace, 1},
		{"unreadable holder", true, 0, false, "lead", missedTrace, 2},
		{"relay holder", true, 1, true, "lead", missedTrace, 2},
		{"no header", true, os.Getpid(), false, "", missedTrace, 1},
		{"traced to another program", true, os.Getpid(), false, "lead", callerIdentity{Kind: callerUnverified, PID: 4242}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spawned := envClaimSpawnServer(t)
			calls := retraceFixture(t, tc.local, tc.owner, tc.trace)
			prevRelay := peerIsRelayProxy
			peerIsRelayProxy = func(pid int) bool { return tc.relay && pid == tc.owner }
			t.Cleanup(func() { peerIsRelayProxy = prevRelay })
			if text, failed := mcpRaw(t, a, tc.header, "spawn_pane", spawnArgs()); failed {
				t.Fatalf("spawn_pane: %q", text)
			}
			if *calls != tc.wantCalls {
				t.Fatalf("traced %d times, want %d", *calls, tc.wantCalls)
			}
			if len(*spawned) != 1 {
				t.Fatalf("spawned %d panes", len(*spawned))
			}
			if o := (*spawned)[0].Origin; o == nil || o.Source != paneOriginAPI || o.ByPaneID != "" {
				t.Fatalf("origin = %+v, want unattributed", o)
			}
		})
	}
}

func TestMCPRefuseUnconfirmedCallerTools(t *testing.T) {
	for _, tc := range []struct {
		params string
		refuse bool
	}{
		{`{"name":"spawn_pane","arguments":{"cmd":"bash"}}`, true},
		{`{"name":"create_schedule","arguments":{}}`, true},
		{`{"name":"set_pane_info","arguments":{"working_on":"x"}}`, true},
		{`{"name":"set_pane_info","arguments":{"pane_id":"  ","working_on":"x"}}`, true},
		{`{"name":"set_pane_info","arguments":{"pane_id":"lead","working_on":"x"}}`, false},
		{`{"name":"ask_pane","arguments":{"pane_id":"lead","prompt":"hi"}}`, false},
		{`{"name":"list_panes"}`, false},
		{`not json`, false},
	} {
		if got := mcpRefuseUnconfirmedCaller(json.RawMessage(tc.params)) != nil; got != tc.refuse {
			t.Fatalf("%s: refused=%t, want %t", tc.params, got, tc.refuse)
		}
	}
}

func TestMCPCallerTraceMissLogIsQuotedAndCapped(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	mcpCallerTraceMisses.Lock()
	mcpCallerTraceMisses.last = map[string]time.Time{}
	mcpCallerTraceMisses.Unlock()
	header := "pane\nforged " + strings.Repeat("x", 100)
	noteMCPCallerTraceMiss(header, true, 0, missedTrace)
	// Another claimed name doesn't buy another line.
	noteMCPCallerTraceMiss(header+"2", true, 0, missedTrace)
	out := buf.String()
	if strings.Count(out, "caller trace missed") != 1 {
		t.Fatalf("want one rate-limited line, got %q", out)
	}
	if strings.Contains(out, "pane\nforged") || strings.Contains(out, strings.Repeat("x", 60)) {
		t.Fatalf("header not quoted and capped: %q", out)
	}
	if !strings.Contains(out, "found=true, owner=0, retry=unverified") {
		t.Fatalf("line = %q", out)
	}
}
