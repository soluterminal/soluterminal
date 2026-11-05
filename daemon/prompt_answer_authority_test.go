package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// answerAsPaneForTest calls answer_prompt as an agent in a traced pane
// (pane_answerer, never the target), for tests of what an answer does.
func answerAsPaneForTest(t *testing.T, a *apiServer, args map[string]any) any {
	t.Helper()
	b, err := json.Marshal(toolCallParams{Name: "answer_prompt", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return a.mcpCallTool(b, nil, "pane_answerer")
}

// st10Server has pane A (a real permission menu), pane B (a Claude pane),
// pane C (a Codex pane) and pane S (a plain shell), with the grant as given.
func st10Server(t *testing.T, grant bool) *apiServer {
	t.Helper()
	a := confirmTestServer(t)
	a.gate = &authGate{token: "fixture-token"}
	if grant {
		a.agentAuthority = grantedAgentAuthority(t)
	}
	for id, agent := range map[string]string{"B": agentClaude, "C": agentCodex, "S": ""} {
		p := testPaneWithTail(t, id, id, "")
		p.Agent = agent
		a.mgr.panes[id] = p
	}
	prompt := choicePromptPane(t, "A")
	prompt.Agent = agentClaude
	drainConn(t, prompt)
	a.mgr.panes["A"] = prompt
	return a
}

// authorityLog is a log capture safe to read while pane goroutines log.
type authorityLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *authorityLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *authorityLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureAuthorityLog(t *testing.T) *authorityLog {
	t.Helper()
	buf := &authorityLog{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// refusedAt reports whether a tool result is one of ST-10's refusals (any
// other error is about the prompt itself, after the gate).
func refusedAt(text string) bool {
	for _, r := range []string{promptAnswerGrantOff, promptAnswerUntraced, promptAnswerOwnPrompt, promptAnswerWorker} {
		if strings.Contains(text, r) {
			return true
		}
	}
	return false
}

func answerArgs(target string) map[string]any {
	return map[string]any{"pane_id": target, "option": "1"}
}

func answerViaSocket(t *testing.T, a *apiServer, id callerIdentity, target string) string {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "answer_prompt", "arguments": answerArgs(target)}})
	resp, _ := a.mcpSocketDispatch(msg, id)
	text, _ := mcpResultText(t, resp.Result)
	return text
}

// toolViaHTTP calls an MCP tool over loopback HTTP /mcp as id, with header as
// the pane header.
func toolViaHTTP(t *testing.T, a *apiServer, id callerIdentity, header, tool string, args map[string]any) string {
	t.Helper()
	callAsIdentity(t, id)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40001"
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
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	text, _ := mcpResultText(t, resp.Result)
	return text
}

// Criteria tests 1: a bridge-traced Claude pane B answers pane A's prompt with
// the grant on; with the grant off it is refused.
func TestST10BridgeTracedPaneAnswersOnlyWithTheGrant(t *testing.T) {
	traced := callerIdentity{Kind: callerPane, PaneID: "B", PID: 4242}
	if text := answerViaSocket(t, st10Server(t, true), traced, "A"); refusedAt(text) {
		t.Fatalf("grant on: refused: %q", text)
	}
	if text := answerViaSocket(t, st10Server(t, false), traced, "A"); !strings.Contains(text, promptAnswerGrantOff) {
		t.Fatalf("grant off: %q", text)
	}
}

// Criteria tests 2: claims, unverified callers, workers, scoped sessions and
// relayed or forwarded callers are refused.
func TestST10UntracedCallersAreRefused(t *testing.T) {
	a := st10Server(t, true)
	for name, id := range map[string]callerIdentity{
		"FromEnv claim":                 {Kind: callerPane, PaneID: "B", PID: 4242, FromEnv: true},
		"unverified (header or none)":   {Kind: callerUnverified},
		"worker":                        {Kind: callerWorker, PID: 7},
		"relay":                         {Kind: callerPane, PaneID: "B", Relay: true},
		"forwarded for a remote device": {Kind: callerPane, PaneID: "B", Forwarded: true},
	} {
		for _, transport := range []string{"unix", "tcp"} {
			if v := a.decidePromptAnswer(mcpCallMeta{id: id, transport: transport}, false, a.mgr.panes["A"]); v.allowed {
				t.Errorf("%s over %s: allowed", name, transport)
			}
		}
	}
	if v := a.decidePromptAnswer(mcpCallMeta{id: callerIdentity{Kind: callerPane, PaneID: "B"}, transport: "tcp"}, true, a.mgr.panes["A"]); v.allowed {
		t.Error("a managed agent's scoped session: allowed")
	}
}

// A4: Codex panes can't answer other panes' prompts where their connection
// can't be traced, until the Codex bridge ships. A Codex pane's header claim, or a SOLU_PANE_ID claim of a Codex
// pane, is refused at answer_prompt, at send_input keys on a prompt, and at
// REST.
func TestST10CodexClaimsAreRefused(t *testing.T) {
	a := st10Server(t, true)
	for name, c := range map[string]struct {
		id     callerIdentity
		header string
	}{
		"header claim":  {callerIdentity{Kind: callerUnverified}, "C"},
		"FromEnv claim": {callerIdentity{Kind: callerPane, PaneID: "C", PID: 4242, FromEnv: true}, ""},
	} {
		if text := toolViaHTTP(t, a, c.id, c.header, "answer_prompt", answerArgs("A")); !strings.Contains(text, promptAnswerUntraced) {
			t.Errorf("%s at answer_prompt: %q", name, text)
		}
		text := toolViaHTTP(t, a, c.id, c.header, "send_input", map[string]any{"pane_id": "A", "text": "1"})
		if !strings.Contains(text, "is showing a permission prompt") || !strings.Contains(text, promptAnswerUntraced) {
			t.Errorf("%s at send_input on a prompt: %q", name, text)
		}
		callAsIdentity(t, c.id)
		rec := httptest.NewRecorder()
		if a.requirePromptAnswer(rec, httptest.NewRequest("POST", "/api/panes/A/approvals/r/resolve", nil), a.mgr.panes["A"], "allow a pane's permission request", "") || rec.Code != http.StatusForbidden {
			t.Errorf("%s at REST: allowed (%d)", name, rec.Code)
		}
	}
}

// ST-10 A2, the bot's reproduction: `setsid env SOLU_PANE_ID=<codex pane>
// curl …` from a Claude pane's job and from a worker are refused at
// answer_prompt and at REST, their own prompt included; and the daemon's own
// child with SOLU_PANE_ID set is a worker.
func TestST10SolupaneIDClaimsAreRefused(t *testing.T) {
	a := st10Server(t, true)
	codex := a.mgr.panes["C"]
	codex.Kind = "native"
	codex.nativePendingApprovals = map[string]*NativeApproval{"r1": {RequestID: "r1"}}
	for name, id := range map[string]callerIdentity{
		"detached job of a Claude pane": {Kind: callerPane, PaneID: "C", PID: 4242, FromEnv: true},
		"worker":                        {Kind: callerWorker, PID: 8},
	} {
		for _, target := range []string{"A", "C"} {
			if text := toolViaHTTP(t, a, id, "", "answer_prompt", answerArgs(target)); !refusedAt(text) {
				t.Errorf("%s, answer_prompt on %s: %q", name, target, text)
			}
			callAsIdentity(t, id)
			rec := httptest.NewRecorder()
			if a.requirePromptAnswer(rec, httptest.NewRequest("POST", "/api/panes/"+target+"/approvals/r1/resolve", nil), a.mgr.panes[target], "allow a pane's permission request", "") || rec.Code != http.StatusForbidden {
				t.Errorf("%s, REST on %s: allowed (%d)", name, target, rec.Code)
			}
		}
	}
	child := startSleeper(t, "SOLU_PANE_ID=C")
	if id, known := a.classifyPeerProcess(child.Process.Pid); !known || id.Kind != callerWorker {
		t.Fatalf("daemon child with SOLU_PANE_ID: %+v", id)
	}
}

// Criteria tests 4: a traced pane never answers its own prompt.
func TestST10NeverItsOwnPrompt(t *testing.T) {
	a := st10Server(t, true)
	if text := answerViaSocket(t, a, callerIdentity{Kind: callerPane, PaneID: "A", PID: 4242}, "A"); !strings.Contains(text, promptAnswerOwnPrompt) {
		t.Fatalf("traced pane, own prompt: %q", text)
	}
}

// The pane socket, put-back keys and schedules: a SOLU_PANE_ID claim is held
// at a prompt even with the grant, and a pane is held at its own prompt; a
// traced pane passes at another pane's.
func TestST10KeysAtAPromptOverRESTAndSockets(t *testing.T) {
	a := st10Server(t, true)
	other := a.mgr.panes["A"]
	traced := callerIdentity{Kind: callerPane, PaneID: "B"}
	fromEnv := callerIdentity{Kind: callerPane, PaneID: "B", FromEnv: true}
	if hold := a.promptHoldFor(traced); hold() {
		t.Error("traced pane held with the grant")
	}
	if hold := a.promptHoldFor(fromEnv); !hold() {
		t.Error("SOLU_PANE_ID claim not held")
	}
	if !promptInOwnPane(traced, a.mgr.panes["B"]) || promptInOwnPane(traced, other) || promptInOwnPane(callerIdentity{Kind: callerHuman}, other) {
		t.Error("own-pane test")
	}
	if a.promptWriteCheck(traced, a.mgr.panes["B"], []byte("1"), "pane-socket") == nil {
		t.Error("no write check for a pane's keys to its own pane")
	}
}

// The REST answer to a native approval: the person, or an agent under the
// same rules.
func TestST10RESTApprovalAnswers(t *testing.T) {
	a := st10Server(t, true)
	target := a.mgr.panes["A"]
	for _, tc := range []struct {
		name string
		id   callerIdentity
		want bool
	}{
		{"person", callerIdentity{Kind: callerHuman}, true},
		{"traced pane", callerIdentity{Kind: callerPane, PaneID: "B"}, true},
		{"own pane", callerIdentity{Kind: callerPane, PaneID: "A"}, false},
		{"SOLU_PANE_ID claim", callerIdentity{Kind: callerPane, PaneID: "C", FromEnv: true}, false},
		{"worker", callerIdentity{Kind: callerWorker}, false},
		{"unverified", callerIdentity{Kind: callerUnverified}, false},
	} {
		callAsIdentity(t, tc.id)
		rec := httptest.NewRecorder()
		got := a.requirePromptAnswer(rec, httptest.NewRequest("POST", "/api/panes/A/approvals/r/resolve", nil), target, "allow a pane's permission request", "")
		if got != tc.want || !got && rec.Code != http.StatusForbidden {
			t.Errorf("%s: allowed=%t code=%d", tc.name, got, rec.Code)
		}
	}
}

// The REST refusal for a caller that isn't an agent keeps refuseCaller's
// code and guidance (device_required for an unapproved device).
func TestST10RESTRefusalKeepsDeviceGuidance(t *testing.T) {
	a := st10Server(t, true)
	callAsIdentity(t, callerIdentity{Kind: callerUnverified, Unproven: true})
	rec := httptest.NewRecorder()
	if a.requirePromptAnswer(rec, httptest.NewRequest("POST", "/x", nil), a.mgr.panes["A"], "deny a pane's permission request", "") {
		t.Fatal("allowed")
	}
	if !strings.Contains(rec.Body.String(), "device_required") || !strings.Contains(rec.Body.String(), "approved device") {
		t.Fatalf("body %s", rec.Body.String())
	}
}

// Interrupting a native pane denies its pending approvals: gated like the
// REST deny, and nothing is denied when refused.
func TestST10NativeInterruptDenyIsGated(t *testing.T) {
	a := st10Server(t, true)
	p := a.mgr.panes["A"]
	p.Kind = "native"
	p.nativePendingApprovals = map[string]*NativeApproval{"r1": {RequestID: "r1"}}
	for name, id := range map[string]callerIdentity{
		"SOLU_PANE_ID claim": {Kind: callerPane, PaneID: "C", FromEnv: true},
		"worker":             {Kind: callerWorker},
		"own pane":           {Kind: callerPane, PaneID: "A"},
	} {
		callAsIdentity(t, id)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/panes/A/native/interrupt", nil)
		req.SetPathValue("id", "A")
		a.handlePaneNativeInterrupt(rec, req)
		if rec.Code != http.StatusForbidden || p.peekNativePending("r1") == nil {
			t.Errorf("%s: code %d, pending %v", name, rec.Code, p.peekNativePending("r1") != nil)
		}
	}
}

// A4: turning on auto-mode, adding to the native allowlist and approving an
// Aria confirmation follow the same rules: for its own pane a pane is
// refused, for another pane under the grant it is allowed, and a SOLU_PANE_ID
// claim is refused on all three. The person is unchanged.
func TestST10GrantRoutesFollowThePromptRules(t *testing.T) {
	routes := map[string]func(a *apiServer) *httptest.ResponseRecorder{
		"turn on auto-mode": func(a *apiServer) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "/api/panes/A/auto-mode", strings.NewReader(`{"autoMode":"always"}`))
			req.SetPathValue("id", "A")
			rec := httptest.NewRecorder()
			a.setPaneAutoMode(rec, req)
			return rec
		},
		"add to the native allowlist": func(a *apiServer) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "/api/panes/A/native/allowlist", strings.NewReader(`{"tool":"Read","action":"add"}`))
			req.SetPathValue("id", "A")
			rec := httptest.NewRecorder()
			a.paneNativeAllowlistPost(rec, req)
			return rec
		},
		"approve an Aria confirmation": func(a *apiServer) *httptest.ResponseRecorder {
			a.aria = testAriaWithPane(t, a.mgr.panes["A"])
			a.aria.confirms["c1"] = &ariaPendingConfirm{ch: make(chan bool, 1), paneID: "A"}
			req := httptest.NewRequest("POST", "/api/aria/confirm", strings.NewReader(`{"id":"c1","approve":true}`))
			rec := httptest.NewRecorder()
			a.handleAriaConfirm(rec, req)
			return rec
		},
	}
	audit := map[string]string{
		"turn on auto-mode":            `prompt="" action="turn on auto-approve"`,
		"add to the native allowlist":  `prompt="" action="add an auto-allow rule"`,
		"approve an Aria confirmation": `prompt="aria-confirm:c1" action="approve an Aria confirmation"`,
	}
	for route, call := range routes {
		for _, tc := range []struct {
			name    string
			id      callerIdentity
			allowed bool
		}{
			{"another pane under the grant", callerIdentity{Kind: callerPane, PaneID: "B"}, true},
			{"its own pane", callerIdentity{Kind: callerPane, PaneID: "A"}, false},
			{"SOLU_PANE_ID claim", callerIdentity{Kind: callerPane, PaneID: "B", FromEnv: true}, false},
			{"person", callerIdentity{Kind: callerHuman}, true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				a := st10Server(t, true)
				callAsIdentity(t, tc.id)
				logged := captureAuthorityLog(t)
				rec := call(a)
				if got := rec.Code != http.StatusForbidden; got != tc.allowed {
					t.Fatalf("code %d, body %s", rec.Code, rec.Body.String())
				}
				// The act is named only for an agent the rules let through.
				agentAllowed := tc.allowed && tc.id.Kind == callerPane
				if got := strings.Contains(logged.String(), "pane=B (traced) target=A "+audit[route]); got != agentAllowed {
					t.Fatalf("code %d, log %q", rec.Code, logged.String())
				}
			})
		}
	}
}

