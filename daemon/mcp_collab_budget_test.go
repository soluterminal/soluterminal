package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ST-290: MCP clients cut a tool call at about 60 s and report nothing of
// what it did. ask_pane and wait_for_pane return before their deadline,
// sending included, with the delivery and a wait_for_pane hint.

func shortCollabBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := collabCallBudget
	collabCallBudget = d
	t.Cleanup(func() { collabCallBudget = prev })
}

func collabResultOf(t *testing.T, got any) collabResult {
	t.Helper()
	text, isErr := mcpResultText(t, got)
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return res
}

// claudeBudgetPane is an idle Claude pane whose fake agent runs reply after
// the message is submitted (nil: the agent works on without replying).
func claudeBudgetPane(t *testing.T, id, message string, reply func(transcript string)) (*Pane, *apiServer) {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(transcript, []byte(claudeLine("user", time.Now().Add(-time.Hour), `"promptSource":"typed","message":{"role":"user","content":"earlier"}`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := testPaneWithTail(t, id, "claude", "❯ \n")
	p.Cmd = []string{"claude"}
	p.ClaudeTranscriptPath = transcript
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	replyAfterSubmit(t, p, message, func() {
		if reply != nil {
			reply(transcript)
		}
	})
	return p, a
}

func appendClaudeReply(transcript, prompt, reply string) {
	now := time.Now()
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	defer f.Close()
	fmt.Fprintln(f, claudeLine("user", now, `"promptSource":"typed","message":{"role":"user","content":"`+prompt+`"}`))
	fmt.Fprintln(f, claudeLine("assistant", now, `"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"`+reply+`"}]}`))
}

// A timeout_seconds past the client's cut is cut to the call's budget, and a
// typed message says it was delivered.
func TestAskPaneReturnsBeforeTheClientCut(t *testing.T) {
	shortCollabBudget(t, 700*time.Millisecond)
	_, a := claudeBudgetPane(t, "pane_slow", "a long review", nil)
	start := time.Now()
	res := collabResultOf(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_slow", "text": "a long review", "timeout_seconds": 90.0}, "pane_caller"))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("ask_pane took %s with a 700 ms budget", took)
	}
	if res.Status != "still_working" || res.Delivery != "delivered" || !strings.Contains(res.Hint, "wait_for_pane") {
		t.Fatalf("result: %+v", res)
	}
	start = time.Now()
	res = collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_slow", "timeout_seconds": 90.0}, "pane_caller"))
	if took := time.Since(start); took > 5*time.Second || res.Status != "still_working" {
		t.Fatalf("wait_for_pane: %s %+v", took, res)
	}
}

// A send blocked behind another write to the pane ends the call at its
// deadline with delivery "sending"; wait_for_pane follows that send and
// then that message's own turn.
func TestAskPaneReturnsWhileTheSendIsBlocked(t *testing.T) {
	shortCollabBudget(t, 700*time.Millisecond)
	p, a := claudeBudgetPane(t, "pane_locked", "review my change", func(transcript string) {
		appendClaudeReply(transcript, "review my change", "Looks good.")
	})
	p.agentWriteMu.Lock() // another writer, or a stalled write
	res := collabResultOf(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_locked", "text": "review my change"}, "pane_caller"))
	if res.Status != "still_working" || res.Delivery != "sending" || !strings.Contains(res.Hint, "do not resend") {
		p.agentWriteMu.Unlock()
		t.Fatalf("blocked send: %+v", res)
	}
	res = collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_locked"}, "pane_caller"))
	if res.Status != "still_working" || res.Delivery != "sending" {
		p.agentWriteMu.Unlock()
		t.Fatalf("still blocked: %+v", res)
	}
	p.agentWriteMu.Unlock()
	shortCollabBudget(t, 10*time.Second)
	res = collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_locked"}, "pane_caller"))
	if res.Status != "idle" || res.Reply != "Looks good." || res.Delivery != "delivered" {
		t.Fatalf("after the send: %+v", res)
	}
	if _, open := a.queuedAsks.get("pane_locked", "pane_caller"); open {
		t.Fatal("the answered ask stayed open")
	}
}

// A send that fails after ask_pane returned is reported, once, as unsent.
func TestAskPaneLateSendFailureIsReported(t *testing.T) {
	shortCollabBudget(t, 500*time.Millisecond)
	p, a := claudeBudgetPane(t, "pane_gone", "hello", nil)
	p.agentWriteMu.Lock()
	res := collabResultOf(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_gone", "text": "hello"}, "pane_caller"))
	if res.Delivery != "sending" {
		p.agentWriteMu.Unlock()
		t.Fatalf("blocked send: %+v", res)
	}
	p.mu.Lock()
	_ = p.agentConn.Close() // the agent connection dies before the write
	p.mu.Unlock()
	p.agentWriteMu.Unlock()
	shortCollabBudget(t, 10*time.Second)
	res = collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_gone"}, "pane_caller"))
	if res.Status != "unsent" || !strings.Contains(res.Hint, "not sent") {
		t.Fatalf("late failure: %+v", res)
	}
	if _, open := a.queuedAsks.get("pane_gone", "pane_caller"); open {
		t.Fatal("the failed ask stayed open")
	}
}

// SoluTerminal configures a Codex pane's MCP client with
// codexSoluToolTimeoutSec, so a traced Codex caller keeps its long wait.
// Everyone else, unverified callers included, gets the budget.
func TestCollabDeadlineByCaller(t *testing.T) {
	codex := testPaneWithTail(t, "pane_codex", "codex", "")
	codex.Agent = agentCodex
	claude := testPaneWithTail(t, "pane_claude_caller", "claude", "")
	claude.Agent = agentClaude
	m := testManagerWithPane(codex)
	m.panes[claude.ID] = claude
	a := &apiServer{mgr: m, uiBus: newUIBus()}
	start := time.Now()
	for _, tc := range []struct {
		c    paneCaller
		want time.Duration
	}{
		{paneCaller{ID: "pane_codex"}, time.Duration(codexSoluToolTimeoutSec)*time.Second - 10*time.Second},
		{paneCaller{ID: "pane_claude_caller"}, collabCallBudget},
		{paneCaller{Claimed: "pane_codex"}, collabCallBudget},
		{paneCaller{}, collabCallBudget},
	} {
		if got := a.collabDeadline(start, tc.c).Sub(start); got != tc.want {
			t.Fatalf("%+v: budget %s, want %s", tc.c, got, tc.want)
		}
	}
	if collabCallBudget >= 60*time.Second {
		t.Fatal("the budget must end calls before the clients' 60 s cut")
	}
}

// While its send is still pending, wait_for_pane keeps to timeout_seconds,
// not only to the call's budget.
func TestWaitForPaneOnAPendingSendKeepsItsTimeout(t *testing.T) {
	shortCollabBudget(t, 500*time.Millisecond)
	p, a := claudeBudgetPane(t, "pane_pending", "hello", nil)
	p.agentWriteMu.Lock()
	defer p.agentWriteMu.Unlock()
	if res := collabResultOf(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_pending", "text": "hello"}, "pane_caller")); res.Delivery != "sending" {
		t.Fatalf("blocked send: %+v", res)
	}
	shortCollabBudget(t, 30*time.Second)
	start := time.Now()
	res := collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_pending", "timeout_seconds": 0.3}, "pane_caller"))
	if took := time.Since(start); took > 3*time.Second || res.Delivery != "sending" {
		t.Fatalf("wait_for_pane took %s: %+v", took, res)
	}
}

// A late send counts as started work once it finished sentAskStarted ago,
// also when that comes during the wait: with no transcript, the settled
// screen ends the wait.
func TestWaitForPaneAfterALateSendTakesTheSettledScreen(t *testing.T) {
	shortCollabBudget(t, 500*time.Millisecond)
	prev := sentAskStarted
	// Short, but not zero: wait_for_pane starts before the send is this old,
	// so the wait itself must notice it come of age.
	sentAskStarted = 400 * time.Millisecond
	t.Cleanup(func() { sentAskStarted = prev })
	p := testPaneWithTail(t, "pane_shell", "bash", "$ \n")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	replyAfterSubmit(t, p, "make test", func() {})
	p.agentWriteMu.Lock()
	if res := collabResultOf(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_shell", "text": "make test"}, "pane_caller")); res.Delivery != "sending" {
		p.agentWriteMu.Unlock()
		t.Fatalf("blocked send: %+v", res)
	}
	p.agentWriteMu.Unlock()
	base, _ := a.queuedAsks.get("pane_shell", "pane_caller")
	select {
	case <-base.Sending.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the send did not finish")
	}
	shortCollabBudget(t, 10*time.Second)
	res := collabResultOf(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_shell"}, "pane_caller"))
	if res.Status != "idle" || res.Delivery != "delivered" {
		t.Fatalf("after a late send: %+v", res)
	}
}

// A held message typed since reports how it left the input box, not a bare
// "delivered": nothing for text left in the box, and never over unsent.
func TestNoteHeldAskReportsTheTypingOutcome(t *testing.T) {
	p := testPaneWithTail(t, "pane_held", "claude", "")
	for _, tc := range []struct {
		resolution string
		outcome    deliveryOutcome
		status     string
		want       string
	}{
		{"delivered", deliverySubmitted, "replied", "delivered"},
		{"sent", deliveryUnconfirmed, "still_working", "unconfirmed"},
		{"delivered", deliveryQueued, "still_working", "queued"},
		{"delivered", deliveryLeftInComposer, "still_working", ""},
		{"delivered", deliverySubmitted, "unsent", ""},
	} {
		id := fmt.Sprintf("held-%s-%s-%s", tc.resolution, tc.outcome, tc.status)
		p.mu.Lock()
		p.heldResolved = append(p.heldResolved, &heldDelivery{ID: id, resolution: tc.resolution, outcome: tc.outcome})
		p.mu.Unlock()
		res := collabResult{Status: tc.status}
		noteHeldAsk(p, id, &res)
		if res.Delivery != tc.want {
			t.Fatalf("%s %s %s: delivery %q, want %q", tc.resolution, tc.outcome, tc.status, res.Delivery, tc.want)
		}
	}
}

// Callers SoluTerminal can't tell apart share a bucket, so a typed ask's
// baseline is not kept for them; a traced caller's is.
func TestTypedAskBaselineNeedsACallerBucket(t *testing.T) {
	a := &apiServer{}
	base := paneTurnBaseline{Since: time.Now()}
	a.rememberOpenAsk("pane_x", paneCaller{}, base, deliverySubmitted, "still_working")
	if _, ok := a.queuedAsks.get("pane_x", ""); ok {
		t.Fatal("a typed ask was kept in the shared bucket")
	}
	a.rememberOpenAsk("pane_x", paneCaller{ID: "pane_caller"}, base, deliverySubmitted, "still_working")
	if got, ok := a.queuedAsks.get("pane_x", "pane_caller"); !ok || got.Delivery != "delivered" {
		t.Fatalf("traced caller's typed ask: %+v %v", got, ok)
	}
}
