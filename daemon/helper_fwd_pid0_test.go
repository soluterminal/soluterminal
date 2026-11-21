package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// @secure ruling 8.6a.1: a helper caller is a verified pane caller for its
// attested pane, exactly that. Its PID is 0 (a Linux pid means nothing on the
// host), so every gate that reads the caller's PID must fail closed at 0; and
// every agent-authority gate must treat it as it treats a traced local pane.
// One test per gate found by the PID-0 audit (docs/design/one-solud/
// w3-hooks-plan.md).

const (
	pid0Pane  = "pane_00000000000000aa"
	pid0Other = "pane_00000000000000bb"
)

// realCallerClassification turns off the package tests' stand-in person
// (identifyCallerHook) so identifyCaller classifies as in production, where
// the hook is nil.
func realCallerClassification(t *testing.T) {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = nil
	t.Cleanup(func() { identifyCallerHook = prev })
}

func helperCallerID() callerIdentity {
	return callerIdentity{Kind: callerPane, PaneID: pid0Pane, Helper: "Ubuntu"}
}

// localPaneID is the same pane as a traced local caller (a real pid).
func localPaneID() callerIdentity {
	return callerIdentity{Kind: callerPane, PaneID: pid0Pane, PID: 4242}
}

// helperRequest is a request as helperFwdHost.dispatch builds it.
func helperRequest(method, path, body string) *http.Request {
	ctx := context.WithValue(context.Background(), helperFwdCallerKey{}, helperFwdCaller{PaneID: pid0Pane, AgentID: "0123456789abcdef0123456789abcdef", Distro: "Ubuntu"})
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header = http.Header{"Content-Type": {"application/json"}}
	r.Host = "127.0.0.1"
	r.RemoteAddr = helperFwdRemoteAddr
	return r
}

func TestPID0ClassifiedAsTheAttestedPaneOnly(t *testing.T) {
	realCallerClassification(t)
	r := helperRequest("POST", "/mcp", "{}")
	// Even a relay-marked request: the fwd key is read first, and it never
	// becomes human or relay.
	r = r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	id := (&apiServer{}).identifyCaller(r)
	if id.Kind != callerPane || id.PaneID != pid0Pane || id.Relay || id.Forwarded || id.PID != 0 || id.Helper != "Ubuntu" || id.Device != "" {
		t.Fatalf("identified %+v", id)
	}
}

// caller_authority.go refuseCaller: the "browser started from a pane" message
// needs PID > 0.
func TestPID0RefuseCallerMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	(&apiServer{}).refuseCaller(rec, helperCallerID(), "do something", false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "browser") {
		t.Fatalf("pid-0 caller treated as a browser: %q", rec.Body.String())
	}
}

// mcp.go mcpCallerForIdentity: the verified pane, never the header's claim,
// never "outside the panes" through PID > 0.
func TestPID0MCPCallerForIdentity(t *testing.T) {
	a := &apiServer{}
	if got := a.mcpCallerForIdentity(pid0Other, helperCallerID()); got != pid0Pane {
		t.Fatalf("caller %q, want %q", got, pid0Pane)
	}
	if got, want := a.mcpCallerForIdentity("", helperCallerID()), a.mcpCallerForIdentity("", localPaneID()); got != want {
		t.Fatalf("helper %q vs local pane %q", got, want)
	}
	// With no pane, PID 0 is never "a traced program outside the panes"
	// that owns none; it falls to the claimed (unverified) prefix.
	noPane := callerIdentity{Kind: callerUnverified}
	if got := a.mcpCallerForIdentity(pid0Other, noPane); got != mcpClaimedCallerPrefix+pid0Other {
		t.Fatalf("pid 0 without a pane: %q", got)
	}
}

// mcp_caller_retrace.go mcpTraceMissed: PID == 0 retraces only unverified
// callers, never a pane.
func TestPID0MCPTraceMissed(t *testing.T) {
	if mcpTraceMissed(helperCallerID()) {
		t.Fatal("a helper pane caller is retraced as unconfirmed")
	}
}

// mcp_socket.go mcpPeerUnderDaemon: pid 0 is never under the daemon.
func TestPID0MCPPeerUnderDaemon(t *testing.T) {
	if mcpPeerUnderDaemon(0) {
		t.Fatal("pid 0 is under the daemon")
	}
}

// trusted_devices.go deviceManagementProven: needs a device or a human with
// PID > 0; a helper pane is neither, with or without require.
func TestPID0DeviceManagementProven(t *testing.T) {
	r := helperRequest("POST", "/api/devices/approve", "{}")
	for _, require := range []bool{false, true} {
		if deviceManagementProven(r, helperCallerID(), require) {
			t.Fatalf("require=%t: a helper pane proved device management", require)
		}
	}
}