// Rule 7: the log names the transport a write came by.
func TestST10PromptAnswerLogNamesTheTransport(t *testing.T) {
	logged := captureAuthorityLog(t)
	p := testPaneWithTail(t, "T", "T", "")
	notePromptAnsweredBy(callerIdentity{Kind: callerPane, PaneID: "B"}, p, "rest-schedule")
	if !strings.Contains(logged.String(), `transport=rest-schedule pane=B (traced) target=T prompt="rev:0" action="keys at a prompt"`) {
		t.Fatalf("log %q", logged.String())
	}
}

// st10DrawnDialog adds pane D to a: an agent's dialog still drawn after its
// first answer (menuInputDrawn), which the cached state counts as answered.
func st10DrawnDialog(t *testing.T, a *apiServer) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, "D")
	p.Agent = agentClaude
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	typeMenuSteps(t, p, frames, []menuStep{{"\x1b[B", questionFocused(2, "")}})
	a.mgr.panes["D"] = p
	if got := p.menuInputState(); got != menuInputDrawn || p.computeStateCached() == StateChoicePrompt {
		t.Fatalf("fixture: menu input state %d, cached state %s", got, p.computeStateCached())
	}
	return p, frames
}

// expectHeldAtDrawnDialog sends id's keys to D by every text-delivery route:
// each refuses, and nothing reaches the pane.
func expectHeldAtDrawnDialog(t *testing.T, a *apiServer, p *Pane, frames <-chan directAutoFrameRead, name string, id callerIdentity, header string) {
	t.Helper()
	for _, text := range []string{"1", "\x1b[B"} {
		got := toolViaHTTP(t, a, id, header, "ask_pane", map[string]any{"pane_id": "D", "text": text})
		if !strings.Contains(got, errPromptHeld.Error()) && !strings.Contains(got, "your own pane") {
			t.Errorf("%s, ask_pane %q: %q", name, text, got)
		}
		got = toolViaHTTP(t, a, id, header, "send_input", map[string]any{"pane_id": "D", "text": text})
		if !strings.Contains(got, "is showing a permission prompt") || !refusedAt(got) {
			t.Errorf("%s, send_input %q: %q", name, text, got)
		}
	}
	callAsIdentity(t, id)
	policy := a.socketInputPolicyFor(id)
	if policy.allowed(p) {
		t.Errorf("%s: the pane socket's gate let keys through", name)
	}
	if check := policy.promptCheck(p, []byte("1")); check == nil || check() == nil {
		t.Errorf("%s: the pane socket's write check let keys through", name)
	}
	for route, handler := range map[string]struct {
		call http.HandlerFunc
		body string
	}{
		"schedule": {a.injectPane, `{"text":"1"}`},
		"put-back": {a.actOnUnsentMessage, `{"id":"m1","action":"send"}`},
	} {
		req := httptest.NewRequest("POST", "/api/panes/D/x", strings.NewReader(handler.body))
		req.SetPathValue("id", "D")
		rec := httptest.NewRecorder()
		handler.call(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Errorf("%s, %s: %d %s", name, route, rec.Code, rec.Body.String())
		}
	}
	expectNoFrame(t, frames, name)
}

