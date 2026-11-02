package main

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func farFuture() time.Time { return time.Now().Add(time.Hour) }

// drainConn attaches a drained in-memory agent connection so Write/
// WriteNoPromptRecord complete instead of erroring "pane not connected".
func drainConn(t *testing.T, p *Pane) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c2.Read(buf); err != nil {
				return
			}
		}
	}()
	p.mu.Lock()
	p.agentConn = c1
	p.mu.Unlock()
}

func TestParseMenuOptions(t *testing.T) {
	t.Parallel()
	tail := strings.Join([]string{
		"Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, allow all edits during this session",
		"   3. No, and tell Claude what to do differently",
	}, "\n")
	opts := parseMenuOptions(tail)
	if len(opts) != 3 {
		t.Fatalf("parsed %d options, want 3: %+v", len(opts), opts)
	}
	if opts["1"] != "Yes" {
		t.Fatalf("option 1 = %q, want Yes", opts["1"])
	}
	if !strings.HasPrefix(opts["2"], "Yes, allow") {
		t.Fatalf("option 2 = %q", opts["2"])
	}

	// Stacked menu: the BOTTOM-most copy wins (earlier is scrollback).
	stacked := tail + "\n...spinner...\n" + strings.Join([]string{
		" ❯ 1. Yes (retry)",
		"   2. No",
	}, "\n")
	opts = parseMenuOptions(stacked)
	if opts["1"] != "Yes (retry)" {
		t.Fatalf("stacked option 1 = %q, want the bottom-most 'Yes (retry)'", opts["1"])
	}

	// Prose with no menu → nothing parsed.
	if got := parseMenuOptions("Would you like me to continue with the refactor?"); len(got) != 0 {
		t.Fatalf("prose parsed as menu: %+v", got)
	}
}

// choicePromptPane builds a terminal pane sitting on the canonical Claude
// approval menu — computeState returns StateChoicePrompt for it.
func choicePromptPane(t *testing.T, id string) *Pane {
	t.Helper()
	p := &Pane{
		ID:     id,
		Cols:   84,
		Rows:   31,
		Status: PaneRunning,
		buffer: NewRingBuffer(1 << 20),
		subs:   map[int64]*subEntry{},
	}
	menu := strings.Join([]string{
		"Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, allow all edits during this session",
		"   3. No, and tell Claude what to do differently",
	}, "\n") + "\n"
	if _, err := p.buffer.Write([]byte(menu)); err != nil {
		t.Fatalf("buffer.Write: %v", err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(menu)))
	if got := p.computeState(); got != StateChoicePrompt {
		t.Fatalf("setup pane not in choice-prompt: got %q", got)
	}
	return p
}

func TestAnswerPromptTerminalSelectsValidOption(t *testing.T) {
	t.Parallel()
	p := choicePromptPane(t, "pane_ap1")
	drainConn(t, p) // so the PTY write completes
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}

	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "2"}))
	if isErr {
		t.Fatalf("valid option refused: %q", text)
	}
	if !strings.Contains(text, "Selected option 2") {
		t.Fatalf("result = %q, want confirmation of option 2", text)
	}
}

func TestAnswerPromptTerminalRejectsAbsentOption(t *testing.T) {
	t.Parallel()
	p := choicePromptPane(t, "pane_ap2")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}

	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "9"}))
	if !isErr {
		t.Fatalf("absent option 9 was accepted: %q", text)
	}
	if !strings.Contains(text, "not on pane") || !strings.Contains(text, "Presented options") {
		t.Fatalf("error = %q, want an options listing", text)
	}
}

func TestAnswerPromptRefusesNonPromptPane(t *testing.T) {
	t.Parallel()
	// A plain idle pane — not on a menu.
	p := testPaneWithTail(t, "pane_ap3", "worker", "$ ls\nfile.txt\n$ ")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}

	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "1"}))
	if !isErr {
		t.Fatalf("answer_prompt acted on a non-prompt pane: %q", text)
	}
	if !strings.Contains(text, "not on a choice prompt") {
		t.Fatalf("error = %q, want choice-prompt refusal", text)
	}
}

func TestAnswerPromptExitedPane(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "pane_ap4", "worker", "done")
	p.Status = PaneExited
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}

	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "1"}))
	if !isErr || !strings.Contains(text, "has exited") {
		t.Fatalf("exited pane answer = (%q, err=%v), want exited refusal", text, isErr)
	}
}

func TestAnswerPromptNativeValidatesOptionAndPending(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane_apn",
		Title:  "focus",
		Kind:   "native",
		Status: PaneRunning,
		buffer: NewRingBuffer(1 << 16),
		subs:   map[int64]*subEntry{},
	}
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}

	// A native pane only takes allow/deny — a menu number is a category error.
	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "2"}))
	if !isErr || !strings.Contains(text, `must be "allow" or "deny"`) {
		t.Fatalf("native numeric option = (%q, err=%v), want allow/deny guidance", text, isErr)
	}

	// Valid option but nothing pending → clear refusal, not a silent no-op.
	text, isErr = mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "allow"}))
	if !isErr || !strings.Contains(text, "no pending approval") {
		t.Fatalf("native no-pending answer = (%q, err=%v), want no-pending refusal", text, isErr)
	}
}

func TestAnswerPromptScopeGated(t *testing.T) {
	t.Parallel()
	// answer_prompt must be in a scoped caller's budget to be callable.
	p := choicePromptPane(t, "pane_ap5")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus(), agentAuthority: grantedAgentAuthority(t)}
	readOnly := &mcpScope{Identity: "reviewer", Tools: map[string]bool{"get_pane_state": true}, Expires: farFuture()}

	text, isErr := mcpResultText(t, scopedCallForTest(t, a, readOnly, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "1"}))
	if !isErr || !strings.Contains(text, "not in this session's scope") {
		t.Fatalf("out-of-scope answer_prompt = (%q, err=%v), want scope refusal", text, isErr)
	}
}

// grantedAgentAuthority is the person's agent-permissions grant switched on, as
// these tests answer prompts the way an interactive agent would.
func grantedAgentAuthority(t *testing.T) *agentAuthority {
	t.Helper()
	g := newAgentAuthority(t.TempDir())
	if err := g.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	return g
}

// Without the grant an interactive agent cannot answer another pane's prompt.
func TestAnswerPromptNeedsAgentPermissions(t *testing.T) {
	p := choicePromptPane(t, "pane_ap_grant")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "answer_prompt",
		map[string]any{"pane_id": p.ID, "option": "1"}))
	if !isErr || !strings.Contains(text, "Agent permissions") {
		t.Fatalf("answer_prompt without the grant = %q (err=%v)", text, isErr)
	}
}
