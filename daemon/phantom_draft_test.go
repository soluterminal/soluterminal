package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 2026-11-02, @lead (Claude, auto-approve always): another agent's send_input
// had no trailing newline, so 1069 bytes went in raw. Claude showed them as an
// unsubmitted "[Pasted text #7]", and the recorder counted them as the person's
// draft. A message for an agent pane is refused unsent instead; keys still
// go through.
func TestSendInputRefusesUnsubmittedMessageToAgentPane(t *testing.T) {
	stubSendInputBusy(t, false)
	p, frames := newTypingHoldPane(t, "pane_agent_target")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	if agent, _ := paneAgentProvider(p); !agent {
		t.Fatal("fixture: want an agent pane")
	}
	send := func(text string) (string, bool) {
		t.Helper()
		return mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": text}, "pane_qa"))
	}
	for _, msg := range []string{
		"[from @qa] fbbbccdf: NOT READY, 1 MEDIUM (small fix). " + strings.Repeat("detail ", 150),
		"please review the delta",
		"line one\nline two",
	} {
		got, isErr := send(msg)
		if !isErr || !strings.Contains(got, "NOT sent") || !strings.Contains(got, `"\n"`) {
			t.Fatalf("unsubmitted message %q was typed or not explained: %q", msg[:min(len(msg), 30)], got)
		}
		expectNoFrame(t, frames, "nothing typed for an unsubmitted message")
	}
	if pending := p.composerPendingLen(); pending != 0 {
		t.Fatalf("refused message reached the recorder: %q", p.promptRec.buf)
	}
	// Keys keep raw semantics: a one- or two-key answer, Esc, an arrow.
	for _, key := range []string{"y", "12", "\x1b", "\x1b[A", "\x03"} {
		if got, isErr := send(key); isErr {
			t.Fatalf("key %q refused: %s", key, got)
		}
		expectFrame(t, frames, "key", key)
	}
}

// A plain shell pane still takes unterminated text: completing a command line
// in a shell is ordinary use, and nothing there holds auto-approval.
func TestSendInputUnterminatedTextStillTypesIntoShell(t *testing.T) {
	stubSendInputBusy(t, false)
	p, frames := newTypingHoldPane(t, "pane_shell_partial")
	p.Agent, p.forceCodexForTest = "", false
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	if got, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "git status --short"}, "pane_caller")); isErr {
		t.Fatalf("shell text refused: %s", got)
	}
	expectFrame(t, frames, "shell text", "git status --short")
}