// A drawn dialog counts as at a prompt on every text-delivery route (ST-10
// rules 2 and 4), with the grant on or off.
func TestST10DrawnDialogHoldsEveryRoute(t *testing.T) {
	a := st10Server(t, true)
	p, frames := st10DrawnDialog(t, a)
	for name, c := range map[string]struct {
		id     callerIdentity
		header string
	}{
		"worker":                 {id: callerIdentity{Kind: callerWorker, PID: 8}},
		"untraced, no claim":     {id: callerIdentity{Kind: callerUnverified}},
		"untraced, header claim": {id: callerIdentity{Kind: callerUnverified}, header: "B"},
		"SOLU_PANE_ID claim":     {id: callerIdentity{Kind: callerPane, PaneID: "B", PID: 4242, FromEnv: true}},
		"its own traced pane":    {id: callerIdentity{Kind: callerPane, PaneID: "D"}},
	} {
		expectHeldAtDrawnDialog(t, a, p, frames, name, c.id, c.header)
	}

	off := st10Server(t, false)
	p, frames = st10DrawnDialog(t, off)
	expectHeldAtDrawnDialog(t, off, p, frames, "traced pane, grant off", callerIdentity{Kind: callerPane, PaneID: "B"}, "")
}

// Rule 4: the detached child of pane A, whose only link to A is the
// SOLU_PANE_ID it inherited, is refused at A's own drawn dialog.
func TestST10DetachedChildIsRefusedAtItsPanesDrawnDialog(t *testing.T) {
	for _, grant := range []bool{true, false} {
		a := st10Server(t, grant)
		p, frames := st10DrawnDialog(t, a)
		expectHeldAtDrawnDialog(t, a, p, frames, "detached child", callerIdentity{Kind: callerPane, PaneID: "D", PID: 4242, FromEnv: true}, "")
	}
}