// human_proof.go (set mode: PID == 0 check; acknowledge: PID > 0) and
// trusted_devices.go approve-remote-device (PID > 0): a helper pane is
// refused by the human check before any of them, and nothing changes.
func TestPID0HumanOnlyHandlersRefuse(t *testing.T) {
	realCallerClassification(t)
	a := humanProofTestServer(t, humanProofRequire)
	for name, h := range map[string]func(http.ResponseWriter, *http.Request){
		"put human proof":       a.handlePutHumanProof,
		"acknowledge":           a.handleAcknowledgeHumanProof,
		"approve remote device": a.handleApproveRemoteDevice,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, helperRequest("POST", "/x", `{"mode":"off","deviceId":"d"}`))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	a.humanProof.mu.Lock()
	mode := a.humanProof.configuredMode()
	a.humanProof.mu.Unlock()
	if mode != humanProofRequire {
		t.Fatalf("a helper pane changed the device check to %q", mode)
	}
	rec := httptest.NewRecorder()
	if a.requireHuman(rec, helperRequest("POST", "/x", "{}"), "act as the person") || rec.Code != http.StatusForbidden {
		t.Fatalf("requireHuman let a helper pane through (%d)", rec.Code)
	}
}

// prompt_answer_authority.go decidePromptAnswer, which approve, answer_prompt,
// auto-mode and typed keys at a prompt go through: exactly the verdict a
// traced local pane gets, with Agent permissions on and off.
func TestPID0PromptAnswerMatchesALocalPane(t *testing.T) {
	own := &Pane{ID: pid0Pane}
	other := &Pane{ID: pid0Other}
	for _, granted := range []bool{false, true} {
		a := &apiServer{agentAuthority: newAgentAuthority(t.TempDir())}
		if granted {
			a.agentAuthority = grantedAgentAuthority(t)
		}
		for _, target := range []*Pane{own, other} {
			h := a.decidePromptAnswer(mcpCallMeta{id: helperCallerID(), transport: "tcp"}, false, target)
			l := a.decidePromptAnswer(mcpCallMeta{id: localPaneID(), transport: "tcp"}, false, target)
			if h != l {
				t.Fatalf("granted=%t target=%s: helper %+v, local pane %+v", granted, target.ID, h, l)
			}
		}
		if v := a.decidePromptAnswer(mcpCallMeta{id: helperCallerID(), transport: "tcp"}, false, own); v.allowed {
			t.Fatal("a helper pane answered its own prompt")
		}
	}
}

// caller_authority.go promptInOwnPane: the helper pane's own prompts are its
// own, as for a local pane.
func TestPID0PromptInOwnPane(t *testing.T) {
	if !promptInOwnPane(helperCallerID(), &Pane{ID: pid0Pane}) || promptInOwnPane(helperCallerID(), &Pane{ID: pid0Other}) {
		t.Fatal("own-pane check differs for a helper pane")
	}
}

// requireHumanOrDelegated (autonomy and Aria resume): the same answer as a
// traced local pane, with Agent permissions on and off.
func TestPID0DelegatedMatchesALocalPane(t *testing.T) {
	realCallerClassification(t)
	for _, granted := range []bool{false, true} {
		a := &apiServer{agentAuthority: newAgentAuthority(t.TempDir())}
		if granted {
			a.agentAuthority = grantedAgentAuthority(t)
		}
		helperRec := httptest.NewRecorder()
		gotHelper := a.requireHumanOrDelegated(helperRec, helperRequest("POST", "/x", "{}"), "resume")

		identifyCallerHook = func(*http.Request) callerIdentity { return localPaneID() }
		localRec := httptest.NewRecorder()
		gotLocal := a.requireHumanOrDelegated(localRec, httptest.NewRequest("POST", "/x", nil), "resume")
		identifyCallerHook = nil

		if gotHelper != gotLocal || helperRec.Code != localRec.Code {
			t.Fatalf("granted=%t: helper %t/%d, local pane %t/%d", granted, gotHelper, helperRec.Code, gotLocal, localRec.Code)
		}
	}
}

// The real MCP handler admits a forwarded request by its attestation (no
// token, no loopback) and answers as the attested pane; a request without
// the fwd key and without a token is still refused.
func TestHelperFwdThroughTheRealMCPHandler(t *testing.T) {
	realCallerClassification(t)
	a := &apiServer{metrics: NewMetrics(), agentAuthority: newAgentAuthority(t.TempDir()), mgr: NewManager(t.TempDir())}
	rec := httptest.NewRecorder()
	a.mcp(rec, helperRequest("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tools"`) {
		t.Fatalf("forwarded tools/list: %d %s", rec.Code, rec.Body.String())
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.RemoteAddr = helperFwdRemoteAddr
	rec = httptest.NewRecorder()
	a.mcp(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unforwarded, untokened request: %d", rec.Code)
	}
}