// The live hold that renewed at 01:48:16: auto-approval's Enter (unrecorded)
// answered step 1 of a dialog, and the person's own key reached the same
// dialog while it was still drawn. computeState already counted the menu
// answered, so the key was sampled "drawn", the recorder never entered the
// menu, and the key became a composer draft holding the next step for 2m.
func TestKeyAimedAtDialogAfterAutoAnswerIsNotADraft(t *testing.T) {
	p, frames := newTypingHoldPane(t, "auto-answered-then-key")
	p.Cols, p.Rows = 160, 48
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	if p.computeState() != StateChoicePrompt {
		t.Fatal("fixture is not a menu on screen")
	}
	// Auto-approval answers it, through the unrecorded writer.
	if _, err := p.WriteNoPromptRecord([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "auto-approval Enter", "\r")
	p.mu.Lock()
	p.AutoModeLastFiredMs = time.Now().UnixMilli()
	p.mu.Unlock()
	if p.computeState() == StateChoicePrompt {
		t.Fatal("menu still counted unanswered; the race needs computeState to see it answered")
	}
	if got := p.menuInputState(); got != menuInputAutoAnswered {
		t.Fatalf("menuInputState = %v, want auto-answered (dialog still on screen)", got)
	}
	// The person's key lands on the still-drawn dialog.
	if _, err := p.Write([]byte("2")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "person's key", "2")
	if pending := p.composerPendingLen(); pending != 0 {
		t.Fatalf("a key aimed at the dialog became a draft: %q", p.promptRec.buf)
	}
	if held, reason := autoFireUserHold(p, time.Now().Add(ariaAutoTypingSettle+time.Second)); held {
		t.Fatalf("next approval held: %s", reason)
	}
}

// A draft typed before the dialog keeps its protection and its age when a
// key reaches the dialog after an auto-answer.
func TestKeyAimedAtDialogAfterAutoAnswerKeepsEarlierDraft(t *testing.T) {
	p, frames := newTypingHoldPane(t, "auto-answered-keeps-draft")
	p.Cols, p.Rows = 160, 48
	if _, err := p.Write([]byte("half typed")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "draft", "half typed")
	_, draftAt := p.composerPendingSnapshot()
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	if _, err := p.WriteNoPromptRecord([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "auto-approval Enter", "\r")
	p.MarkAutoModeFired()
	time.Sleep(5 * time.Millisecond)
	if _, err := p.Write([]byte("2")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "person's key", "2")
	if n, at := p.composerPendingSnapshot(); n != len("half typed") || at != draftAt {
		t.Fatalf("draft = %d bytes at %d, want %d bytes at %d", n, at, len("half typed"), draftAt)
	}
}

// newClaudeComposerPane is a Claude terminal pane whose recorder holds a
// draft last touched well past the settle window.
func newClaudeComposerPane(t *testing.T, id, draft string, screen string) *Pane {
	t.Helper()
	p, _ := newTypingHoldPane(t, id)
	p.Agent, p.forceCodexForTest = agentClaude, false
	p.Cols, p.Rows = 160, 48
	seedGridMenu(t, p, screen)
	p.promptRec.buf = []byte(draft)
	p.promptRec.lastRecordedInputMs = time.Now().Add(-30 * time.Second).UnixMilli()
	return p
}

// The hold relied on the recorder's guess, not what the screen showed. When
// Claude's input box is visible and empty, a recorder draft is a phantom:
// the attention tick drops it and the next approval is not held.
func TestReconcileDropsDraftTheScreenShowsIsGone(t *testing.T) {
	p := newClaudeComposerPane(t, "phantom-draft", "2", claude2110Screen(t, "esc_sent"))
	if held, _ := autoFireUserHold(p, time.Now()); !held {
		t.Fatal("fixture: the phantom draft should hold before reconciling")
	}
	if !p.reconcileComposerDraft(time.Now()) {
		t.Fatal("empty input box on screen did not drop the phantom draft")
	}
	if held, reason := autoFireUserHold(p, time.Now()); held {
		t.Fatalf("still held after reconcile: %s", reason)
	}
	// The attention tick runs it.
	p.promptRec.buf = []byte("2")
	e := newAttentionEngine(nil)
	e.Refresh(p)
	if pending := p.composerPendingLen(); pending != 0 {
		t.Fatalf("attention refresh kept the phantom draft: %q", p.promptRec.buf)
	}
}

// Anything short of proof keeps the draft: text in the box (or the
// "[Pasted text]" placeholder), a menu over the box, a keystroke too recent
// to have been echoed, or a pane that is not an agent.
func TestReconcileKeepsDraftWithoutProof(t *testing.T) {
	for name, tc := range map[string]struct {
		screen string
		setup  func(*Pane)
	}{
		"text in the box":    {screen: claude2110Screen(t, "working_draft")},
		"paste placeholder":  {screen: strings.Replace(claude2110Screen(t, "esc_sent"), "❯\u00a0\n", "❯\u00a0[Pasted text #7 +3 lines]\n", 1)},
		"menu over the box":  {screen: claudeAskUserQuestionWrappedScreen},
		"unrecognised box":   {screen: "$ "},
		"keystroke too new":  {screen: claude2110Screen(t, "esc_sent"), setup: func(p *Pane) { p.promptRec.lastRecordedInputMs = time.Now().UnixMilli() }},
		"not an agent pane":  {screen: claude2110Screen(t, "esc_sent"), setup: func(p *Pane) { p.Agent = "" }},
		"native agent pane":  {screen: claude2110Screen(t, "esc_sent"), setup: func(p *Pane) { p.Kind = "native" }},
		"inside a menu step": {screen: claude2110Screen(t, "esc_sent"), setup: func(p *Pane) { p.promptRec.enterMenu() }},
		// The last glyph row is a continuation of a multi-line draft: no rule
		// directly above it, so the box is not provably one empty row.
		"multi-line draft": {screen: strings.Replace(claude2110Screen(t, "esc_sent"), "❯\u00a0\n", "❯ first line\n❯\u00a0\n", 1)},
		"codex pane":       {screen: claude2110Screen(t, "esc_sent"), setup: func(p *Pane) { p.Agent = agentCodex }},
		"dialog footer":    {screen: claude2110Screen(t, "esc_sent") + "\n  Enter to confirm · Esc to cancel"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newClaudeComposerPane(t, "keep-"+strings.ReplaceAll(name, " ", "-"), "half typed", tc.screen)
			if tc.setup != nil {
				tc.setup(p)
			}
			if p.reconcileComposerDraft(time.Now()) {
				t.Fatal("draft dropped without proof the box is empty")
			}
			if string(p.promptRec.buf) != "half typed" {
				t.Fatalf("draft changed: %q", p.promptRec.buf)
			}
		})
	}
}

// A box that visibly holds text is re-read at most once per recheck interval.
func TestReconcileThrottlesWhileTheBoxHoldsText(t *testing.T) {
	p := newClaudeComposerPane(t, "throttle", "half typed", claude2110Screen(t, "working_draft"))
	now := time.Now()
	p.reconcileComposerDraft(now)
	if after := atomic.LoadInt64(&p.composerReconcileAfterMs); after != now.Add(composerReconcileRecheck).UnixMilli() {
		t.Fatalf("recheck not scheduled: %d", after)
	}
	seedGridMenu(t, p, claude2110Screen(t, "esc_sent"))
	if p.reconcileComposerDraft(now.Add(time.Second)) {
		t.Fatal("screen re-read inside the recheck interval")
	}
	if !p.reconcileComposerDraft(now.Add(composerReconcileRecheck + time.Second)) {
		t.Fatal("phantom kept after the recheck interval")
	}
}

// The hold still works for a real draft: auto-approval does not renew it
// (its Enter is unrecorded), so it lifts ariaAutoDraftStaleAfter after the
// last keystroke, not after the last fire.
func TestAutoApprovalFireDoesNotRenewDraft(t *testing.T) {
	p, frames := newTypingHoldPane(t, "fire-no-renew")
	a := testAriaWithPane(t, p)
	a.sweepStreaks = map[string]*sweepStreak{}
	a.evalLog = newAriaEvalLog()
	if _, err := p.Write([]byte("half typed")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "draft", "half typed")
	// Age the keystroke past both windows, then let a fire land.
	old := time.Now().Add(-ariaAutoDraftStaleAfter - time.Second).UnixMilli()
	p.agentWriteMu.Lock()
	p.promptRec.lastRecordedInputMs = old
	p.agentWriteMu.Unlock()
	atomic.StoreInt64(&p.LastInputMs, old)
	seedGridMenu(t, p, codexNarrowCommandMenu("git push"))
	a.directAutoFire(context.Background(), p.ID, p.Title)
	expectFrame(t, frames, "stale draft does not hold", "\r")
	if _, at := p.composerPendingSnapshot(); at != old {
		t.Fatalf("auto-approval renewed the draft: recorded %d, want %d", at, old)
	}
	seedGridMenu(t, p, codexNarrowCommandMenu("git pull"))
	if held, reason := autoFireUserHold(p, time.Now()); held && strings.HasPrefix(reason, "composer has text") {
		t.Fatalf("next prompt held by a renewed draft: %s", reason)
	}
}

// A footer phrase alone (assistant output quoting "enter to confirm") does
// not move typing out of the draft; only a fresh auto-answer does.
func TestFooterPhraseWithoutAutoAnswerKeepsTypingAsDraft(t *testing.T) {
	p, frames := newTypingHoldPane(t, "footer-phrase-output")
	p.Cols, p.Rows = 160, 48
	seedGridMenu(t, p, "● Run the installer and press Enter to confirm.\n\n❯ \n  ? for shortcuts")
	if got := p.menuInputState(); got != menuInputDrawn {
		t.Fatalf("fixture: menuInputState = %v, want drawn", got)
	}
	if _, err := p.Write([]byte("real draft")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "composer typing", "real draft")
	if got := string(p.promptRec.buf); got != "real draft" || p.composerPendingLen() != len(got) {
		t.Fatalf("typing under a footer phrase left the draft: buf=%q pending=%d", got, p.composerPendingLen())
	}
	// An auto-answer older than the settle window does not count either.
	p.mu.Lock()
	p.AutoModeLastFiredMs = time.Now().Add(-ariaAutoTypingSettle - time.Second).UnixMilli()
	p.mu.Unlock()
	if got := p.menuInputState(); got != menuInputDrawn {
		t.Fatalf("stale auto-answer: menuInputState = %v, want drawn", got)
	}
	// Nor does a fresh one once the dialog has closed: the phrase is output
	// above the input box (review round 2).
	p.MarkAutoModeFired()
	if got := p.menuInputState(); got != menuInputDrawn {
		t.Fatalf("fresh auto-answer, dialog closed: menuInputState = %v, want drawn", got)
	}
	if _, err := p.Write([]byte(" more")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "composer typing after an auto-answer", " more")
	if got := string(p.promptRec.buf); got != "real draft more" || p.composerPendingLen() != len(got) {
		t.Fatalf("typing after an auto-answer left the draft: buf=%q pending=%d", got, p.composerPendingLen())
	}
}

func TestMenuDialogFooterBelowLastPrompt(t *testing.T) {
	for name, tc := range map[string]struct {
		screen string
		want   bool
	}{
		"question dialog":       {claudeAskUserQuestionWrappedScreen, true},
		"footer-only dialog":    {claudeAskUserQuestionFooterOnlyScreen, true},
		"codex command menu":    {codexNarrowCommandMenu("git push"), true},
		"phrase above the box":  {"● Press Enter to confirm.\n\n❯ \n  ? for shortcuts", false},
		"phrase with no prompt": {"Enter to confirm · Esc to cancel", false},
	} {
		if got := menuDialogFooterBelowLastPrompt(tc.screen); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestClaudeComposerProvablyEmpty(t *testing.T) {
	rule := strings.Repeat("─", 40)
	for name, tc := range map[string]struct {
		screen       string
		empty, known bool
	}{
		"empty box":          {rule + "\n❯\u00a0\n" + rule + "\n  ? for shortcuts", true, true},
		"text in box":        {rule + "\n❯ fix it\n" + rule, false, true},
		"multi-line glyph":   {rule + "\n❯ fix it\n❯ \n" + rule, false, false},
		"no bottom rule":     {rule + "\n❯ \n  ? for shortcuts", false, false},
		"transcript only":    {"❯ earlier prompt\n● reply", false, false},
		"short dash run":     {"──\n❯ \n──", false, false},
		"busy fixture":       {claude2110Screen(t, "esc_sent"), true, true},
		"draft fixture":      {claude2110Screen(t, "working_draft"), false, true},
		"placeholder (idle)": {claude2110Screen(t, "idle"), false, true},
	} {
		empty, known := claudeComposerProvablyEmpty(tc.screen)
		if empty != tc.empty || known != tc.known {
			t.Errorf("%s: empty=%v known=%v, want %v %v", name, empty, known, tc.empty, tc.known)
		}
	}
}