// ask_pane is a message and never an answer: even a traced pane the grant lets
// answer prompts doesn't answer a drawn dialog with one.
func TestST10AskPaneNeverAnswersADrawnDialog(t *testing.T) {
	a := st10Server(t, true)
	_, frames := st10DrawnDialog(t, a)
	got := toolViaHTTP(t, a, callerIdentity{Kind: callerPane, PaneID: "B"}, "", "ask_pane", map[string]any{"pane_id": "D", "text": "1"})
	if !strings.Contains(got, errPromptHeld.Error()) {
		t.Fatalf("ask_pane: %q", got)
	}
	expectNoFrame(t, frames, "ask_pane")
}

// Keys at a drawn dialog answer it: a traced pane under the grant gets them
// through by send_input and the pane socket, and the answer is audited.
func TestST10TracedPaneAnswersADrawnDialogAndIsAudited(t *testing.T) {
	a := st10Server(t, true)
	p, frames := st10DrawnDialog(t, a)
	traced := callerIdentity{Kind: callerPane, PaneID: "B"}
	policy := a.socketInputPolicyFor(traced)
	if !policy.allowed(p) {
		t.Fatal("the pane socket's gate held a traced pane under the grant")
	}
	before := promptRevID(p)
	logged := captureAuthorityLog(t)
	got := toolViaHTTP(t, a, traced, "", "send_input", map[string]any{"pane_id": "D", "text": "1"})
	if refusedAt(got) || strings.Contains(got, "permission prompt") {
		t.Fatalf("send_input: %q", got)
	}
	expectFrame(t, frames, "the answer", "1")
	want := `transport=tcp pane=B (traced) target=D prompt="` + before + `" action="send_input"`
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("log %q", logged.String())
	}
}

const promptAnswerLogPrefix = "authority: prompt answer by agent"

// st10NativePane adds native pane N to a, with a parallel approval open for
// each request id.
func st10NativePane(t *testing.T, a *apiServer, reqIDs ...string) *Pane {
	t.Helper()
	p, agentEnd := nativePaneForApproval(t, "N")
	go func() { _, _ = io.Copy(io.Discard, agentEnd) }()
	for _, id := range reqIDs {
		p.processNativeLine([]byte(nativeCanUseToolLine(id, id+"-use", "Read", `{"file_path":"/tmp/example"}`)), false, false)
		if p.peekNativePending(id) == nil {
			t.Fatalf("approval %s not pending", id)
		}
	}
	a.mgr.panes["N"] = p
	return p
}

// Rule 7, MCP: with two parallel approvals open, the audit line names the one
// answer_prompt actually resolved. Its pick among them varies, so repeat.
func TestST10AuditNamesTheApprovalAnswerPromptResolved(t *testing.T) {
	traced := callerIdentity{Kind: callerPane, PaneID: "B"}
	for i := 0; i < 8; i++ {
		a := st10Server(t, true)
		p := st10NativePane(t, a, "r1", "r2")
		logged := captureAuthorityLog(t)
		text := toolViaHTTP(t, a, traced, "", "answer_prompt", map[string]any{"pane_id": "N", "option": "allow"})
		if !strings.Contains(text, "Allowed") {
			t.Fatalf("answer: %q", text)
		}
		answered, open := "r1", "r2"
		if p.peekNativePending("r1") != nil {
			answered, open = "r2", "r1"
		}
		if p.peekNativePending(open) == nil || p.peekNativePending(answered) != nil {
			t.Fatalf("exactly one approval should be resolved: %s open, %s answered", open, answered)
		}
		want := `transport=tcp pane=B (traced) target=N prompt="approval:` + answered + `" action="answer_prompt"`
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("resolved %s, log %q", answered, logged.String())
		}
	}
}

// Rule 7, REST: resolving either of two parallel approvals logs that one, and
// a resolve that finds nothing to answer logs no answer.
func TestST10AuditNamesTheApprovalRESTResolved(t *testing.T) {
	resolve := func(a *apiServer, reqID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/panes/N/approvals/x/resolve", strings.NewReader(`{"allow":true}`))
		req.SetPathValue("id", "N")
		req.SetPathValue("reqId", reqID)
		rec := httptest.NewRecorder()
		a.handlePaneApprovalResolve(rec, req)
		return rec
	}
	for _, tc := range []struct{ answered, open string }{{"r1", "r2"}, {"r2", "r1"}} {
		a := st10Server(t, true)
		p := st10NativePane(t, a, "r1", "r2")
		callAsIdentity(t, callerIdentity{Kind: callerPane, PaneID: "B"})
		logged := captureAuthorityLog(t)
		if rec := resolve(a, tc.answered); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved":true`) {
			t.Fatalf("resolve %s: %d %s", tc.answered, rec.Code, rec.Body.String())
		}
		if p.peekNativePending(tc.answered) != nil || p.peekNativePending(tc.open) == nil {
			t.Fatalf("resolving %s should leave only %s open", tc.answered, tc.open)
		}
		want := `transport=rest pane=B (traced) target=N prompt="approval:` + tc.answered + `" action="allow a pane's permission request"`
		if !strings.Contains(logged.String(), want) || strings.Contains(logged.String(), "approval:"+tc.open) {
			t.Fatalf("resolved %s, log %q", tc.answered, logged.String())
		}
	}

	a := st10Server(t, true)
	st10NativePane(t, a, "r1")
	callAsIdentity(t, callerIdentity{Kind: callerPane, PaneID: "B"})
	logged := captureAuthorityLog(t)
	if rec := resolve(a, "gone"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved":false`) {
		t.Fatalf("unknown id: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logged.String(), promptAnswerLogPrefix) {
		t.Fatalf("no answer happened, log %q", logged.String())
	}
}

// Rule 7: interrupting denies every open approval, and the line names each.
func TestST10AuditNamesEveryApprovalAnInterruptDenied(t *testing.T) {
	a := st10Server(t, true)
	p := st10NativePane(t, a, "r2", "r1")
	callAsIdentity(t, callerIdentity{Kind: callerPane, PaneID: "B"})
	logged := captureAuthorityLog(t)
	req := httptest.NewRequest("POST", "/api/panes/N/native/interrupt", nil)
	req.SetPathValue("id", "N")
	a.handlePaneNativeInterrupt(httptest.NewRecorder(), req)
	if p.hasNativePending() {
		t.Fatal("approvals still open")
	}
	want := `pane=B (traced) target=N prompt="approval:r1,r2" action="deny a pane's permission request"`
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("log %q", logged.String())
	}
}

// Rule 7, terminal panes: the line names the menu's revision from before the
// answer, and only an answer that was written is logged.
func TestST10AuditTerminalAnswers(t *testing.T) {
	traced := callerIdentity{Kind: callerPane, PaneID: "B"}
	for tool, args := range map[string]map[string]any{
		"answer_prompt": {"pane_id": "A", "option": "1"},
		"send_input":    {"pane_id": "A", "text": "1"},
	} {
		a := st10Server(t, true)
		before := promptRevID(a.mgr.panes["A"])
		logged := captureAuthorityLog(t)
		toolViaHTTP(t, a, traced, "", tool, args)
		if a.mgr.panes["A"].inputRevision.Load() == 0 {
			t.Fatalf("%s wrote nothing", tool)
		}
		want := `transport=tcp pane=B (traced) target=A prompt="` + before + `" action="` + tool + `"`
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("%s: log %q", tool, logged.String())
		}
	}

	a := st10Server(t, true)
	logged := captureAuthorityLog(t)
	text := toolViaHTTP(t, a, traced, "", "send_input", map[string]any{"pane_id": "A", "text": "much too long to be a menu answer"})
	if !strings.Contains(text, "NOT sent") || strings.Contains(logged.String(), promptAnswerLogPrefix) {
		t.Fatalf("refused send: %q, log %q", text, logged.String())
	}
}
